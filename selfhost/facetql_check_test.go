package selfhost

// page.fct / pager.fct against the real facetql storage engine.
//
// Two directions, each checked:
//
//   - Rust -> fct: testdata/page_golden.tsv holds, per page script, the
//     SHA-256 of the summary facetql's own Page produced (counters, cells
//     and the encoded body, CRC included) — the port must produce the same
//     summary byte for byte. testdata/pages_rust.db is a paged file the
//     real Pager wrote (three heap pages under facetqlCheckKey); the port's
//     pager must read every page back.
//   - fct -> Rust: when cargo is available, testdata/facetql_check (a
//     small binary that links facetql as a library and never modifies it)
//     decrypts and decodes pages the port sealed, reads a whole paged file
//     the port's pager wrote, and decodes WAL frames the port appended.
//     Without cargo those subtests skip; set FCT_FACETQL_CHECK_TARGET to
//     choose where it builds (default: the OS temp dir).
//
// FCT_REGEN_PAGE_GOLDEN=1 rewrites both golden files from the Rust side.

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// facetqlCheckKey: a non-zero AES-256 key, so the checks exercise a real
// key rather than the development one.
const facetqlCheckKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func loadPagerApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("pager.fct")
	if err != nil {
		t.Fatalf("compile selfhost/pager.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var pagerLast = map[*httptest.Server]string{}

func pagerCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["pagerOut"].(string); ok {
		pagerLast[ts] = v
	}
	return pagerLast[ts]
}

var (
	facetqlCheckOnce sync.Once
	facetqlCheckBin  string
	facetqlCheckErr  string
)

// facetqlCheck builds testdata/facetql_check once and returns its path, or
// skips the calling test when cargo (or the facetql checkout) is absent.
func facetqlCheck(t *testing.T) string {
	t.Helper()
	facetqlCheckOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			facetqlCheckErr = "cargo is not installed"
			return
		}
		if _, err := os.Stat("../../facetql/Cargo.toml"); err != nil {
			facetqlCheckErr = "no facetql checkout beside fct"
			return
		}
		target := os.Getenv("FCT_FACETQL_CHECK_TARGET")
		if target == "" {
			target = filepath.Join(os.TempDir(), "fct-facetql-check")
		}
		cmd := exec.Command(cargo, "build", "--release", "--quiet")
		cmd.Dir = "testdata/facetql_check"
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			facetqlCheckErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		facetqlCheckBin = filepath.Join(target, "release", "facetql_check")
	})
	if facetqlCheckBin == "" {
		if strings.HasPrefix(facetqlCheckErr, "cargo build failed") {
			t.Fatal(facetqlCheckErr)
		}
		t.Skip("facetql_check unavailable: " + facetqlCheckErr)
	}
	return facetqlCheckBin
}

// runCheck runs the Rust checker in `mode`, one input per stdin line.
func runCheck(t *testing.T, stdin []string, args ...string) []string {
	t.Helper()
	return runCheckEnv(t, nil, stdin, args...)
}

func runCheckEnv(t *testing.T, env []string, stdin []string, args ...string) []string {
	t.Helper()
	bin := facetqlCheck(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(append(os.Environ(), "FACETQL_MASTER_KEY="+facetqlCheckKey), env...)
	cmd.Stdin = strings.NewReader(strings.Join(stdin, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("facetql_check %v: %v\n%s", args, err, out)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

func cellHex(n int, fill byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill + byte(i)
	}
	return hex.EncodeToString(b)
}

// pageScripts: every page operation, the compaction path (a replace that
// grows a cell, inserts into a fragmented page), a full page, and each
// page kind.
func pageScripts() [][2]string {
	var fill []string
	fill = append(fill, "new 2")
	for i := 0; i < 17; i++ {
		fill = append(fill, "push "+cellHex(960, byte(i)))
	}
	return [][2]string{
		{"empty-heap", "new 4"},
		{"ops", "new 2;push 0102;push aabbcc;insert 0 ff;replace 1 0102030405;remove 0;extra 7"},
		{"branch", "new 3;extra 4294967295;push 00;push " + cellHex(300, 9) + ";insert 1 77"},
		{"fragmented", "new 4;push " + cellHex(7000, 1) + ";push " + cellHex(7000, 2) + ";replace 0 " + cellHex(10, 3) + ";insert 1 " + cellHex(8000, 4) + ";compact"},
		{"full", strings.Join(fill, ";")},
		{"meta-overflow", "new 1;push 01;new 5;push " + cellHex(16328, 5) + ";push 00"},
	}
}

func pageGolden(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("testdata/page_golden.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestPageScriptsMatchRust(t *testing.T) {
	if os.Getenv("FCT_REGEN_PAGE_GOLDEN") == "1" {
		var in []string
		for _, s := range pageScripts() {
			in = append(in, s[1])
		}
		outs := runCheck(t, in, "page-script")
		var lines []string
		for i, s := range pageScripts() {
			lines = append(lines, s[0]+"\t"+sum(strings.ReplaceAll(outs[i], "|", "\n")))
		}
		if err := os.WriteFile("testdata/page_golden.tsv", []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := pageGolden(t)
	ts := loadPagerApp(t, t.TempDir())
	for _, s := range pageScripts() {
		s := s
		t.Run(s[0], func(t *testing.T) {
			got := pagerCall(t, ts, "runPageScript", strings.ReplaceAll(s[1], ";", "\n"))
			want, ok := golden[s[0]]
			if !ok {
				t.Fatalf("no golden line for %s", s[0])
			}
			if sum(got) != want {
				head := got
				if i := strings.Index(head, "\nbody="); i >= 0 {
					head = head[:i]
				}
				t.Fatalf("page summary differs from facetql's (sha256 %s, want %s):\n%s", sum(got), want, head)
			}
		})
	}
}

// pageBody: the encoded body the port produced for a script.
func pageBody(t *testing.T, ts *httptest.Server, script string) []byte {
	t.Helper()
	got := pagerCall(t, ts, "runPageScript", script)
	i := strings.Index(got, "\nbody=")
	b, err := hex.DecodeString(got[i+len("\nbody="):])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withCRC(b []byte) []byte {
	out := append([]byte{}, b...)
	h := crc32.NewIEEE()
	h.Write(out[:16])
	h.Write(out[20:])
	binary.LittleEndian.PutUint32(out[16:], h.Sum32())
	return out
}

func TestPageDecodeRefusals(t *testing.T) {
	ts := loadPagerApp(t, t.TempDir())
	body := pageBody(t, ts, "new 2\npush 0102\npush aabbcc")
	mut := func(f func(b []byte)) string {
		b := append([]byte{}, body...)
		f(b)
		return hex.EncodeToString(b)
	}
	cases := []struct{ name, hex, want string }{
		{"valid", hex.EncodeToString(body), "kind=2 count=2 cells=0102,aabbcc"},
		{"length", hex.EncodeToString(body[:100]), "error: page body is 100 bytes, expected 16356"},
		{"magic", mut(func(b []byte) { b[0] = 'X' }), "error: page magic mismatch — not a FacetQL page"},
		{"version", mut(func(b []byte) { b[5] = 9 }), "error: page format version 9 is not supported by this build (expected 1)"},
		{"kind", mut(func(b []byte) { b[4] = 9 }), "error: unknown page kind 9"},
		{"checksum", mut(func(b []byte) { b[16300] ^= 1 }), "error: page checksum mismatch — the page is corrupt"},
		{"overlap", hex.EncodeToString(withCRC(func() []byte { b := append([]byte{}, body...); binary.LittleEndian.PutUint16(b[8:], 20); return b }())), "error: page directory/cell areas overlap: 2 slots, free_end 20"},
		{"slot", hex.EncodeToString(withCRC(func() []byte { b := append([]byte{}, body...); binary.LittleEndian.PutUint16(b[26:], 900); return b }())), "error: page slot 0 points outside the cell area (offset 16354, length 900)"},
	}
	for _, c := range cases {
		if got := pagerCall(t, ts, "runPageDecode", c.hex); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPagerSemantics: the buffer pool's own contract, no Rust needed —
// LRU eviction with write-back, a page past the end, a torn extension, a
// wrong key.
func TestPagerSemantics(t *testing.T) {
	dir := t.TempDir()
	ts := loadPagerApp(t, dir)
	got := pagerCall(t, ts, "runPager", "p.db", facetqlCheckKey, 7, 2, "write 0 aa\nwrite 1 bb\nread 0\nwrite 2 cc\nread 5\nflush")
	// Capacity 2: writing page 2 evicts page 1 (page 0 was just read), so
	// page 1 reaches the disk on eviction and pages 0 and 2 on flush.
	want := "open 0;write 0;[0] write 1;[0,1] read 0=aa;[0,1] write 2;[0,2] read 5 error: page 5 is past the end of p.db (3 pages);[0,2] flush;[0,2] pages=3"
	if got != want {
		t.Fatalf("session:\n got  %s\n want %s", got, want)
	}
	st, err := os.Stat(filepath.Join(dir, "p.db"))
	if err != nil || st.Size() != 3*16384 {
		t.Fatalf("paged file: %v size %d, want %d", err, st.Size(), 3*16384)
	}
	// A torn extension (a partial trailing page) rounds the count down.
	f, _ := os.OpenFile(filepath.Join(dir, "p.db"), os.O_APPEND|os.O_WRONLY, 0)
	f.Write(make([]byte, 100))
	f.Close()
	if got := pagerCall(t, ts, "runPager", "p.db", facetqlCheckKey, 8, 2, "read 1"); got != "open 3;read 1=bb;[1] pages=3" {
		t.Fatalf("reopen after a torn extension: %s", got)
	}
	// Under another key the GCM tag refuses the page.
	other := strings.Repeat("22", 32)
	if got := pagerCall(t, ts, "runPager", "p.db", other, 9, 2, "read 2"); !strings.Contains(got, "read 2 error: failed to decrypt page 2 of p.db: decryption failed — wrong FACETQL_MASTER_KEY, or data is corrupted/tampered — wrong FACETQL_MASTER_KEY, or the page is damaged") {
		t.Fatalf("wrong key: %s", got)
	}
}

// TestPagerReadsRustFile: the real Pager's file (testdata/pages_rust.db),
// every page decrypted and decoded by the port.
func TestPagerReadsRustFile(t *testing.T) {
	const ops = "write 0 " + "0a0b0c;write 1 ff;write 2 "
	rustOps := ops + cellHex(40, 1)
	if os.Getenv("FCT_REGEN_PAGE_GOLDEN") == "1" {
		tmp := filepath.Join(t.TempDir(), "r.db")
		runCheck(t, nil, "pager-write", tmp, rustOps)
		b, err := os.ReadFile(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/pages_rust.db", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	b, err := os.ReadFile("testdata/pages_rust.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r.db"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	ts := loadPagerApp(t, dir)
	got := pagerCall(t, ts, "runPager", "r.db", facetqlCheckKey, 3, 8, "read 0\nread 1\nread 2")
	want := "open 3;read 0=0a0b0c;[0] read 1=ff;[0,1] read 2=" + cellHex(40, 1) + ";[0,1,2] pages=3"
	if got != want {
		t.Fatalf("reading facetql's paged file:\n got  %s\n want %s", got, want)
	}
}

// TestRustReadsFctPages: the reverse direction — facetql's own Pager and
// Page::decode read what the port wrote, and its WAL decoder reads the
// port's frames.
func TestRustReadsFctPages(t *testing.T) {
	facetqlCheck(t)
	dir := t.TempDir()
	ts := loadPagerApp(t, dir)
	got := pagerCall(t, ts, "runPager", "f.db", facetqlCheckKey, 11, 2, "write 0 0102\nwrite 1 "+cellHex(500, 7)+"\nwrite 2 cafe\nflush")
	if !strings.HasSuffix(got, "pages=3") {
		t.Fatalf("port session: %s", got)
	}
	out := runCheck(t, nil, "pager-read", filepath.Join(dir, "f.db"))
	want := "pages=3;read 0=0102;read 1=" + cellHex(500, 7) + ";read 2=cafe;"
	if out[0] != want {
		t.Fatalf("facetql reading the port's paged file:\n got  %s\n want %s", out[0], want)
	}

	// A sealed page, and Rust's own seal opened by the port.
	body := pageBody(t, ts, "new 3\npush 00\nextra 12")
	blob := pagerCall(t, ts, "runPageSeal", facetqlCheckKey, "0000000100000000000000aa", hex.EncodeToString(body))
	if out := runCheck(t, []string{blob}, "open-page"); out[0] != "kind=3 count=1 cells=00" {
		t.Fatalf("facetql opening the port's sealed page: %s", out[0])
	}
	rustBlob := runCheck(t, []string{hex.EncodeToString(body)}, "seal-page")[0]
	if got := pagerCall(t, ts, "runPageOpen", facetqlCheckKey, rustBlob); got != "kind=3 count=1 cells=00" {
		t.Fatalf("port opening facetql's sealed page: %s", got)
	}

	// Every page script's summary, live.
	var in []string
	for _, s := range pageScripts() {
		in = append(in, s[1])
	}
	outs := runCheck(t, in, "page-script")
	golden := pageGolden(t)
	for i, s := range pageScripts() {
		if sum(strings.ReplaceAll(outs[i], "|", "\n")) != golden[s[0]] {
			t.Errorf("%s: facetql's own summary no longer matches testdata/page_golden.tsv — regenerate it (FCT_REGEN_PAGE_GOLDEN=1) and re-check the port", s[0])
		}
	}

	// WAL frames the port appended, decoded by facetql's decode_frame.
	wdir := t.TempDir()
	wts := loadWalApp(t, wdir)
	if got := walCall(t, wts, "runWalSession", "facetql.wal", facetqlCheckKey, 3, walWorkload); got != "next=15/4/15" {
		t.Fatalf("wal session: %s", got)
	}
	raw, err := os.ReadFile(filepath.Join(wdir, "facetql.wal"))
	if err != nil {
		t.Fatal(err)
	}
	frames := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	decoded := runCheck(t, frames, "wal-decode")
	if len(decoded) != len(frames) {
		t.Fatalf("facetql decoded %d of %d frames", len(decoded), len(frames))
	}
	for i, d := range decoded {
		if !strings.HasPrefix(d, "durable ") {
			t.Fatalf("frame %d: facetql says %s", i, d)
		}
		mine := strings.SplitN(walCall(t, wts, "runWalGolden", facetqlCheckKey, frames[i]), "|", 3)
		if mine[0] != "durable" || mine[1] != strings.TrimPrefix(d, "durable ") || mine[2] != frames[i] {
			t.Fatalf("frame %d: port decodes %.200q, facetql %.200q", i, mine, d)
		}
	}
}

func loadCkptApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("checkpoint.fct")
	if err != nil {
		t.Fatalf("compile selfhost/checkpoint.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var ckptLast = map[*httptest.Server]string{}

func ckptCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["ckptOut"].(string); ok {
		ckptLast[ts] = v
	}
	return ckptLast[ts]
}

const ckptRefuse = ". Refusing to guess: a checkpoint that reads too high silently skips WAL recovery of durable records. Restore the file from a backup, or delete it to replay the whole WAL (safe, but re-applies non-idempotent archive/edge operations)."

// TestCheckpointContract: checkpoint.fct reads fail closed exactly where
// checkpoint.rs's do, and advance honours monotonicity and the fences —
// checked against the literal contract, and against facetql itself when
// cargo is available.
func TestCheckpointContract(t *testing.T) {
	dir := t.TempDir()
	ts := loadCkptApp(t, dir)
	const name = "facetql.checkpoint"
	path := filepath.Join(dir, name)
	cases := []struct {
		name    string
		content []byte // nil: no file
		want    string
	}{
		{"missing", nil, "value=0"},
		{"empty", []byte{}, "value=0 warning: facetql.checkpoint is zero-length — treating the checkpoint as 0 and replaying the WAL from the beginning. This is what an interrupted checkpoint write leaves behind; it is safe (replay is redundant, never lossy) but on a non-empty data directory it means a checkpoint update did not complete."},
		{"plain", []byte("123"), "value=123"},
		{"padded", []byte(" 0042\n"), "value=42"},
		{"letters", []byte("12ab"), "error: corrupt checkpoint file facetql.checkpoint: content is not a plain decimal integer: \"12ab\"" + ckptRefuse},
		{"sign", []byte("-5\t"), "error: corrupt checkpoint file facetql.checkpoint: content is not a plain decimal integer: \"-5\\t\"" + ckptRefuse},
		{"whitespace", []byte(" \n "), "error: corrupt checkpoint file facetql.checkpoint: content is whitespace only: \" \\n \" — this is not an interrupted create (that leaves a zero-length file), so the previous checkpoint value has been overwritten by something that is not a number" + ckptRefuse},
		{"long", []byte(strings.Repeat("1", 70)), "error: corrupt checkpoint file facetql.checkpoint: file is 70 bytes; a checkpoint is a single ASCII u64 and cannot exceed 64 bytes" + ckptRefuse},
		{"utf8", []byte{'1', 0xff, '2'}, "error: corrupt checkpoint file facetql.checkpoint: content is not valid UTF-8 (invalid utf-8 sequence of 1 bytes from index 1): \"1\ufffd2\"" + ckptRefuse},
		{"utf8-cut", []byte{'7', 0xe2, 0x82}, "error: corrupt checkpoint file facetql.checkpoint: content is not valid UTF-8 (incomplete utf-8 byte sequence from index 1): \"7\ufffd\"" + ckptRefuse},
		{"control", []byte("1\x01"), "error: corrupt checkpoint file facetql.checkpoint: content is not a plain decimal integer: \"1\\u{1}\"" + ckptRefuse},
		{"overflow", []byte("99999999999999999999"), "error: corrupt checkpoint file facetql.checkpoint: content does not fit in a u64 (number too large to fit in target type): \"99999999999999999999\"" + ckptRefuse},
	}
	haveRust := true
	rustHome := t.TempDir()
	rustPath := filepath.Join(rustHome, ".facetql", name)
	os.MkdirAll(filepath.Dir(rustPath), 0o755)
	if _, err := exec.LookPath("cargo"); err != nil {
		haveRust = false
	}
	for _, c := range cases {
		os.Remove(path)
		os.Remove(rustPath)
		if c.content != nil {
			os.WriteFile(path, c.content, 0o644)
			os.WriteFile(rustPath, c.content, 0o644)
		}
		got := ckptCall(t, ts, "runCkptRead", name)
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
		if haveRust {
			out := runCheckEnv(t, []string{"HOME=" + rustHome}, nil, "ckpt-read")
			// the warning is facetql's stderr, printed before the value
			if len(out) == 2 && strings.HasPrefix(out[0], "warning: ") {
				out = []string{out[1], out[0]}
			}
			rust := strings.ReplaceAll(strings.Join(out, " "), rustPath, name)
			if rust != c.want {
				t.Errorf("%s: facetql says\n %s\nthe contract here says\n %s", c.name, rust, c.want)
			}
		}
	}

	os.Remove(path)
	os.Remove(rustPath)
	script := "advance 5;advance 3;fence 8;advance 20;fence 12;release 8;advance 30;release 12;advance 30"
	want := "advance 5=5 written;advance 3=5;fence 8;advance 20=7 written;fence 12;release 8;advance 30=11 written;release 12;advance 30=30 written;"
	if got := ckptCall(t, ts, "runCkptScript", "", strings.ReplaceAll(script, ";", "\n")); got != want {
		t.Fatalf("advance:\n got  %s\n want %s", got, want)
	}
	if b, _ := os.ReadFile(path); string(b) != "30" {
		t.Fatalf("checkpoint file holds %q, want \"30\"", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("data directory holds %d entries after advancing, want only the checkpoint (no temp files left)", len(entries))
	}
	if haveRust {
		out := runCheckEnv(t, []string{"HOME=" + rustHome}, nil, "ckpt-script", script)
		if out[0] != want {
			t.Fatalf("facetql advance:\n got  %s\n want %s", out[0], want)
		}
	}
}

func loadBinaryApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("binary.fct")
	if err != nil {
		t.Fatalf("compile selfhost/binary.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var binaryLast = map[*httptest.Server]string{}

func binaryCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["binaryOut"].(string); ok {
		binaryLast[ts] = v
	}
	return binaryLast[ts]
}

// TestRecordFrame: binary.rs's frame, built independently here (magic,
// version 2, u32 length, CRC-32 IEEE, payload) and every decode refusal.
func TestRecordFrame(t *testing.T) {
	ts := loadBinaryApp(t, t.TempDir())
	payload := []byte("hello, frame \x00\xff")
	want := append([]byte("FQR1\x02"), make([]byte, 8)...)
	binary.LittleEndian.PutUint32(want[5:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(want[9:], crc32.ChecksumIEEE(payload))
	want = append(want, payload...)
	if got := binaryCall(t, ts, "runFrame", hex.EncodeToString(payload)); got != hex.EncodeToString(want) {
		t.Fatalf("frame:\n got  %s\n want %s", got, hex.EncodeToString(want))
	}
	mut := func(f func([]byte) []byte) string { return hex.EncodeToString(f(append([]byte{}, want...))) }
	cases := []struct{ name, frame, want string }{
		{"ok", hex.EncodeToString(want), "payload=" + hex.EncodeToString(payload)},
		{"short", "46515231", "error: record frame is 4 bytes, shorter than the 13-byte header"},
		{"magic", mut(func(b []byte) []byte { b[1] = 'X'; return b }), "error: record frame magic mismatch"},
		{"version", mut(func(b []byte) []byte { b[4] = 1; return b }), "error: record frame format version 1 is not supported by this build (expected 2)"},
		{"length", mut(func(b []byte) []byte { return b[:len(b)-2] }), "error: record frame declares a 15-byte payload but carries 13"},
		{"checksum", mut(func(b []byte) []byte { b[14] ^= 4; return b }), "error: record frame checksum mismatch — the record is corrupt"},
	}
	for _, c := range cases {
		if got := binaryCall(t, ts, "runUnframe", c.frame); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestUsersLog: the framed, sealed users log — appended by the port and
// read back, then torn, corrupted and version-bumped; and a log facetql
// itself appended (testdata/users_rust.log) read by the port. With cargo,
// facetql reads each of the port's files and must say the same.
func TestUsersLog(t *testing.T) {
	if os.Getenv("FCT_REGEN_PAGE_GOLDEN") == "1" {
		tmp := filepath.Join(t.TempDir(), "u.log")
		runCheck(t, nil, "users-append", tmp, "put h1 alice admin;put h2 bob user;revoke h1")
		b, err := os.ReadFile(tmp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/users_rust.log", b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	ts := loadBinaryApp(t, dir)
	_, cargoErr := exec.LookPath("cargo")
	check := func(name, file, want string) {
		t.Helper()
		if got := binaryCall(t, ts, "runUsersRead", file, facetqlCheckKey); got != want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, want)
		}
		if cargoErr == nil {
			full := filepath.Join(dir, file)
			rust := strings.ReplaceAll(runCheck(t, nil, "users-read", full)[0], full, file)
			if rust != want {
				t.Errorf("%s: facetql says\n %s\nthe port's contract says\n %s", name, rust, want)
			}
		}
	}
	got := binaryCall(t, ts, "runUsers", "u.log", facetqlCheckKey, 21, "put h1 alice admin\nput h2 bob user\nrevoke h1")
	if !strings.HasPrefix(got, "@0 @") || !strings.HasSuffix(got, "revoke h1;clean") {
		t.Fatalf("append: %s", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "u.log"))
	if err != nil {
		t.Fatal(err)
	}
	// Offsets: each frame is 13 + nonce(12) + bincode + tag(16).
	f0 := 13 + 12 + (4 + 8 + 2 + 8 + 5 + 4) + 16
	f1 := 13 + 12 + (4 + 8 + 2 + 8 + 3 + 4) + 16
	clean := "0:put h1 alice admin;" + itoa(f0) + ":put h2 bob user;" + itoa(f0+f1) + ":revoke h1;clean"
	check("clean", "u.log", clean)

	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("torn.log", raw[:len(raw)-5])
	check("torn payload", "torn.log", "0:put h1 alice admin;"+itoa(f0)+":put h2 bob user;torn@"+itoa(f0+f1)+" record payload at offset "+itoa(f0+f1)+" is truncated: header declares "+itoa(len(raw)-f0-f1-13)+" bytes but only "+itoa(len(raw)-f0-f1-13-5)+" remain")
	write("torn-header.log", raw[:f0+6])
	check("torn header", "torn-header.log", "0:put h1 alice admin;torn@"+itoa(f0)+" record header at offset "+itoa(f0)+" is truncated: 6 of 13 header bytes present")
	bad := append([]byte{}, raw...)
	bad[f0+20] ^= 1
	stored := binary.LittleEndian.Uint32(raw[f0+9:])
	computed := crc32.ChecksumIEEE(bad[f0+13 : f0+f1])
	write("crc.log", bad)
	check("checksum", "crc.log", "error: corrupt record in crc.log at offset "+itoa(f0)+": record checksum mismatch at offset "+itoa(f0)+": stored "+hex8(stored)+", computed "+hex8(computed)+" — payload corrupted")
	magic := append([]byte{}, raw...)
	magic[f0] = 'X'
	write("magic.log", magic)
	check("magic", "magic.log", "error: corrupt record in magic.log at offset "+itoa(f0)+": bad record magic at offset "+itoa(f0)+": expected [70, 81, 82, 49], found [88, 81, 82, 49]")
	ver := append([]byte{}, raw...)
	ver[f0+4] = 1
	write("ver.log", ver)
	check("version", "ver.log", "error: ver.log holds a v1 record frame at offset "+itoa(f0)+", but this build reads and writes v2. The on-disk record format changed. There is no in-place upgrade for this: stop the server and recreate the data directory (or restore a backup taken with a matching build). Nothing here is corrupt — refusing to read is deliberate, because a v1 payload decoded as v2 would not fail, it would quietly mean the wrong thing.")

	rustLog, err := os.ReadFile("testdata/users_rust.log")
	if err != nil {
		t.Fatal(err)
	}
	write("rust.log", rustLog)
	check("facetql's log", "rust.log", clean)
}

func hex8(v uint32) string {
	const d = "0123456789abcdef"
	out := []byte("0x00000000")
	for i := 9; i >= 2; i-- {
		out[i] = d[v&15]
		v >>= 4
	}
	return string(out)
}

func loadCatalogApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("catalog.fct")
	if err != nil {
		t.Fatalf("compile selfhost/catalog.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var catalogLast = map[*httptest.Server]string{}

func catalogCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["catalogOut"].(string); ok {
		catalogLast[ts] = v
	}
	return catalogLast[ts]
}

// TestCatalogBothWays: catalog.fct and facetql's Catalog share one data
// directory — each reads what the other saved, and both refuse a damaged,
// foreign-key or other-generation catalog with the same words. Without
// cargo, the port is held to the same expected strings on its own.
func TestCatalogBothWays(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".facetql")
	os.MkdirAll(dir, 0o755)
	ts := loadCatalogApp(t, dir)
	_, cargoErr := exec.LookPath("cargo")
	rust := func(env []string, args ...string) string {
		return strings.Join(runCheckEnv(t, append([]string{"HOME=" + home}, env...), nil, args...), " ")
	}
	same := func(what, fct string, env []string, args ...string) {
		t.Helper()
		if cargoErr != nil {
			return
		}
		if r := rust(env, args...); r != fct {
			t.Fatalf("%s: facetql says\n %s\nthe port says\n %s", what, r, fct)
		}
	}

	fresh := catalogCall(t, ts, "runCatalogRead", "", facetqlCheckKey)
	if fresh != "v1 page=16384 next=1 active=0 segs=0:0:0" {
		t.Fatalf("fresh catalog: %s", fresh)
	}
	same("fresh", fresh, nil, "catalog-read")

	wrote := catalogCall(t, ts, "runCatalogWrite", "", facetqlCheckKey, 31, "segs 0:12:4096,1:3:0,4:7:123456789012 active 4 next 5")
	if wrote != "v1 page=16384 next=5 active=4 segs=0:12:4096,1:3:0,4:7:123456789012" {
		t.Fatalf("port write: %s", wrote)
	}
	if got := catalogCall(t, ts, "runCatalogRead", "", facetqlCheckKey); got != wrote {
		t.Fatalf("port reread: %s", got)
	}
	same("facetql reading the port's catalog", wrote, nil, "catalog-read")

	if cargoErr == nil {
		r := rust(nil, "catalog-write", "segs 2:9:77,3:1:0 active 3 next 4")
		if got := catalogCall(t, ts, "runCatalogRead", "", facetqlCheckKey); got != r {
			t.Fatalf("port reading facetql's catalog:\n got  %s\n want %s", got, r)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("data directory holds %d entries after saving, want only the catalog", len(entries))
	}

	// Refusals.
	other := strings.Repeat("22", 32)
	wrongKey := catalogCall(t, ts, "runCatalogRead", "", other)
	if wrongKey != "error: failed to decrypt the catalog: decryption failed — wrong FACETQL_MASTER_KEY, or data is corrupted/tampered — wrong FACETQL_MASTER_KEY, or the file is damaged" {
		t.Fatalf("wrong key: %s", wrongKey)
	}
	same("wrong key", wrongKey, []string{"FACETQL_MASTER_KEY=" + other}, "catalog-read")

	catalogCall(t, ts, "runCatalogWrite", "", facetqlCheckKey, 32, "version 2")
	gen := catalogCall(t, ts, "runCatalogRead", "", facetqlCheckKey)
	if gen != "error: database format version 2 is not supported by this build (expected 1). The physical layout changed; recreate the data directory." {
		t.Fatalf("other generation: %s", gen)
	}
	same("other generation", gen, nil, "catalog-read")

	path := filepath.Join(dir, "facetql.catalog")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, b[:10], 0o644)
	torn := catalogCall(t, ts, "runCatalogRead", "", facetqlCheckKey)
	if torn != "error: catalog is unreadable: record frame is 10 bytes, shorter than the 13-byte header" {
		t.Fatalf("torn catalog: %s", torn)
	}
	same("torn catalog", torn, nil, "catalog-read")
}

func loadFqBTreeApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqbtree.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqbtree.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var fqbtreeLast = map[*httptest.Server]string{}

func fqbtreeCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["btreeOut"].(string); ok {
		fqbtreeLast[ts] = v
	}
	return fqbtreeLast[ts]
}

// btreeWorkload: enough ~1 KiB values to split leaves and grow a branch
// root, then overwrites, removals (one emptying a leaf), point reads,
// seeks both ways and prefix scans both ways, across two commits.
func btreeWorkload() []string {
	var ops []string
	for i := 0; i < 40; i++ {
		ops = append(ops, fmt.Sprintf("put %08x %s", i*7%40, cellHex(900+i%5*37, byte(i))))
	}
	ops = append(ops, "commit")
	ops = append(ops, "put 00000003 "+cellHex(10, 9), "put 00000011 -")
	for i := 20; i < 30; i++ {
		ops = append(ops, fmt.Sprintf("del %08x", i))
	}
	ops = append(ops, "del 000000ff", "get 00000003", "get 00000014", "get 00000021",
		"seek ge 00000014", "seek gt 00000003", "seek le 00000014", "seek lt 00000000",
		"scan 000000", "scan 0000 rev", "commit")
	return ops
}

// fqbtreeEntries: the result line and the entries of the port's output,
// in the Rust checker's shape.
func fqbtreeShape(out string) string {
	lines := strings.Split(out, "\n")
	var entries []string
	count := ""
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "generation=") {
			count = l[strings.Index(l, "entries="):]
			count = strings.Fields(count)[0]
			continue
		}
		entries = append(entries, l)
	}
	return lines[0] + "|" + count + "|" + strings.Join(entries, "|")
}

// decryptedPages: every page of a paged file, opened under key.
func decryptedPages(t *testing.T, path, keyHex string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := hex.DecodeString(keyHex)
	blk, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blk)
	var out [][]byte
	for off := 0; off+16384 <= len(b); off += 16384 {
		pg := b[off : off+16384]
		body, err := gcm.Open(nil, pg[:12], pg[12:], nil)
		if err != nil {
			t.Fatalf("%s page %d does not open: %v", path, off/16384, err)
		}
		out = append(out, body)
	}
	return out
}

// TestFqBTreeBothWays: the same workload run by fqbtree.fct and by
// facetql's BTree gives the same answers and the same pages — every page
// of the two index files decrypts to the same body — and each reads (and
// keeps writing) the other's file.
func TestFqBTreeBothWays(t *testing.T) {
	dir := t.TempDir()
	ts := loadFqBTreeApp(t, dir)
	script := strings.Join(btreeWorkload(), "\n")
	mine := fqbtreeCall(t, ts, "runBTree", "f.idx", facetqlCheckKey, 41, script)
	if strings.Contains(mine, "error") {
		t.Fatalf("port: %s", mine)
	}
	// The port's answers, checked on their own first.
	if !strings.Contains(mine, "get=") || !strings.Contains(mine, "get none;") || !strings.Contains(mine, "del true;") || !strings.Contains(mine, "del false;") {
		t.Fatalf("port results: %.400s", mine)
	}
	if !strings.Contains(mine, "entries=30 ") {
		t.Fatalf("port entry count: %.600s", mine)
	}
	facetqlCheck(t)
	rustPath := filepath.Join(dir, "r.idx")
	rust := runCheck(t, nil, "btree-script", rustPath, strings.Join(btreeWorkload(), ";"))[0]
	if got := fqbtreeShape(mine); got != rust {
		t.Fatalf("results differ:\n port     %.600s\n facetql  %.600s", got, rust)
	}
	a, b := decryptedPages(t, filepath.Join(dir, "f.idx"), facetqlCheckKey), decryptedPages(t, rustPath, facetqlCheckKey)
	if len(a) != len(b) {
		t.Fatalf("port wrote %d pages, facetql %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("page %d differs between the port's index and facetql's", i)
		}
	}
	// Each continues the other's file.
	more := "put 000000aa 01;del 00000000;commit;scan -"
	rustOnMine := runCheck(t, nil, "btree-script", filepath.Join(dir, "f.idx"), more)[0]
	mineOnRust := fqbtreeCall(t, ts, "runBTree", "r.idx", facetqlCheckKey, 42, strings.ReplaceAll(more, ";", "\n"))
	if got := fqbtreeShape(mineOnRust); got != rustOnMine {
		t.Fatalf("continuing each other's index:\n port on facetql's  %.600s\n facetql on port's  %.600s", got, rustOnMine)
	}
}

func loadFqHeapApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqheap.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqheap.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var fqheapLast = map[*httptest.Server]string{}

func fqheapCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["heapOut"].(string); ok {
		fqheapLast[ts] = v
	}
	return fqheapLast[ts]
}

// heapWorkload: small records sharing a page, a record too big for one
// (an overflow chain behind a stub), records after it, reads of each kind
// and a missing slot, a sync, and reads again.
var heapWorkload = []string{
	"node Post:1 10", "edge Post:1 User:9 author", "history Post:1 3 20",
	"node Post:2 40000", "node Post:3 5",
	"read 0:0:0:0", "read 0:0:1:0", "read 0:0:2:0", "read 0:4:0:40000", "read 0:4:1:0", "read 0:0:9:0",
	"sync", "read 0:4:0:40000",
}

// TestFqHeapBothWays: the same workload through fqheap.fct and through
// facetql's RecordStore yields the same locations, records and catalog,
// the segment files decrypt to the same pages, and each reads the other's
// records.
func TestFqHeapBothWays(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".facetql")
	os.MkdirAll(dir, 0o755)
	mineDir := t.TempDir()
	ts := loadFqHeapApp(t, mineDir)
	mine := fqheapCall(t, ts, "runHeap", "", facetqlCheckKey, 51, strings.Join(heapWorkload, "\n"))
	want := "@0:0:0:91;@0:0:1:72;@0:0:2:131;@0:4:0:40081;@0:4:1:86;node Post:1 Post data=10 value=7;edge Post:1>User:9:author@alice;history Post:1 v3 t1700000000 data=20;node Post:2 Post data=40000 value=7;node Post:3 Post data=5 value=7;read error: no record at segment 0 page 0 slot 9;sync;node Post:2 Post data=40000 value=7;\nv1 page=16384 next=1 active=0 segs=0:5:0"
	if mine != want {
		t.Fatalf("port heap session:\n got  %s\n want %s", mine, want)
	}
	facetqlCheck(t)
	rust := strings.Join(runCheckEnv(t, []string{"HOME=" + home}, nil, "heap-script", strings.Join(heapWorkload, ";")), " ")
	if got := strings.Replace(mine, "\n", "|", 1); got != rust {
		t.Fatalf("heap sessions differ:\n port     %s\n facetql  %s", got, rust)
	}
	a := decryptedPages(t, filepath.Join(mineDir, "facetql.heap.000000.seg"), facetqlCheckKey)
	b := decryptedPages(t, filepath.Join(dir, "facetql.heap.000000.seg"), facetqlCheckKey)
	if len(a) != len(b) {
		t.Fatalf("port wrote %d heap pages, facetql %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("heap page %d differs between the port and facetql", i)
		}
	}
	reads := "read 0:0:1:0;read 0:4:0:40000;read 0:0:2:0;scan 0"
	// facetql's data directory is $HOME/.facetql: point it at a copy of the
	// port's directory laid out that way.
	home2 := t.TempDir()
	os.MkdirAll(filepath.Join(home2, ".facetql"), 0o755)
	for _, name := range []string{"facetql.catalog", "facetql.heap.000000.seg"} {
		bs, err := os.ReadFile(filepath.Join(mineDir, name))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(home2, ".facetql", name), bs, 0o644)
	}
	rustReads := strings.Join(runCheckEnv(t, []string{"HOME=" + home2}, nil, "heap-script", reads), " ")
	ts2 := loadFqHeapApp(t, dir)
	mineReads := fqheapCall(t, ts2, "runHeap", "", facetqlCheckKey, 52, strings.ReplaceAll(reads, ";", "\n"))
	if got := strings.Replace(mineReads, "\n", "|", 1); got != rustReads {
		t.Fatalf("reading each other's heap:\n port on facetql's  %s\n facetql on port's  %s", got, rustReads)
	}
	if !strings.Contains(rustReads, "@0:4:0:40081 node Post:2 Post data=40000 value=7;@0:4:1:86 node Post:3") {
		t.Fatalf("the segment scan did not report the overflow record by its stub: %s", rustReads)
	}
	// Retiring the segment: out of the catalog, its file gone — each side
	// over the other's directory.
	rustDrop := strings.Join(runCheckEnv(t, []string{"HOME=" + home2}, nil, "heap-script", "drop 0"), " ")
	mineDrop := fqheapCall(t, ts2, "runHeap", "", facetqlCheckKey, 53, "drop 0")
	if got := strings.Replace(mineDrop, "\n", "|", 1); got != rustDrop || got != "drop;|v1 page=16384 next=1 active=0 segs=" {
		t.Fatalf("dropping a segment:\n port     %s\n facetql  %s", got, rustDrop)
	}
	for _, f := range []string{filepath.Join(dir, "facetql.heap.000000.seg"), filepath.Join(home2, ".facetql", "facetql.heap.000000.seg")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s survived drop_segment (%v)", f, err)
		}
	}
}

// fqjsonCorpus: documents serde_json accepts and ones it refuses, chosen
// for where a JSON reader or writer can differ from it — number forms and
// their boundaries (u64/i64 limits, -0, ryu's switch to exponents, whole
// floats past 2^53 whose shortest digits are not their exact ones), string
// escapes (control characters, surrogate pairs, a lone surrogate, "/"),
// repeated and unsorted keys, nesting, and malformed input.
var fqjsonCorpus = []string{
	`null`, `true`, `false`, `0`, `-0`, `-0.0`, `0.0`, `1`, `-1`, `1.0`, `1.5`, `-2.25`,
	`18446744073709551615`, `18446744073709551616`, `-9223372036854775808`, `-9223372036854775809`,
	`1e2`, `1E2`, `1e+2`, `1e-2`, `1.5e300`, `1e400`, `-1e400`, `1e-400`, `123456789012345678`,
	`1e15`, `1e16`, `1e17`, `12345678901234567.0`, `0.1`, `0.00001`, `0.000001`, `1e-7`, `1.25e-7`,
	`4611686018427387904.0`, `9007199254740993.0`, `9.223372036854776e18`, `1.7976931348623157e308`,
	`5e-324`, `123.456`, `3.0e0`, `100000000000000000000.0`, `0.12345678901234567890123`,
	`1.00000000000000011102230246251565404236316680908203125`, `123456789012345678901234567890e-10`,
	`1e99999999999`, `0e99999999999`, `-1e-99999999999`, `1e308`, `1e309`, `123e-400`, `1e-310`,
	`2.2250738585072014e-308`, `4.9e-324`, `1.5E+10`, `0.3`, `2.675`, `-1.1e-5`, `99999999999999999999e-2`,
	`"a"`, `""`, `"\u0041\n\t\r\b\f\"\\\/"`, `"\u001f\u007f"`, `"é ☃ 😀"`, `"\ud83d\ude00"`, `"\ud83d"`, `"\ude00x"`,
	`[]`, `{}`, `[1,2,[3,{"b":1,"a":2}]]`, `{"z":1,"a":{"y":[true,null],"b":"x"},"m":1.0}`,
	`{"a":1,"a":2}`, ` { "k" : [ 1 , 2 ] } `, `{"a":1,}`, `[1,]`, `[01]`, `01`, `1.`, `.5`, `-`, `1e`,
	`"abc`, `{"a" 1}`, `[1 2]`, `nul`, `tru`, `"\x"`, "\"a\tb\"", `{"a":1} x`, `{1:2}`,
	strings.Repeat("[", 127) + strings.Repeat("]", 127), strings.Repeat("[", 128) + strings.Repeat("]", 128),
}

// TestFqJsonBothWays: fqjson.fct reads and writes back every corpus
// document exactly as serde_json does, and the ordered-index encoding of
// each value is facetql's.
func TestFqJsonBothWays(t *testing.T) {
	g, err := compile.File("fqjson.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqjson.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	d := postExprJSON(t, ts, "runJson", strings.Join(fqjsonCorpus, "\n"))
	mine := strings.Split(strings.TrimSuffix(d["jsonOut"].(string), "\n"), "\n")
	if len(mine) != len(fqjsonCorpus) {
		t.Fatalf("port answered %d documents of %d", len(mine), len(fqjsonCorpus))
	}
	if mine[0] != "null | 100000" || mine[4] != "-0.0 | 307fffffffffffffff0000" {
		t.Fatalf("port: %q, %q", mine[0], mine[4])
	}
	facetqlCheck(t)
	rust := runCheckEnv(t, nil, fqjsonCorpus, "json-canon")
	for i := range fqjsonCorpus {
		if mine[i] != rust[i] {
			t.Errorf("document %q:\n port     %s\n facetql  %s", fqjsonCorpus[i], mine[i], rust[i])
		}
	}
}

func loadFqIndexApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqindex.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqindex.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// indexWorkload: nodes of several kinds, ordered and inverted indexes
// declared over existing data (a backfill) and then maintained through
// updates that change the indexed value, the owner and the kind, edges
// inserted, re-inserted and removed, a delete, an index dropped, and
// enough rows to split every tree.
func indexWorkload() []string {
	w := []string{
		`put P1 Post alice {"title":"Hello World","n":3,"tags":["b","a"]}`,
		`put P2 Post bob {"title":"hello there","n":1.5}`,
		`put U1 User alice {"name":"Al"}`,
		`index byN Post n`, `text byTitle Post title`, `index byTags Post tags`,
		`edge P1 U1 author`, `edge P2 U1 author`,
	}
	for i := 0; i < 150; i++ {
		w = append(w, fmt.Sprintf(`put N%03d Post owner%d {"n":%d,"title":"note number %d","x":%d.25}`, i, i%7, 150-i, i, i))
	}
	w = append(w,
		`put P1 Post alice {"title":"Goodbye","n":-2}`,
		`put P2 Post carol {"n":null}`,
		`put U1 Tag alice {}`,
		`unedge P2 U1 author`, `edge P1 U1 author`,
		`del P2`, `del N010`, `put N011 Post owner4 {"title":"note number 11 again"}`,
		`drop byTags`, `checkpoint`,
	)
	return w
}

// fctIndexScript: the workload for fqindex.fct, each put/del preceded by
// the archive version and time facetql stamped on it.
func fctIndexScript(ops []string, rustOut string) string {
	results := strings.Split(strings.SplitN(rustOut, "|", 2)[0], ";")
	var out []string
	for i, op := range ops {
		if i < len(results) {
			if f := strings.Fields(results[i]); len(f) == 3 && f[0] == "ok" {
				out = append(out, "as "+strings.TrimPrefix(f[1], "v=")+" "+strings.TrimPrefix(f[2], "t="))
			}
		}
		out = append(out, op)
	}
	return strings.Join(out, ";")
}

func samePagesIn(t *testing.T, dirA, dirB string, names []string) {
	t.Helper()
	for _, name := range names {
		a := decryptedPages(t, filepath.Join(dirA, name), facetqlCheckKey)
		b := decryptedPages(t, filepath.Join(dirB, name), facetqlCheckKey)
		if len(a) != len(b) {
			t.Fatalf("%s: %d pages in %s, %d in %s", name, len(a), dirA, len(b), dirB)
		}
		for i := range a {
			if !bytes.Equal(a[i], b[i]) {
				t.Fatalf("%s page %d differs between %s and %s", name, i, dirA, dirB)
			}
		}
	}
}

func storageFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "facetql.idx.") || strings.HasPrefix(n, "facetql.heap.") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// TestFqIndexBothWays: the same writes through fqindex.fct's apply step
// and through facetql's StorageEngine leave every index tree and the heap
// page-identical (the same files, the dropped index's gone from both);
// then each side loads the other's directory — the definition logs
// replayed, the declared indexes reopened — and a second round of writes
// again leaves them identical.
func TestFqIndexBothWays(t *testing.T) {
	facetqlCheck(t)
	home := t.TempDir()
	rdir := filepath.Join(home, ".facetql")
	os.MkdirAll(rdir, 0o755)
	ops := indexWorkload()
	rust := strings.Join(runCheckEnv(t, []string{"HOME=" + home}, nil, "engine-script", strings.Join(ops, ";")), " ")
	if strings.Contains(rust, "error") {
		t.Fatalf("facetql refused the workload: %s", rust)
	}
	mdir := t.TempDir()
	ts := loadFqIndexApp(t, mdir)
	d := postExprJSON(t, ts, "runIndex", "", facetqlCheckKey, 61, fctIndexScript(ops, rust))
	mine, _ := d["indexOut"].(string)
	if mine != rust {
		t.Fatalf("apply sessions differ:\n port     %s\n facetql  %s", mine, rust)
	}
	files := storageFiles(t, rdir)
	if got := storageFiles(t, mdir); strings.Join(got, ",") != strings.Join(files, ",") {
		t.Fatalf("storage files differ:\n port     %v\n facetql  %v", got, files)
	}
	for _, want := range []string{"facetql.idx.data.byN", "facetql.idx.text.byTitle", "facetql.idx.primary", "facetql.idx.history"} {
		if !strings.Contains(strings.Join(files, ","), want) {
			t.Fatalf("no %s among %v", want, files)
		}
	}
	samePagesIn(t, mdir, rdir, files)

	// Each loads the other's directory.
	home2 := t.TempDir()
	r2 := filepath.Join(home2, ".facetql")
	os.MkdirAll(r2, 0o755)
	ents, _ := os.ReadDir(mdir)
	for _, e := range ents {
		bs, err := os.ReadFile(filepath.Join(mdir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(r2, e.Name()), bs, 0o644)
	}
	more := []string{
		`put P9 Post dave {"title":"Late Arrival","n":0.5,"tags":[1]}`,
		`put N020 Post owner1 {"n":"text now","title":"renamed"}`,
		`index byX Post x`, `drop byTitle`, `checkpoint`,
	}
	rust2 := strings.Join(runCheckEnv(t, []string{"HOME=" + home2}, nil, "engine-script", strings.Join(more, ";")), " ")
	if strings.Contains(rust2, "error") {
		t.Fatalf("facetql refused the second round over the port's directory: %s", rust2)
	}
	ts2 := loadFqIndexApp(t, rdir)
	d2 := postExprJSON(t, ts2, "runIndex", "", facetqlCheckKey, 62, fctIndexScript(more, rust2))
	mine2, _ := d2["indexOut"].(string)
	if mine2 != rust2 {
		t.Fatalf("second round differs:\n port on facetql's  %s\n facetql on port's  %s", mine2, rust2)
	}
	files2 := storageFiles(t, r2)
	if got := storageFiles(t, rdir); strings.Join(got, ",") != strings.Join(files2, ",") {
		t.Fatalf("storage files differ after the second round:\n port     %v\n facetql  %v", got, files2)
	}
	samePagesIn(t, rdir, r2, files2)
}

func loadFqEngineApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqengine.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqengine.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// engineWorkload: users granted and revoked, nodes written and rewritten
// (each rewrite an archive + insert transaction frame), a unique ordered
// index, a text index, edges, a declared cascade reference and a set-null
// one, deletes that cascade and clear through them — across several
// automatic checkpoints and WAL rotations — ending in a refused write and
// no final checkpoint, as if the process died there.
func engineWorkload() []string {
	w := []string{
		`user h1 alice admin`, `user h2 bob user`, `revoke h2`,
		`put P1 Post alice {"slug":"one","title":"First Post","n":1}`,
		`put P2 Post bob {"slug":"two","title":"Second","n":2}`,
		`index bySlug Post slug unique`, `text byTitle Post title`,
		`index byPost Comment post`, `index byLike Like post`,
		`ref onPost Comment post Post - cascade`, `ref likes Like post Post - set_null`,
	}
	for i := 0; i < 12; i++ {
		post := []string{"P1", "P2"}[i%2]
		w = append(w, fmt.Sprintf(`put C%02d Comment alice {"post":"%s","body":"comment %d"}`, i, post, i))
	}
	w = append(w,
		`put L1 Like carol {"post":"P1"}`, `put L2 Like carol {"post":"P2"}`,
		`edge C00 P1 on`, `edge C01 P2 on`, `edge P1 P2 links`,
		`put P1 Post alice {"slug":"one","title":"First Post, edited","n":10}`,
		`put C03 Comment dave {"post":"P2","body":"moved"}`,
		`unedge P1 P2 links`, `edge P1 P2 links`,
		`del P1`,
		`put P3 Post erin {"slug":"three","title":"Third","n":[1,{"b":2,"a":1}]}`,
		`del C05`, `drop byTitle`, `checkpoint`,
		`put P4 Post erin {"slug":"four","title":"Fourth"}`,
		`put P2 Post bob {"slug":"two","title":"Second, again","n":2.5}`,
		`put X1 Post mallory {"slug":"two"}`,
	)
	return w
}

// engineReads: point reads, a multi-get with and without a requester,
// histories, both edge directions, owner lists, and query's kind/owner/
// requester/limit/offset paths (and its refusal of a deep offset).
const engineReads = "get P2;get P1;get C03;multi P2,C03,P9,L1 alice;multi P2,C03,L2 -;history P1;history P2;history C03;" +
	"from C00;to P2;from P1;to C01;owner alice -;owner carol alice;owner carol -;query Comment - - 5 2;query - alice - 100 0;" +
	"query Post - bob 10 0;query - - - 3 1;query Comment - - 0 0;query Comment - - 5 20000;query Like carol carol 10 0"

// stripTimes drops facetql's archive timestamps (the clock) from an
// engine session, leaving versions, results and the checkpoint.
var archiveTime = regexp.MustCompile(` t=\d+`)

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	os.MkdirAll(to, 0o755)
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		bs, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(to, e.Name()), bs, 0o644)
	}
}

// TestFqEngineBothWays: the write path and recovery against facetql's
// StorageEngine. The same workload run through each yields the same
// results, versions and checkpoint; then each recovers the other's
// crashed directory and its own, and every index tree and heap segment
// comes out page-identical between the two recoveries — and the index
// trees identical across the two writers.
func TestFqEngineBothWays(t *testing.T) {
	facetqlCheck(t)
	env := []string{"FACETQL_CHECKPOINT_INTERVAL=8", "FACETQL_WAL_ROTATE_BYTES=4096"}
	ops := strings.Join(engineWorkload(), ";")

	// facetql writes, then crashes.
	home := t.TempDir()
	rdir := filepath.Join(home, ".facetql")
	os.MkdirAll(rdir, 0o755)
	rust := strings.Join(runCheckEnv(t, append(env, "HOME="+home), nil, "engine-wal", ops), " ")
	if !strings.Contains(rust, "error: unique index") {
		t.Fatalf("facetql session: %s", rust)
	}

	// The port writes the same, then crashes.
	mdir := t.TempDir()
	ts := loadFqEngineApp(t, mdir)
	d := postExprJSON(t, ts, "runEngine", "", facetqlCheckKey, 71, 8, 4096, ops)
	mine, _ := d["engineOut"].(string)
	if got := archiveTime.ReplaceAllString(rust, ""); mine != got {
		t.Fatalf("engine sessions differ:\n port     %s\n facetql  %s", mine, got)
	}

	recoverBoth := func(t *testing.T, src string) (string, string, string) {
		h := t.TempDir()
		a := filepath.Join(h, ".facetql")
		copyDir(t, src, a)
		b := t.TempDir()
		copyDir(t, src, b)
		r := strings.Join(runCheckEnv(t, append(env, "HOME="+h), nil, "engine-wal", ""), " ")
		ts := loadFqEngineApp(t, b)
		d := postExprJSON(t, ts, "runEngine", "", facetqlCheckKey, 72, 8, 4096, "")
		m, _ := d["engineOut"].(string)
		if m != r {
			t.Fatalf("recovering %s:\n port     %s\n facetql  %s", src, m, r)
		}
		files := storageFiles(t, a)
		if got := storageFiles(t, b); strings.Join(got, ",") != strings.Join(files, ",") {
			t.Fatalf("storage files after recovery differ:\n port     %v\n facetql  %v", got, files)
		}
		samePagesIn(t, a, b, files)
		// The read path over each recovered store.
		rreads := strings.Join(runCheckEnv(t, append(env, "HOME="+h), nil, "engine-read", engineReads), "\n")
		d2 := postExprJSON(t, ts, "runEngineRead", "", facetqlCheckKey, 73, engineReads)
		mreads := strings.TrimSuffix(d2["engineOut"].(string), "\n")
		if mreads != rreads {
			t.Fatalf("reads over the recovered %s differ:\n port\n%s\n facetql\n%s", src, mreads, rreads)
		}
		if !strings.Contains(rreads, "C03|Comment|dave|7|private|-|{") || !strings.Contains(rreads, "none") {
			t.Fatalf("reads did not see the workload:\n%s", rreads)
		}
		return a, b, r
	}
	ra, _, rr := recoverBoth(t, rdir)
	ma, _, mr := recoverBoth(t, mdir)
	if rr != mr {
		t.Fatalf("recovered checkpoints differ: facetql's directory %s, the port's %s", rr, mr)
	}
	var idx []string
	for _, f := range storageFiles(t, ra) {
		if strings.HasPrefix(f, "facetql.idx.") {
			idx = append(idx, f)
		}
	}
	if len(idx) < 8 {
		t.Fatalf("expected the six trees and the declared indexes, got %v", idx)
	}
	samePagesIn(t, ra, ma, idx)
}

func loadFqQueryApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqquery.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqquery.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// predicate JSON in the IR's expression shape.
func pGet(field string) string {
	return `{"kind":"get","obj":{"kind":"ref","name":"p"},"field":"` + field + `"}`
}
func pLit(val, vtype string) string {
	return `{"kind":"lit","val":` + val + `,"vtype":"` + vtype + `"}`
}
func pBin(op, l, r string) string {
	return `{"kind":"bin","op":"` + op + `","l":` + l + `,"r":` + r + `}`
}
func pUn(op, x string) string { return `{"kind":"un","op":"` + op + `","x":` + x + `}` }

// queryDataset: posts under a unique slug index, a text index on the
// title and an ordered index on n, with fields missing, null, of mixed
// types and categories to group by, across several owners.
func queryDataset() string {
	w := []string{`index bySlug Post slug unique`, `text byTitle Post title`, `index byN Post n`, `index byCat Item cat`}
	for i := 0; i < 24; i++ {
		n := fmt.Sprintf("%d", (i*7)%11)
		switch i % 6 {
		case 1:
			n = fmt.Sprintf("%d.5", i)
		case 4:
			n = "null"
		}
		cat := []string{`"red"`, `"blue"`, `"green"`, `null`}[i%4]
		body := fmt.Sprintf(`{"slug":"s%02d","title":"Post number %d about %s","n":%s,"score":%d,"cat":%s}`, i, i, []string{"cats", "dogs", "birds"}[i%3], n, (i*13)%17, cat)
		if i%5 == 3 {
			body = fmt.Sprintf(`{"slug":"s%02d","title":"untitled","cat":"red"}`, i)
		}
		w = append(w, fmt.Sprintf(`put P%02d Post owner%d %s`, i, i%3, body))
	}
	for i := 0; i < 10; i++ {
		w = append(w, fmt.Sprintf(`put I%02d Item owner%d {"cat":"%s","qty":%d,"price":%d.25}`, i, i%2, []string{"a", "b", "c"}[i%3], i, i))
	}
	w = append(w, `put P03 Post owner0 {"slug":"s03","title":"rewritten Post number 3","n":99,"score":1,"cat":"blue"}`, `del P07`, `checkpoint`)
	return strings.Join(w, ";")
}

func queryRequests() []string {
	n, slug, title, score := pGet("n"), pGet("slug"), pGet("title"), pGet("score")
	eqSlug := pBin("==", slug, pLit(`"s05"`, "text"))
	line := func(op, kind, owner, req, where, order, desc, after, limit, offset, groupBy, values, fn, field string) string {
		return strings.Join([]string{op, kind, owner, req, where, "p", order, desc, after, limit, offset, groupBy, values, fn, field}, "\t")
	}
	return []string{
		line("where", "Post", "-", "-", "-", "-", "asc", "-", "5", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "-", "asc", "@prev", "5", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "-", "desc", "-", "4", "2", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", eqSlug, "-", "asc", "-", "10", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", eqSlug, "slug", "asc", "-", "10", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "n", "asc", "-", "6", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "n", "asc", "@prev", "6", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "n", "desc", "-", "6", "1", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "score", "asc", "-", "5", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "score", "asc", "@prev", "5", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "score", "desc", "-", "5", "3", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("contains", title, pLit(`"number 1"`, "text")), "-", "asc", "-", "20", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("contains", title, pLit(`"dogs"`, "text")), "-", "desc", "-", "3", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("contains", title, pLit(`"dogs"`, "text")), "-", "desc", "@prev", "3", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("starts_with", slug, pLit(`"s1"`, "text")), "slug", "asc", "-", "4", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("&&", pBin(">", n, pLit("3", "int")), pBin("<=", score, pLit("12", "int"))), "-", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("||", pBin("in", pGet("cat"), `{"kind":"lit","val":["red","green"]}`), pUn("!", pBin("!=", n, pLit("0", "int")))), "score", "desc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("==", pBin("%", score, pLit("4", "int")), pLit("1", "int")), "title", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin(">", pBin("+", n, pLit(`"x"`, "text")), pLit("1", "int")), "-", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("==", pBin("/", score, pLit("0", "int")), pLit("1", "int")), "-", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("not in", pGet("cat"), pLit(`"red"`, "text")), "-", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", pBin("==", pUn("-", score), pLit("-5", "int")), "-", "asc", "-", "50", "0", "-", "-", "-", "-"),
		line("where", "-", "owner1", "-", "-", "-", "asc", "-", "4", "1", "-", "-", "-", "-"),
		line("where", "-", "-", "owner2", "-", "-", "desc", "-", "3", "0", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "-", "asc", "-", "5", "20000", "-", "-", "-", "-"),
		line("where", "Post", "-", "-", "-", "-", "asc", "not-a-cursor!", "5", "0", "-", "-", "-", "-"),
		line("count", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-"),
		line("count", "-", "owner0", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-"),
		line("count", "Post", "-", "owner1", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-"),
		line("count", "Post", "-", "-", eqSlug, "-", "-", "-", "-", "-", "-", "-", "-", "-"),
		line("count", "Post", "-", "-", pBin("contains", title, pLit(`"cats"`, "text")), "-", "-", "-", "-", "-", "-", "-", "-", "-"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "sum", "score"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "sum", "n"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "avg", "score"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "min", "n"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "max", "title"),
		line("aggregate", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "-", "-", "sum", "title"),
		line("aggregate", "Item", "owner1", "-", "-", "-", "-", "-", "-", "-", "-", "-", "avg", "price"),
		line("countBy", "Item", "-", "-", "-", "-", "-", "-", "-", "-", "cat", "-", "-", "-"),
		line("countBy", "Item", "-", "-", "-", "-", "-", "-", "-", "-", "cat", `["c","a","zz"]`, "-", "-"),
		line("countBy", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "cat", "-", "-", "-"),
		line("countBy", "Post", "-", "-", pBin(">", score, pLit("3", "int")), "-", "-", "-", "-", "-", "cat", `["red",null]`, "-", "-"),
		line("aggregateBy", "Post", "-", "-", "-", "-", "-", "-", "-", "-", "cat", "-", "sum", "score"),
		line("aggregateBy", "Item", "-", "owner0", "-", "-", "-", "-", "-", "-", "cat", "-", "max", "qty"),
	}
}

// TestFqQueryBothWays: query_where (every access path and its cursor),
// count/aggregate_where and count/aggregate_by over the same stored
// dataset, answered by fqquery.fct and by facetql's StorageEngine.
func TestFqQueryBothWays(t *testing.T) {
	facetqlCheck(t)
	home := t.TempDir()
	rdir := filepath.Join(home, ".facetql")
	os.MkdirAll(rdir, 0o755)
	built := strings.Join(runCheckEnv(t, []string{"HOME=" + home}, nil, "engine-wal", queryDataset()), " ")
	if strings.Contains(built, "error") {
		t.Fatalf("building the dataset: %s", built)
	}
	mdir := t.TempDir()
	copyDir(t, rdir, mdir)
	reqs := queryRequests()
	rust := runCheckEnv(t, []string{"HOME=" + home}, reqs, "engine-query")
	ts := loadFqQueryApp(t, mdir)
	d := postExprJSON(t, ts, "runQuery", "", facetqlCheckKey, 81, strings.Join(reqs, "\n"))
	mine := strings.Split(strings.TrimSuffix(d["queryOut"].(string), "\n"), "\n")
	if len(mine) != len(rust) {
		t.Fatalf("port answered %d requests, facetql %d:\n%s", len(mine), len(rust), strings.Join(mine, "\n"))
	}
	for i := range reqs {
		if mine[i] != rust[i] {
			t.Errorf("request %d %s:\n port     %s\n facetql  %s", i, reqs[i], mine[i], rust[i])
		}
	}
}

// txScripts: execute_transaction's every TxOperation (and the batch's own
// staged view: a rewrite archiving what an earlier op of the batch wrote,
// a delete of something the batch inserted, a clear and a predicate
// delete cascading through a reference), claim, sequence_next and
// insert_with_edges — then one refusal per script, each its own session.
func txScripts() []string {
	gt := pBin(">", `{"kind":"get","obj":{"kind":"ref","name":"item"},"field":"n"}`, pLit("4", "int"))
	return []string{
		`index bySlug Post slug unique;index byPost Comment post;ref onPost Comment post Post - cascade;` +
			`put P1 Post alice {"slug":"a","n":1};put P2 Post alice {"slug":"b","n":5};put P3 Post bob {"slug":"c","n":9};` +
			`put C1 Comment alice {"post":"P1"};put C2 Comment bob {"post":"P2"};` +
			`tx ins P4 Post alice {"slug":"d","n":2}~edge P4 P1 rel~ins C3 Comment alice {"post":"P4"}~ins C3 Comment alice {"post":"P4","v":2};` +
			`tx setif P1 n alice user atmost:3 {"n":2,"x":true}~del P2~unedge P4 P1 rel alice user;` +
			`tx ins T1 Tmp carol {}~ins T2 Tmp carol {}~del T1;` +
			`tx clear Tmp carol user~ins Q1 Post dave {"slug":"q","n":7}~delwhere Post dave user ` + gt + `;` +
			`tx setif P3 n bob user eq:9 {"n":10}~setif P4 missing alice user absent {"missing":null};` +
			`claim P3 worker1;seq orders 5 alice user;seq orders 3 alice user;seq other 1 bob admin;` +
			`iwe P5 Post alice user P1:cites,P4:cites {"slug":"e","n":4};iwe P6 Post erin user - {"slug":"f"}`,
		`tx setif P1 n alice user atmost:1 {"n":3};tx ins P3 Post alice {"slug":"z"};tx del NOPE;` +
			`tx setif P3 n alice user absent {"n":1};tx ins N1 Post alice {"slug":"u1"}~ins N2 Post alice {"slug":"u1"};` +
			`claim P3 worker2;claim NOPE worker2;seq orders 2 bob user;iwe P7 Post alice user NOPE:cites {"slug":"g"};` +
			`tx unedge P4 P1 rel alice user;checkpoint`,
	}
}

func loadFqTxApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("fqtx.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fqtx.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestFqTxBothWays: each script run by facetql's StorageEngine and by
// fqtx.fct over their own directories gives the same results; afterwards
// both directories hold page-identical index trees, and the same reads.
func TestFqTxBothWays(t *testing.T) {
	facetqlCheck(t)
	home := t.TempDir()
	rdir := filepath.Join(home, ".facetql")
	os.MkdirAll(rdir, 0o755)
	mdir := t.TempDir()
	ts := loadFqTxApp(t, mdir)
	for i, script := range txScripts() {
		rust := archiveTime.ReplaceAllString(strings.Join(runCheckEnv(t, []string{"HOME=" + home, "FACETQL_CHECKPOINT_INTERVAL=4", "FACETQL_WAL_ROTATE_BYTES=4096"}, nil, "engine-wal", script), " "), "")
		d := postExprJSON(t, ts, "runTx", "", facetqlCheckKey, 90+i, 4, 4096, script)
		mine, _ := d["txOut"].(string)
		if mine != rust {
			t.Fatalf("script %d differs:\n %s\n port     %s\n facetql  %s", i, script, mine, rust)
		}
		if i == 0 && strings.Contains(rust, "error") {
			t.Fatalf("the successful script failed: %s", rust)
		}
		if i == 1 && strings.Count(rust, "error: ") != 10 {
			t.Fatalf("the refusals script should refuse ten writes: %s", rust)
		}
	}
	var idx []string
	for _, f := range storageFiles(t, rdir) {
		if strings.HasPrefix(f, "facetql.idx.") {
			idx = append(idx, f)
		}
	}
	samePagesIn(t, rdir, mdir, idx)
	reads := "get P1;get P2;get P3;get P4;get C3;get Q1;history P1;history P4;from P4;from P5;to P1;owner alice -;query Post - - 20 0;query Tmp - - 5 0;get __sequence:orders;get __sequence:other"
	rreads := strings.Join(runCheckEnv(t, []string{"HOME=" + home}, nil, "engine-read", reads), "\n")
	d := postExprJSON(t, ts, "runEngineRead", "", facetqlCheckKey, 99, reads)
	if mreads := strings.TrimSuffix(d["engineOut"].(string), "\n"); mreads != rreads {
		t.Fatalf("reads differ:\n port\n%s\n facetql\n%s", mreads, rreads)
	}
}

// reportSessions: three openings of one database. The first grants and
// revokes enough identities that the next opening compacts the user log,
// declares indexes and a reference, writes, deletes (a cascade) and runs a
// transaction; the second writes across automatic checkpoints and WAL
// rotations; the third only reads. Each is followed by change scans at
// several positions, limits and audiences.
func reportSessions() [][2]string {
	var users []string
	for i := 0; i < 40; i++ {
		users = append(users, fmt.Sprintf("user h%02d owner%d %s", i, i%5, []string{"user", "admin"}[i%7/6]))
	}
	for i := 0; i < 30; i++ {
		users = append(users, fmt.Sprintf("revoke h%02d", i))
	}
	s1 := strings.Join(users, ";") + ";" +
		`index byPost Comment post;ref onPost Comment post Post - cascade;text byTitle Post title;` +
		`put P1 Post alice {"title":"hello"};put P2 Post bob {"title":"world"};` +
		`put C1 Comment alice {"post":"P1"};put C2 Comment bob {"post":"P2"};put P1 Post alice {"title":"hello again"};` +
		`edge C1 P1 on;del P2;tx ins T1 Tmp carol {}~ins T2 Tmp carol {}~del T1;seq s 3 alice user;claim P1 w1`
	var s2 []string
	for i := 0; i < 30; i++ {
		s2 = append(s2, fmt.Sprintf(`put Q%02d Post dave {"title":"q %d","n":%d}`, i, i, i))
	}
	s2 = append(s2, `del Q03`, `put Q04 Post dave {"title":"q four"}`)
	scans := "0:500:alice:user;0:500:x:admin;0:3:x:admin;5:4:bob:user;0:500:carol:user"
	return [][2]string{
		{s1, scans},
		{strings.Join(s2, ";"), scans + ";1000000:5:x:admin"},
		{"", scans},
	}
}

// TestFqEngineReportBothWays: stats, the user/index/reference listings,
// the user log's compaction and the durable change scan, after the same
// sessions through facetql's StorageEngine and through fqtx.fct.
func TestFqEngineReportBothWays(t *testing.T) {
	facetqlCheck(t)
	for _, cfg := range []struct {
		name             string
		interval, rotate int
	}{{"rotating", 6, 2048}, {"retaining", 256, 67108864}} {
		t.Run(cfg.name, func(t *testing.T) {
			env := []string{fmt.Sprintf("FACETQL_CHECKPOINT_INTERVAL=%d", cfg.interval), fmt.Sprintf("FACETQL_WAL_ROTATE_BYTES=%d", cfg.rotate)}
			home := t.TempDir()
			os.MkdirAll(filepath.Join(home, ".facetql"), 0o755)
			mdir := t.TempDir()
			ts := loadFqTxApp(t, mdir)
			for i, sess := range reportSessions() {
				rust := runCheckEnv(t, append(env, "HOME="+home), nil, "engine-report", sess[0], sess[1])
				d := postExprJSON(t, ts, "runReport", "", facetqlCheckKey, 110+i, cfg.interval, cfg.rotate, sess[0], sess[1])
				mine := strings.Split(d["txOut"].(string), "\n")
				if len(mine) != len(rust) {
					t.Fatalf("session %d: %d report lines from the port, %d from facetql:\n port\n%s\n facetql\n%s", i, len(mine), len(rust), strings.Join(mine, "\n"), strings.Join(rust, "\n"))
				}
				for j := range rust {
					if j == 1 {
						var a, b map[string]any
						if err := json.Unmarshal([]byte(mine[j]), &a); err != nil {
							t.Fatalf("session %d: the port's stats are not JSON: %v\n%s", i, err, mine[j])
						}
						json.Unmarshal([]byte(rust[j]), &b)
						delete(a, "runtime")
						if !reflect.DeepEqual(a, b) {
							t.Fatalf("session %d stats differ:\n port     %s\n facetql  %s", i, mine[j], rust[j])
						}
						continue
					}
					if mine[j] != rust[j] {
						t.Errorf("session %d line %d:\n port     %s\n facetql  %s", i, j, mine[j], rust[j])
					}
				}
				all := strings.Join(rust, "\n")
				if cfg.name == "rotating" && i == 1 && !strings.Contains(all, "410 cannot scan changes from 0") {
					t.Fatalf("the rotated log should refuse a scan from 0:\n%s", all)
				}
				if cfg.name == "retaining" && i == 0 && !(strings.Contains(all, `"change":"deleted"`) && strings.Contains(all, `"change":"updated"`) && strings.Contains(all, `"complete":false`)) {
					t.Fatalf("the retained log should page creates, updates and deletes:\n%s", all)
				}
			}
		})
	}
}
