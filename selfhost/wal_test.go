package selfhost

// wal.fct / aes_gcm.fct: the self-hosted facetql WAL, checked three ways.
//
//  1. AES-256-GCM against Go's own crypto/aes + cipher.NewGCM (the same
//     standard the Rust `aes-gcm` crate implements).
//  2. Byte compatibility with the real Rust writer: testdata/wal_golden.tsv
//     holds one line per WalOperation variant — the record's bincode bytes
//     and the frame line facetql's own `wal::encode_frame` produced (under
//     the all-zero development key, FACETQL_MASTER_KEY unset). The port
//     must decode every frame to exactly those bincode bytes and, sealing
//     the decoded record again under the nonce the frame carries,
//     reproduce the Rust line byte for byte.
//  3. The durability contract, against real files: write a workload with
//     BEGIN/COMMIT framing, "crash" it (tear the last append, drop only its
//     newline, corrupt a middle frame, write past a torn frame), and
//     recover — torn tails are cut and the rest replayed, corruption and
//     impossible frames refuse startup, uncommitted and aborted
//     transactions never apply, and a restarted writer continues the
//     identifier counters.

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

const walZeroKey = "0000000000000000000000000000000000000000000000000000000000000000"

func loadWalApp(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	t.Setenv("FACET_DATA_DIR", dataDir)
	g, err := compile.File("wal.fct")
	if err != nil {
		t.Fatalf("compile selfhost/wal.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// walLast remembers each server's walOut: a delta only carries a cell whose
// value changed, so an action that recomputes the same text sends none.
var walLast = map[*httptest.Server]string{}

func walCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	if v, ok := d["walOut"].(string); ok {
		walLast[ts] = v
	}
	return walLast[ts]
}

func TestAesGcmMatchesGo(t *testing.T) {
	ts := loadWalApp(t, t.TempDir())
	cases := []struct{ key, nonce, pt string }{
		{walZeroKey, "000000000000000000000000", ""},
		{walZeroKey, "000102030405060708090a0b", "00"},
		{"603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4", "cafebabefacedbaddecaf888", "d9313225f88406e5a55909c5aff5269a86a7a9531534f7da2e4c303d8a318a721c3c0c95956809532fcf0e2449a6b525b16aedf5aa0de657ba637b39"},
		{"feffe9928665731c6d6a8f9467308308feffe9928665731c6d6a8f9467308308", "cafebabefacedbaddecaf888", strings.Repeat("ab", 47)},
		{"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", "0102030405060708090a0b0c", strings.Repeat("5a", 1000)},
	}
	// The standard builtins (aesGcmSeal/Open, through gcmSeal/gcmOpen) and
	// the cipher written in fct (gcmSealRef/gcmOpenRef) must both be the
	// standard's.
	for _, pair := range [][2]string{{"runGcmSeal", "runGcmOpen"}, {"runGcmSealRef", "runGcmOpenRef"}} {
		for _, c := range cases {
			key, _ := hex.DecodeString(c.key)
			nonce, _ := hex.DecodeString(c.nonce)
			pt, _ := hex.DecodeString(c.pt)
			block, err := aes.NewCipher(key)
			if err != nil {
				t.Fatal(err)
			}
			gcm, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal(err)
			}
			want := hex.EncodeToString(gcm.Seal(nil, nonce, pt, nil))
			got := walCall(t, ts, pair[0], c.key, c.nonce, c.pt)
			if got != want {
				t.Fatalf("seal(pt=%s):\n got  %s\n want %s", c.pt, got, want)
			}
			back := walCall(t, ts, pair[1], c.key, c.nonce, got)
			if back != "pt:"+c.pt {
				t.Fatalf("open(seal(%s)) = %s", c.pt, back)
			}
			tampered := []byte(got)
			if tampered[0] == '0' {
				tampered[0] = '1'
			} else {
				tampered[0] = '0'
			}
			if r := walCall(t, ts, pair[1], c.key, c.nonce, string(tampered)); r != "!auth" {
				t.Fatalf("a modified ciphertext authenticated: %s", r)
			}
		}
	}
}

func readWalGolden(t *testing.T) [][3]string {
	t.Helper()
	f, err := os.Open("testdata/wal_golden.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][3]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 3 {
			t.Fatalf("bad golden line %q", sc.Text())
		}
		out = append(out, [3]string{parts[0], parts[1], parts[2]})
	}
	if len(out) < 16 {
		t.Fatalf("golden file has %d vectors, want every WalOperation variant", len(out))
	}
	return out
}

func TestWalGoldenFramesRoundTripBitExact(t *testing.T) {
	ts := loadWalApp(t, t.TempDir())
	for _, g := range readWalGolden(t) {
		g := g
		t.Run(g[0], func(t *testing.T) {
			got := walCall(t, ts, "runWalGolden", walZeroKey, g[2])
			parts := strings.SplitN(got, "|", 3)
			if parts[0] != "durable" {
				t.Fatalf("Rust frame decoded as %s", got)
			}
			if parts[1] != g[1] {
				t.Fatalf("record bincode:\n got  %s\n want %s", parts[1], g[1])
			}
			if parts[2] != g[2] {
				t.Fatalf("re-encoded frame differs from Rust's:\n got  %.200s\n want %.200s", parts[2], g[2])
			}
		})
	}
}

func TestWalFrameClassification(t *testing.T) {
	ts := loadWalApp(t, t.TempDir())
	line := readWalGolden(t)[3][2]
	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'a' {
			b[i] = 'b'
		} else {
			b[i] = 'a'
		}
		return string(b)
	}
	otherKey := strings.Repeat("11", 32)
	cases := []struct{ name, key, line, want string }{
		{"short header", walZeroKey, line[:20], "torn|frame header truncated: 10 of 14 header bytes present"},
		{"short payload", walZeroKey, line[:len(line)-10], "torn|frame payload truncated"},
		{"odd hex", walZeroKey, line[:len(line)-1], "torn|frame is not valid hex"},
		{"trailing bytes", walZeroKey, line + "00", "corrupt|frame has 1 trailing byte(s) beyond declared payload length"},
		{"checksum", walZeroKey, flip(line, 60), "corrupt|frame checksum mismatch"},
		{"magic", walZeroKey, "00" + line[2:], "corrupt|frame magic mismatch"},
		{"wrong key", otherKey, line, "corrupt|frame failed authentication/decryption"},
	}
	for _, c := range cases {
		got := walCall(t, ts, "runWalDecode", c.key, c.line)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: got %q, want prefix %q", c.name, got, c.want)
		}
	}
}

// walWorkload: two standalone inserts, a committed transaction that
// inserts C and deletes A, an aborted one, an edge and a user, and a
// transaction left open (BEGIN + insert, never committed).
const walWorkload = `0 insert A a1
0 insert B b1
new begin
cur insert C c1
cur delete A
cur commit
new begin
cur insert E e1
cur abort
0 edge B C likes
0 user h1 bob admin
0 index doc_title Doc title
new begin
cur insert D d1`

func TestWalRecoveryContract(t *testing.T) {
	dir := t.TempDir()
	ts := loadWalApp(t, dir)
	const wal = "facetql.wal"
	path := filepath.Join(dir, wal)

	// First boot: nothing to recover, the workload is appended.
	if got := walCall(t, ts, "runWalSession", wal, walZeroKey, 1, walWorkload); got != "next=15/4/15" {
		t.Fatalf("first session: %s", got)
	}
	clean := walCall(t, ts, "runWalRecover", wal, walZeroKey, 0)
	wantClean := "nodes[B=b1,C=c1] edges[B>C:likes] users[h1:admin] indexes[doc_title] applied=12 next=15/4/15 torn=0@0"
	if clean != wantClean {
		t.Fatalf("clean recovery:\n got  %s\n want %s", clean, wantClean)
	}

	// Checkpoint floor: records at or below it are already durable and are
	// not replayed; the counters never go below it.
	if got := walCall(t, ts, "runWalRecover", wal, walZeroKey, 6); !strings.HasPrefix(got, "nodes[] edges[B>C:likes] users[h1:admin] indexes[doc_title] applied=12") {
		t.Fatalf("checkpoint 6: %s", got)
	}
	// A checkpoint that cuts through a committed frame's BEGIN is refused.
	if got := walCall(t, ts, "runWalRecover", wal, walZeroKey, 3); !strings.Contains(got, "has a durable COMMIT at sequence 6 but no BEGIN record") {
		t.Fatalf("checkpoint through a frame: %s", got)
	}

	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(full), "\n"), "\n")
	if len(lines) != 14 {
		t.Fatalf("WAL has %d lines, want 14", len(lines))
	}

	// Crash mid-append: the last frame is torn. Recovery cuts it at the
	// frame boundary (and the file with it) and replays the rest.
	torn := strings.Join(lines[:13], "\n") + "\n" + lines[13][:len(lines[13])/2]
	if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	boundary := len(strings.Join(lines[:13], "\n") + "\n")
	got := walCall(t, ts, "runWalRecover", wal, walZeroKey, 0)
	want := "nodes[B=b1,C=c1] edges[B>C:likes] users[h1:admin] indexes[doc_title] applied=12 next=14/4/14 torn=1@" + itoa(boundary)
	if got != want {
		t.Fatalf("torn tail:\n got  %s\n want %s", got, want)
	}
	if st, _ := os.Stat(path); st.Size() != int64(boundary) {
		t.Fatalf("torn tail not truncated: size %d, want %d", st.Size(), boundary)
	}

	// Restart after the crash: new traffic continues the counters, and the
	// open transaction from before the crash stays unapplied.
	if got := walCall(t, ts, "runWalSession", wal, walZeroKey, 2, "0 insert F f1\nnew begin\ncur delete B\ncur commit"); got != "next=18/5/18" {
		t.Fatalf("restart session: %s", got)
	}
	got = walCall(t, ts, "runWalRecover", wal, walZeroKey, 0)
	want = "nodes[C=c1,F=f1] edges[B>C:likes] users[h1:admin] indexes[doc_title] applied=17 next=18/5/18 torn=0@0"
	if got != want {
		t.Fatalf("after restart:\n got  %s\n want %s", got, want)
	}

	// A tear at exactly the last byte (only the newline lost): the frame is
	// whole and durable, and the splice guard keeps the next append on its
	// own line.
	full, _ = os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(string(full), "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := walCall(t, ts, "runWalSession", wal, walZeroKey, 3, "0 insert G g1"); got != "next=19/5/19" {
		t.Fatalf("session after a lost newline: %s", got)
	}
	got = walCall(t, ts, "runWalRecover", wal, walZeroKey, 0)
	if !strings.HasPrefix(got, "nodes[C=c1,F=f1,G=g1]") || !strings.HasSuffix(got, "torn=0@0") {
		t.Fatalf("after splice guard: %s", got)
	}

	// Mid-log corruption refuses startup rather than guessing.
	full, _ = os.ReadFile(path)
	lines = strings.Split(strings.TrimSuffix(string(full), "\n"), "\n")
	bad := []byte(lines[4])
	if bad[70] == 'f' {
		bad[70] = 'e'
	} else {
		bad[70] = 'f'
	}
	lines[4] = string(bad)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := walCall(t, ts, "runWalRecover", wal, walZeroKey, 0); !strings.Contains(got, "error: WAL line 5 at byte offset") || !strings.Contains(got, "frame checksum mismatch") {
		t.Fatalf("mid-log corruption: %s", got)
	}

	// A torn frame with a whole frame after it is not a crash tail.
	good := strings.Split(strings.TrimSuffix(string(full), "\n"), "\n")
	past := append([]string{}, good[:3]...)
	past = append(past, good[3][:40], good[4])
	if err := os.WriteFile(path, []byte(strings.Join(past, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := walCall(t, ts, "runWalRecover", wal, walZeroKey, 0); !strings.Contains(got, "WAL line 4 is a torn frame") || !strings.Contains(got, "but line 5 follows it") {
		t.Fatalf("torn frame mid-log: %s", got)
	}

	// Under another key every frame fails authentication.
	if err := os.WriteFile(path, full, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := walCall(t, ts, "runWalRecover", wal, strings.Repeat("22", 32), 0); !strings.Contains(got, "failed authentication/decryption") {
		t.Fatalf("wrong key: %s", got)
	}
}
