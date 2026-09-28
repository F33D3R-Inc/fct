package selfhost

// The FacetQL contract, as a table: one row per behaviour the Rust engine
// (facetql/src, facetql/tests) has, and for each row what proves the fct
// engine (fqserver.fct and the fq*.fct it is built from) has it too.
//
// A row is proved one of two ways:
//
//   - `provedBy` names a test in this package (or ../integration) that
//     already runs the scenario against both engines; the row checks the
//     test exists, so a renamed or deleted proof is a failing row, not a
//     silent gap;
//   - `run` is a scenario executed here, against `facetql start` and
//     against `facet exec fqserver.fct`, whose two transcripts must be
//     identical.
//
// A row with `notPorted` set is a known gap: it is skipped with the
// reason, so `go test -run TestFqContract -v | grep SKIP` is the list of
// what the fct engine still cannot do.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"
)

type fqContractRow struct {
	area, name string
	// source is where the behaviour is specified: a Rust file and symbol.
	source string
	// provedBy names an existing both-ways test.
	provedBy string
	// run is a scenario producing one transcript per engine.
	run func(t *testing.T, which string) string
	// cross names the engine that writes a data directory both then open.
	cross string
	// notPorted explains why the row cannot pass yet.
	notPorted string
}

// ── one live server ─────────────────────────────────────────────────────

// fqContractEnv is the environment every contract scenario starts from:
// two identities (the admin alice, the user bob), the development posture
// on a fixed key, and no rate limits (a scenario is one identity issuing
// hundreds of requests as fast as it can).
var fqContractEnv = []string{
	"FACETQL_TOKENS=tok:alice:admin,utok:bob", "FACETQL_ENV=development", "FACETQL_MASTER_KEY=" + facetqlCheckKey,
	"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off", "FACETQL_RATE_SUBSCRIBE=off",
}

// fqLive is one running server under the contract: "rust" (`facetql
// start`) or "fct" (`facet exec fqserver.fct`), on its data directory.
type fqLive struct {
	t     *testing.T
	which string
	dir   string
	env   []string
	port  int
	p     *fqProc
}

func fqBoot(t *testing.T, which, dir string, env ...string) *fqLive {
	t.Helper()
	l := &fqLive{t: t, which: which, dir: dir, env: append(append([]string{}, fqContractEnv...), env...)}
	l.start()
	return l
}

func (l *fqLive) start() {
	l.t.Helper()
	l.p = fqStartProc(l.t, l.which, l.dir, l.env...)
	l.port = fqWaitListening(l.t, l.p)
}

// kill is the crash: SIGKILL, nothing flushed.
func (l *fqLive) kill() {
	_ = l.p.cmd.Process.Kill()
	<-l.p.done
}

// term is the operator's stop: SIGTERM, and the exit status.
func (l *fqLive) term() int {
	l.t.Helper()
	_ = l.p.cmd.Process.Signal(syscall.SIGTERM)
	return l.p.fqExit(l.t)
}

func (l *fqLive) restart() {
	l.t.Helper()
	l.kill()
	l.start()
}

func (l *fqLive) raw(c fqCase) string { return fqRaw(l.t, l.port, c.raw(), 0) }

// do answers the normalised response: status line, sorted headers, body.
func (l *fqLive) do(c fqCase) string { return fqNorm(l.raw(c)) }

func (l *fqLive) status(c fqCase) int {
	r := l.raw(c)
	var code int
	fmt.Sscanf(r, "HTTP/1.1 %d", &code)
	return code
}

func (l *fqLive) body(c fqCase) string {
	r := l.raw(c)
	if i := strings.Index(r, "\r\n\r\n"); i >= 0 {
		return r[i+4:]
	}
	return r
}

func (l *fqLive) exists(address string) bool {
	return l.status(fqGet("/node/"+address, "tok")) == 200
}

// ok posts and insists on a 2xx.
func (l *fqLive) ok(c fqCase) {
	l.t.Helper()
	if s := l.status(c); s < 200 || s > 299 {
		l.t.Fatalf("%s: %s %s answered %d: %s", l.which, c.method, c.path, s, l.body(c))
	}
}

// fqTranscript accumulates labelled, normalised answers.
type fqTranscript struct {
	b strings.Builder
	l *fqLive
}

func (tr *fqTranscript) step(label string, c fqCase) {
	fmt.Fprintf(&tr.b, "## %s\n%s\n", label, tr.l.do(c))
}

// stepUsers is step for GET /admin/users, whose order is a HashMap's.
func (tr *fqTranscript) stepUsers(label string, c fqCase) {
	fmt.Fprintf(&tr.b, "## %s\n%s\n", label, fqSortedUsers(tr.l.do(c)))
}

// stepStream is step for an event stream: the head and what arrives in
// the next half second.
func (tr *fqTranscript) stepStream(label string, c fqCase) {
	fmt.Fprintf(&tr.b, "## %s\n%s\n", label, fqNorm(fqRaw(tr.l.t, tr.l.port, c.raw(), 500*time.Millisecond)))
}

func (tr *fqTranscript) note(format string, args ...any) {
	fmt.Fprintf(&tr.b, "## "+format+"\n", args...)
}

func (tr *fqTranscript) String() string { return tr.b.String() }

// fqBothWays runs one scenario against each engine (in parallel: the two
// share nothing) and insists the transcripts match.
func fqBothWays(t *testing.T, scenario func(t *testing.T, which string) string) {
	t.Helper()
	fqRustReference(t)
	var mu sync.Mutex
	out := map[string]string{}
	t.Run("engines", func(t *testing.T) {
		for _, which := range []string{"rust", "fct"} {
			which := which
			t.Run(which, func(t *testing.T) {
				t.Parallel()
				s := scenario(t, which)
				mu.Lock()
				out[which] = s
				mu.Unlock()
			})
		}
	})
	if t.Failed() {
		return
	}
	if os.Getenv("FQ_SHOW_TRANSCRIPT") != "" {
		t.Logf("transcript (facetql):\n%s", out["rust"])
	}
	if out["fct"] != out["rust"] {
		t.Errorf("transcripts differ:\n--- facetql\n%s\n--- fqserver.fct\n%s\n--- diff\n%s", out["rust"], out["fct"], fqFirstDiff(out["rust"], out["fct"]))
	}
}

// fqFirstDiff names the first differing line of two transcripts.
func fqFirstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d:\n  facetql:      %q\n  fqserver.fct: %q", i+1, x, y)
		}
	}
	return "(identical)"
}

// fqWalLines is the WAL's non-empty lines (one frame per line).
func fqWalLines(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "facetql.wal"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func fqWriteWal(t *testing.T, dir string, lines []string) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "facetql.wal"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fqCopyDir copies a data directory (files only; the engines keep a flat
// directory) so one seed can be opened by both engines.
func fqCopyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(from, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if rel == "." {
			return nil
		}
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, fi.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fqStartupReport is the "FacetQL failed to start" block of a server that
// refused, with the data directory normalised — what
// TestFqServerRefusalsBothWays compares.
func fqStartupReport(p *fqProc, dir string) string {
	stderr := strings.ReplaceAll(p.stderr.String(), dir, "<dir>")
	var failure []string
	keep := false
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, "FacetQL failed to start") {
			keep = true
		}
		if keep {
			failure = append(failure, l)
		}
	}
	return strings.Join(failure, "\n")
}

var fqMintedTokenRE = regexp.MustCompile(`"token":"([0-9a-f]{64})"`)

// ── the seeds ───────────────────────────────────────────────────────────

func fqTx(ops ...string) string { return `{"operations":[` + strings.Join(ops, ",") + `]}` }

func fqInsertOp(addr, kind, data, extra string) string {
	d, _ := json.Marshal(data)
	return fmt.Sprintf(`{"type":"insert_node","address":%q,"kind":%q,"x":0,"y":0,"z":0,"q":0,"data":%s%s}`, addr, kind, d, extra)
}

// fqSeedWide is crash_recovery.rs's `seed`: a parent, three children
// referencing it, a node the batch overwrites, an edge, an index over the
// children's score, the reference's index, and the cascade reference.
func fqSeedWide(l *fqLive) {
	l.t.Helper()
	l.ok(fqSend("POST", "/node", "tok", fqNode("Par:1", "Par", `{"n":"a"}`, `,"public":true`)))
	for i := 1; i <= 3; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Chi:%d", i), "Chi", fmt.Sprintf(`{"par":"Par:1","score":%d}`, i), `,"public":true`)))
	}
	l.ok(fqSend("POST", "/node", "tok", fqNode("Keep:1", "Keep", `{"v":1}`, `,"public":true`)))
	l.ok(fqSend("POST", "/edge", "tok", `{"from":"Par:1","to":"Chi:1","kind":"owns"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"chi_score","kind":"Chi","field":"score"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"chi_par","kind":"Chi","field":"par"}`))
	l.ok(fqSend("POST", "/admin/references", "tok", `{"name":"chi_to_par","kind":"Chi","field":"par","parent_kind":"Par","on_delete":"cascade"}`))
}

// fqWideBatch is crash_recovery.rs's WIDE_BATCH: an overwrite (archive +
// index re-key), a fresh insert, an edge, and a delete that cascades.
var fqWideBatch = fqTx(
	fqInsertOp("Keep:1", "Keep", `{"v":2}`, `,"public":true`),
	fqInsertOp("Chi:1", "Chi", `{"par":"Par:1","score":50}`, `,"public":true`),
	fqInsertOp("Chi:9", "Chi", `{"par":"Par:1","score":9}`, `,"public":true`),
	`{"type":"insert_edge","from":"Chi:9","to":"Chi:2","kind":"peer"}`,
	`{"type":"delete_node","address":"Par:1"}`,
)

// fqWideState reads every access path the wide batch touches.
func fqWideState(tr *fqTranscript) {
	tr.step("Par", fqGet("/nodes?kind=Par", "tok"))
	tr.step("Chi", fqGet("/nodes?kind=Chi", "tok"))
	tr.step("Chi by index", fqSend("POST", "/nodes/query", "tok", `{"kind":"Chi","order":"score","desc":true}`))
	tr.step("Keep:1", fqGet("/node/Keep:1", "tok"))
	for _, a := range []string{"Chi:1", "Chi:2", "Chi:3", "Par:1", "Keep:1"} {
		tr.step("history "+a, fqGet("/node/"+a+"/history", "tok"))
	}
	tr.step("edges out Par:1", fqGet("/node/Par:1/edges/out", "tok"))
	tr.step("Chi:9", fqGet("/node/Chi:9", "tok"))
	tr.step("references", fqGet("/admin/references", "tok"))
	tr.step("indexes", fqGet("/admin/indexes", "tok"))
}

// fqEngineStats is GET /stats without its runtime half.
func fqEngineStats(l *fqLive) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(l.body(fqGet("/stats", "tok"))), &m); err != nil {
		return "stats: " + err.Error()
	}
	delete(m, "runtime")
	b, _ := json.Marshal(m)
	return string(b)
}

// ── durability scenarios ───────────────────────────────────────────────

func fqScenarioCrashAcknowledged(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	var acknowledged []string
	for n := 0; n < 200; n++ {
		a := fmt.Sprintf("Crash:%06d", n)
		if s := l.status(fqSend("POST", "/node", "tok", fqNode(a, "Crash", fmt.Sprintf("payload-%d", n), ""))); s >= 200 && s < 300 {
			acknowledged = append(acknowledged, a)
		}
	}
	l.restart()
	var missing []string
	for _, a := range acknowledged {
		if !l.exists(a) {
			missing = append(missing, a)
		}
	}
	tr := &fqTranscript{l: l}
	tr.note("acknowledged %d missing %d %v", len(acknowledged), len(missing), missing)
	tr.step("count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Crash"}`))
	tr.step("last", fqGet("/node/Crash:000199", "tok"))
	return tr.String()
}

func fqScenarioCrashAtomic(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	const per, batches = 12, 40
	var committed []int
	for b := 0; b < batches; b++ {
		var ops []string
		for i := 0; i < per; i++ {
			ops = append(ops, fqInsertOp(fmt.Sprintf("Batch:%04d:%02d", b, i), "Batch", fmt.Sprintf("b%d", b), `,"public":true`))
		}
		if s := l.status(fqSend("POST", "/transaction", "tok", fqTx(ops...))); s >= 200 && s < 300 {
			committed = append(committed, b)
		}
	}
	l.restart()
	tr := &fqTranscript{l: l}
	whole, half, lost := 0, 0, 0
	for b := 0; b < batches; b++ {
		present := 0
		for i := 0; i < per; i++ {
			if l.exists(fmt.Sprintf("Batch:%04d:%02d", b, i)) {
				present++
			}
		}
		switch {
		case present == per:
			whole++
		case present == 0:
			for _, c := range committed {
				if c == b {
					lost++
				}
			}
		default:
			half++
		}
	}
	tr.note("batches %d committed %d whole %d half-applied %d committed-but-lost %d", batches, len(committed), whole, half, lost)
	tr.step("count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Batch"}`))
	return tr.String()
}

func fqScenarioCrashRepeated(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	var confirmed []string
	for round := 0; round < 3; round++ {
		for n := 0; n < 50; n++ {
			a := fmt.Sprintf("Round:%d:%04d", round, n)
			if s := l.status(fqSend("POST", "/node", "tok", fqNode(a, "Round", fmt.Sprintf("r%dn%d", round, n), ""))); s >= 200 && s < 300 {
				confirmed = append(confirmed, a)
			}
		}
		l.restart()
		missing := 0
		for _, a := range confirmed {
			if !l.exists(a) {
				missing++
			}
		}
		tr.note("after crash %d: confirmed %d missing %d", round, len(confirmed), missing)
	}
	tr.step("count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Round"}`))
	return tr.String()
}

func fqScenarioCleanRestart(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	for n := 0; n < 100; n++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Clean:%04d", n), "Clean", fmt.Sprintf("v%d", n), "")))
	}
	l.ok(fqSend("POST", "/edge", "tok", `{"from":"Clean:0001","to":"Clean:0002","kind":"next"}`))
	l.ok(fqSend("POST", "/sequence/ids/next", "tok", `{"count":5}`))
	l.ok(fqSend("PUT", "/node/Clean:0003", "tok", `{"data":"v3b"}`))
	code := l.term()
	l.start()
	tr := &fqTranscript{l: l}
	tr.note("exit %d", code)
	missing := 0
	for n := 0; n < 100; n++ {
		if !l.exists(fmt.Sprintf("Clean:%04d", n)) {
			missing++
		}
	}
	tr.note("missing %d", missing)
	tr.step("page", fqGet("/nodes?kind=Clean&limit=5&offset=95", "tok"))
	tr.step("edge", fqGet("/node/Clean:0001/edges/out", "tok"))
	tr.step("history", fqGet("/node/Clean:0003/history", "tok"))
	tr.step("sequence continues", fqSend("POST", "/sequence/ids/next", "tok", `{}`))
	tr.note("stats %s", fqEngineStats(l))
	return tr.String()
}

func fqScenarioCommitDropped(t *testing.T, which string) string {
	dir := t.TempDir()
	l := fqBoot(t, which, dir, "FACETQL_CHECKPOINT_INTERVAL=1000000")
	fqSeedWide(l)
	tr := &fqTranscript{l: l}
	tr.note("before")
	fqWideState(tr)
	l.ok(fqSend("POST", "/transaction", "tok", fqWideBatch))
	tr.note("applied")
	fqWideState(tr)
	l.kill()
	lines := fqWalLines(t, dir)
	fqWriteWal(t, dir, lines[:len(lines)-1])
	l.start()
	tr.note("after the COMMIT was removed")
	fqWideState(tr)
	return tr.String()
}

func fqScenarioCommitReplayed(t *testing.T, which string) string {
	dir := t.TempDir()
	l := fqBoot(t, which, dir, "FACETQL_CHECKPOINT_INTERVAL=1000000")
	fqSeedWide(l)
	l.ok(fqSend("POST", "/transaction", "tok", fqWideBatch))
	l.restart()
	tr := &fqTranscript{l: l}
	tr.note("after replay")
	fqWideState(tr)
	return tr.String()
}

func fqScenarioNoBegin(t *testing.T, which string) string {
	dir := t.TempDir()
	l := fqBoot(t, which, dir, "FACETQL_CHECKPOINT_INTERVAL=1000000")
	fqSeedWide(l)
	before := len(fqWalLines(t, dir))
	l.ok(fqSend("POST", "/transaction", "tok", fqWideBatch))
	lines := fqWalLines(t, dir)
	l.kill()
	kept := append(append([]string{}, lines[:before]...), lines[before+1:]...)
	fqWriteWal(t, dir, kept)
	p := fqStartProc(t, which, dir, l.env...)
	code := p.fqExit(t)
	return fmt.Sprintf("frame lines %d\nexit %d\n%s\n", len(lines)-before, code, fqStartupReport(p, dir))
}

func fqScenarioSingleProcess(t *testing.T, which string) string {
	dir := t.TempDir()
	l := fqBoot(t, which, dir)
	second := fqStartProc(t, which, dir, l.env...)
	code := second.fqExit(t)
	still := l.status(fqGet("/", ""))
	return fmt.Sprintf("second process exit %d\nfirst still answers %d\n%s\n", code, still, fqStartupReport(second, dir))
}

func fqScenarioCheckpointEveryWrite(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_CHECKPOINT_INTERVAL=1")
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"ck_n","kind":"Ck","field":"n"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"ck_t","kind":"Ck","field":"t","mode":"text"}`))
	for n := 0; n < 40; n++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Ck:%03d", n), "Ck", fmt.Sprintf(`{"n":%d,"t":"word%d and more"}`, n%7, n), `,"public":true`)))
	}
	l.ok(fqSend("POST", "/sequence/ck/next", "tok", `{"count":3}`))
	l.ok(fqSend("PUT", "/node/Ck:005", "tok", `{"data":"{\"n\":100,\"t\":\"rewritten\"}"}`))
	l.ok(fqSend("POST", "/edge", "tok", `{"from":"Ck:001","to":"Ck:002","kind":"e"}`))
	l.ok(fqSend("POST", "/transaction", "tok", fqTx(`{"type":"delete_node","address":"Ck:007"}`)))
	code := l.term()
	l.start()
	tr := &fqTranscript{l: l}
	tr.note("exit %d", code)
	tr.step("count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Ck"}`))
	tr.step("by index", fqSend("POST", "/nodes/query", "tok", `{"kind":"Ck","order":"n","desc":true,"limit":6}`))
	tr.step("text", fqSend("POST", "/nodes/query", "tok", `{"kind":"Ck","where":{"kind":"bin","op":"contains","l":{"kind":"get","obj":{"kind":"ref","name":"item"},"name":"t"},"r":{"kind":"lit","val":"word1"}}}`))
	tr.step("sequence", fqSend("POST", "/sequence/ck/next", "tok", `{}`))
	tr.step("history", fqGet("/node/Ck:005/history", "tok"))
	tr.step("edge", fqGet("/node/Ck:002/edges/in", "tok"))
	tr.step("deleted", fqGet("/node/Ck:007", "tok"))
	tr.note("stats %s", fqEngineStats(l))
	return tr.String()
}

func fqScenarioWalRotation(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_WAL_ROTATE_BYTES=4096", "FACETQL_CHECKPOINT_INTERVAL=8")
	for n := 0; n < 60; n++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Rot:%03d", n), "Rot", strings.Repeat("r", 64), `,"public":true`)))
	}
	tr := &fqTranscript{l: l}
	tr.step("changes from the start, live", fqGet("/changes?after=0&limit=5", "tok"))
	l.restart()
	missing := 0
	for n := 0; n < 60; n++ {
		if !l.exists(fmt.Sprintf("Rot:%03d", n)) {
			missing++
		}
	}
	tr.note("missing %d", missing)
	tr.step("changes from the start, reopened", fqGet("/changes?after=0&limit=5", "tok"))
	tr.step("count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Rot"}`))
	tr.note("stats %s", fqEngineStats(l))
	return tr.String()
}

func fqScenarioOverflowRecords(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	for i, size := range []int{3000, 5000, 20000, 100000} {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Big:%d", i), "Big", fmt.Sprintf(`{"i":%d,"blob":"%s"}`, i, strings.Repeat("x", size)), `,"public":true`)))
	}
	l.ok(fqSend("PUT", "/node/Big:2", "tok", fmt.Sprintf(`{"data":"{\"i\":2,\"blob\":\"%s\"}"}`, strings.Repeat("y", 30000))))
	l.restart()
	tr := &fqTranscript{l: l}
	for i := 0; i < 4; i++ {
		tr.step(fmt.Sprintf("Big:%d", i), fqGet(fmt.Sprintf("/node/Big:%d", i), "tok"))
	}
	tr.step("history", fqGet("/node/Big:2/history", "tok"))
	tr.step("sum", fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Big","func":"sum","field":"i"}`))
	return tr.String()
}

// fqSeedCross writes everything a data directory can hold.
func fqSeedCross(l *fqLive) {
	l.t.Helper()
	fqSeedWide(l)
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"par_n","kind":"Par","field":"n","mode":"text"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"keep_v","kind":"Keep","field":"v","unique":true}`))
	l.ok(fqSend("POST", "/node", "utok", fqNode("Bob:1", "Keep", `{"v":7}`, "")))
	l.ok(fqSend("POST", "/sequence/cross/next", "tok", `{"count":4}`))
	l.ok(fqSend("PUT", "/node/Keep:1", "tok", `{"data":"{\"v\":3}"}`))
	l.ok(fqSend("POST", "/admin/users", "tok", `{"owner":"carol"}`))
	l.ok(fqSend("POST", "/admin/users", "tok", `{"owner":"dave","role":"admin"}`))
	l.ok(fqSend("POST", "/publish", "tok", `{"payload":"hello"}`))
	l.ok(fqSend("POST", "/node", "tok", fqNode("Big:1", "Big", `{"blob":"`+strings.Repeat("z", 9000)+`"}`, `,"public":true`)))
	l.ok(fqSend("POST", "/transaction", "tok", fqTx(`{"type":"delete_node","address":"Chi:3"}`)))
}

// fqReadCross reads it all back.
func fqReadCross(tr *fqTranscript) {
	fqWideState(tr)
	tr.stepUsers("users", fqGet("/admin/users", "tok"))
	tr.step("sequence", fqSend("POST", "/sequence/cross/next", "tok", `{}`))
	tr.step("Bob:1 as bob", fqGet("/node/Bob:1", "utok"))
	tr.step("Keep by unique", fqSend("POST", "/nodes/query", "tok", `{"kind":"Keep","order":"v"}`))
	tr.step("Par text", fqSend("POST", "/nodes/query", "tok", `{"kind":"Par","where":{"kind":"bin","op":"starts_with","l":{"kind":"get","obj":{"kind":"ref","name":"item"},"name":"n"},"r":{"kind":"lit","val":"a"}}}`))
	tr.step("Big:1", fqGet("/node/Big:1", "tok"))
	tr.step("changes", fqGet("/changes?limit=100", "tok"))
	tr.step("unique still enforced", fqSend("POST", "/node", "tok", fqNode("Keep:2", "Keep", `{"v":7}`, "")))
	tr.note("stats %s", fqEngineStats(tr.l))
}

// fqCrossEngine: a data directory one engine wrote, opened by the other.
// The reader's transcript must match the writer reopening its own
// directory — the operator's binary swap.
func fqCrossEngine(t *testing.T, writer string) {
	t.Helper()
	fqRustReference(t)
	seed := t.TempDir()
	w := fqBoot(t, writer, seed)
	fqSeedCross(w)
	w.term()
	var mu sync.Mutex
	out := map[string]string{}
	t.Run("readers", func(t *testing.T) {
		for _, reader := range []string{"rust", "fct"} {
			reader := reader
			t.Run(reader, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				fqCopyDir(t, seed, dir)
				r := fqBoot(t, reader, dir)
				tr := &fqTranscript{l: r}
				fqReadCross(tr)
				r.ok(fqSend("POST", "/node", "tok", fqNode("After:1", "Keep", `{"v":99}`, "")))
				r.restart()
				tr.step("written by the reader, reopened", fqGet("/node/After:1", "tok"))
				mu.Lock()
				out[reader] = tr.String()
				mu.Unlock()
			})
		}
	})
	if t.Failed() {
		return
	}
	if out["fct"] != out["rust"] {
		t.Errorf("a directory written by %s:\n--- read by facetql\n%s\n--- read by fqserver.fct\n%s\n--- diff\n%s", writer, out["rust"], out["fct"], fqFirstDiff(out["rust"], out["fct"]))
	}
}

// ── engine semantics scenarios ─────────────────────────────────────────

func fqField(name string) string {
	return `{"kind":"get","obj":{"kind":"ref","name":"item"},"name":"` + name + `"}`
}

func fqBin(op, l, r string) string {
	return `{"kind":"bin","op":"` + op + `","l":` + l + `,"r":` + r + `}`
}

func fqLit(v string) string { return `{"kind":"lit","val":` + v + `}` }

func fqScenarioMultiGet(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	for i := 0; i < 8; i++ {
		key := "tok"
		public := `,"public":true`
		if i%3 == 0 {
			key, public = "utok", ""
		}
		l.ok(fqSend("POST", "/node", key, fqNode(fmt.Sprintf("M:%d", i), "M", fmt.Sprintf(`{"i":%d}`, i), public)))
	}
	tr := &fqTranscript{l: l}
	tr.step("in the order asked", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":["M:5","M:1","M:7","M:2"]}`))
	tr.step("absent ones skipped", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":["M:5","nope","M:1","M:9"]}`))
	tr.step("visibility as alice", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":["M:0","M:1","M:3"]}`))
	tr.step("visibility as bob", fqSend("POST", "/nodes/multiget", "utok", `{"addresses":["M:0","M:1","M:3","M:2"]}`))
	tr.step("duplicates", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":["M:1","M:1"]}`))
	tr.step("empty", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":[]}`))
	var many []string
	for i := 0; i < 1001; i++ {
		many = append(many, fmt.Sprintf(`"M:%d"`, i))
	}
	tr.step("past the bound", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":[`+strings.Join(many, ",")+`]}`))
	tr.step("at the bound", fqSend("POST", "/nodes/multiget", "tok", `{"addresses":[`+strings.Join(many[:1000], ",")+`]}`))
	return tr.String()
}

func fqScenarioPredicates(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	names := []string{"alice", "alistair", "bob", "carol", "ALFRED", "", "dave"}
	for i, n := range names {
		data := fmt.Sprintf(`{"handle":"%s","n":%d,"tags":["a","b%d"],"ok":%v,"ratio":%d.5}`, n, i, i, i%2 == 0, i)
		if n == "" {
			data = fmt.Sprintf(`{"n":%d}`, i)
		}
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("P:%d", i), "P", data, `,"public":true`)))
	}
	q := func(where string) fqCase {
		return fqSend("POST", "/nodes/query", "tok", `{"kind":"P","order":"n","where":`+where+`}`)
	}
	tr := &fqTranscript{l: l}
	run := func(label, where string) {
		tr.step(label+" (scan)", q(where))
	}
	cases := [][2]string{
		{"starts_with", fqBin("starts_with", fqField("handle"), fqLit(`"al"`))},
		{"ends_with", fqBin("ends_with", fqField("handle"), fqLit(`"ob"`))},
		{"contains", fqBin("contains", fqField("handle"), fqLit(`"ar"`))},
		{"contains empty needle", fqBin("contains", fqField("handle"), fqLit(`""`))},
		{"starts_with missing field", fqBin("starts_with", fqField("nope"), fqLit(`"a"`))},
		{"starts_with non-string", fqBin("starts_with", fqField("n"), fqLit(`"1"`))},
		{"in", fqBin("in", fqField("handle"), fqLit(`["bob","carol","zed"]`))},
		{"not in", fqBin("not in", fqField("handle"), fqLit(`["bob","carol"]`))},
		{"in numbers", fqBin("in", fqField("n"), fqLit(`[1,3,5.0]`))},
		{"in scalar right", fqBin("in", fqField("handle"), fqLit(`"bob"`))},
		{"in empty set", fqBin("in", fqField("handle"), fqLit(`[]`))},
		{"eq bool", fqBin("==", fqField("ok"), fqLit(`true`))},
		{"ne", fqBin("!=", fqField("n"), fqLit(`2`))},
		{"lt float", fqBin("<", fqField("ratio"), fqLit(`2.7`))},
		{"ge", fqBin(">=", fqField("n"), fqLit(`5`))},
		{"gt string", fqBin(">", fqField("handle"), fqLit(`"b"`))},
		{"and", fqBin("&&", fqBin(">", fqField("n"), fqLit(`1`)), fqBin("<", fqField("n"), fqLit(`5`)))},
		{"or", fqBin("||", fqBin("==", fqField("n"), fqLit(`0`)), fqBin("==", fqField("handle"), fqLit(`"dave"`)))},
		{"not", `{"kind":"un","op":"!","e":` + fqBin("==", fqField("n"), fqLit(`0`)) + `}`},
		{"neg", fqBin("==", `{"kind":"un","op":"-","e":`+fqField("n")+`}`, fqLit(`-3`))},
		{"unknown op", fqBin("~", fqField("n"), fqLit(`1`))},
		{"unknown kind", `{"kind":"frob"}`},
		{"eq null", fqBin("==", fqField("nope"), fqLit(`null`))},
		{"lit true", fqLit(`true`)},
		{"lit string", fqLit(`"yes"`)},
	}
	for _, c := range cases {
		run(c[0], c[1])
	}
	var set []string
	for i := 0; i < 1001; i++ {
		set = append(set, fmt.Sprintf(`"h%d"`, i))
	}
	run("in oversized set", fqBin("in", fqField("handle"), fqLit(`[`+strings.Join(set, ",")+`]`)))
	run("in set at the bound", fqBin("in", fqField("handle"), fqLit(`[`+strings.Join(set[:1000], ",")+`]`)))
	deep := fqLit(`1`)
	for i := 0; i < 70; i++ {
		deep = `{"kind":"un","op":"-","e":` + deep + `}`
	}
	run("too deep", fqBin("==", fqField("n"), deep))
	wide := fqBin("==", fqField("n"), fqLit(`0`))
	for i := 0; i < 130; i++ {
		wide = fqBin("||", wide, fqBin("==", fqField("n"), fqLit(`0`)))
	}
	run("too many nodes", wide)
	// The same string tests through a declared text index.
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"p_handle","kind":"P","field":"handle","mode":"text"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"p_n","kind":"P","field":"n"}`))
	for _, c := range cases[:6] {
		tr.step(c[0]+" (indexed)", q(c[1]))
	}
	tr.step("prefix with another condition (indexed)", q(fqBin("&&", fqBin("starts_with", fqField("handle"), fqLit(`"al"`)), fqBin(">", fqField("n"), fqLit(`0`)))))
	tr.step("count where", fqSend("POST", "/nodes/count", "tok", `{"kind":"P","where":`+fqBin("starts_with", fqField("handle"), fqLit(`"a"`))+`}`))
	return tr.String()
}

func fqScenarioKeysetPaging(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	for i := 0; i < 23; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("K:%02d", i), "K", fmt.Sprintf(`{"score":%d,"grp":"g%d"}`, (i*7)%10, i%3), `,"public":true`)))
	}
	tr := &fqTranscript{l: l}
	walk := func(label, base string) {
		after := ""
		for page := 0; page < 30; page++ {
			body := base
			if after != "" {
				body = strings.TrimSuffix(base, "}") + `,"after":` + after + `}`
			}
			resp := l.raw(fqSend("POST", "/nodes/query", "tok", body))
			tr.note("%s page %d", label, page)
			tr.b.WriteString(fqNorm(resp) + "\n")
			var m map[string]any
			_ = json.Unmarshal([]byte(resp[strings.Index(resp, "\r\n\r\n")+4:]), &m)
			n, ok := m["next"]
			if !ok || n == nil {
				break
			}
			b, _ := json.Marshal(n)
			after = string(b)
		}
	}
	walk("by score", `{"kind":"K","order":"score","limit":5}`)
	walk("by score desc", `{"kind":"K","order":"score","desc":true,"limit":7}`)
	walk("by address", `{"kind":"K","limit":10}`)
	walk("filtered", `{"kind":"K","order":"score","limit":4,"where":`+fqBin("==", fqField("grp"), fqLit(`"g1"`))+`}`)
	tr.step("bad cursor", fqSend("POST", "/nodes/query", "tok", `{"kind":"K","order":"score","limit":5,"after":"nonsense"}`))
	tr.step("cursor beats offset", fqSend("POST", "/nodes/query", "tok", `{"kind":"K","limit":3,"offset":20,"after":""}`))
	tr.step("huge cursor", fqSend("POST", "/nodes/query", "tok", `{"kind":"K","limit":3,"after":"`+strings.Repeat("a", 5000)+`"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"k_score","kind":"K","field":"score"}`))
	walk("by score, indexed", `{"kind":"K","order":"score","limit":5}`)
	walk("by score desc, indexed", `{"kind":"K","order":"score","desc":true,"limit":7}`)
	return tr.String()
}

func fqScenarioGrouping(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	for i := 0; i < 15; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("G:%02d", i), "G", fmt.Sprintf(`{"grp":"g%d","v":%d,"f":%d.25,"s":"x"}`, i%4, i, i), `,"public":true`)))
	}
	l.ok(fqSend("POST", "/node", "tok", fqNode("G:99", "G", `{"grp":"g0"}`, `,"public":true`)))
	tr := &fqTranscript{l: l}
	for _, c := range []struct{ label, path, body string }{
		{"count_by values", "/nodes/count_by", `{"kind":"G","group_by":"grp","values":["g1","g3","zz"]}`},
		{"count_by all", "/nodes/count_by", `{"kind":"G","group_by":"grp"}`},
		{"count_by numeric groups", "/nodes/count_by", `{"kind":"G","group_by":"v","values":[1,2.0,3]}`},
		{"count_by where", "/nodes/count_by", `{"kind":"G","group_by":"grp","where":` + fqBin(">", fqField("v"), fqLit(`6`)) + `}`},
		{"aggregate_by sum values", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"sum","field":"v","values":["g0","g2"]}`},
		{"aggregate_by avg", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"avg","field":"f"}`},
		{"aggregate_by min", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"min","field":"v"}`},
		{"aggregate_by max float", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"max","field":"f"}`},
		{"aggregate_by count", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"count"}`},
		{"aggregate_by count with field", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"count","field":"v"}`},
		{"aggregate_by non-number", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"sum","field":"s"}`},
		{"aggregate_by bogus func", "/nodes/aggregate_by", `{"kind":"G","group_by":"grp","func":"mode","field":"v"}`},
		{"aggregate min", "/nodes/aggregate", `{"kind":"G","func":"min","field":"f"}`},
		{"aggregate max", "/nodes/aggregate", `{"kind":"G","func":"max","field":"v"}`},
		{"aggregate empty sum", "/nodes/aggregate", `{"kind":"Nothing","func":"sum","field":"v"}`},
		{"aggregate empty avg", "/nodes/aggregate", `{"kind":"Nothing","func":"avg","field":"v"}`},
		{"aggregate empty min", "/nodes/aggregate", `{"kind":"Nothing","func":"min","field":"v"}`},
		{"aggregate empty count", "/nodes/aggregate", `{"kind":"Nothing","func":"count"}`},
		{"aggregate non-number", "/nodes/aggregate", `{"kind":"G","func":"sum","field":"s"}`},
		{"aggregate as bob", "/nodes/aggregate", `{"kind":"G","func":"count"}`},
	} {
		key := "tok"
		if strings.HasSuffix(c.label, "as bob") {
			key = "utok"
		}
		tr.step(c.label, fqSend("POST", c.path, key, c.body))
	}
	var vals []string
	for i := 0; i < 1001; i++ {
		vals = append(vals, fmt.Sprintf(`"g%d"`, i))
	}
	tr.step("count_by too many values", fqSend("POST", "/nodes/count_by", "tok", `{"kind":"G","group_by":"grp","values":[`+strings.Join(vals, ",")+`]}`))
	return tr.String()
}

func fqScenarioUniqueIndex(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"u_handle","kind":"U","field":"handle","unique":true}`))
	tr.step("first", fqSend("POST", "/node", "tok", fqNode("U:1", "U", `{"handle":"ann"}`, `,"public":true`)))
	tr.step("duplicate refused", fqSend("POST", "/node", "tok", fqNode("U:2", "U", `{"handle":"ann"}`, `,"public":true`)))
	tr.step("original intact", fqGet("/node/U:1", "tok"))
	tr.step("duplicate absent", fqGet("/node/U:2", "tok"))
	tr.step("keeps its own value", fqSend("PUT", "/node/U:1", "tok", `{"data":"{\"handle\":\"ann\",\"x\":1}"}`))
	tr.step("two in one batch", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("U:3", "U", `{"handle":"bea"}`, ""), fqInsertOp("U:4", "U", `{"handle":"bea"}`, ""))))
	tr.step("neither landed", fqGet("/nodes?kind=U", "tok"))
	l.ok(fqSend("POST", "/node", "tok", fqNode("U:5", "U", `{"handle":"cid"}`, "")))
	tr.step("moved in one batch", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("U:5", "U", `{"handle":"cid2"}`, ""), fqInsertOp("U:6", "U", `{"handle":"cid"}`, ""))))
	tr.step("after the move", fqSend("POST", "/nodes/query", "tok", `{"kind":"U","order":"handle"}`))
	tr.step("duplicate via transaction", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("U:7", "U", `{"handle":"cid"}`, ""))))
	tr.step("null handle twice", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("U:8", "U", `{}`, ""), fqInsertOp("U:9", "U", `{"handle":null}`, ""))))
	l.ok(fqSend("POST", "/node", "tok", fqNode("D:1", "D", `{"h":"same"}`, "")))
	l.ok(fqSend("POST", "/node", "tok", fqNode("D:2", "D", `{"h":"same"}`, "")))
	tr.step("unique over duplicated data refused", fqSend("POST", "/admin/indexes", "tok", `{"name":"d_h","kind":"D","field":"h","unique":true}`))
	tr.step("non-unique over the same data", fqSend("POST", "/admin/indexes", "tok", `{"name":"d_h2","kind":"D","field":"h"}`))
	tr.step("unique text mode", fqSend("POST", "/admin/indexes", "tok", `{"name":"u_t","kind":"U","field":"handle","mode":"text","unique":true}`))
	l.restart()
	tr.step("survives a reopen", fqSend("POST", "/node", "tok", fqNode("U:10", "U", `{"handle":"ann"}`, "")))
	tr.step("delete frees the value", fqSend("DELETE", "/node/U:1", "tok", ""))
	tr.step("taken again", fqSend("POST", "/node", "tok", fqNode("U:11", "U", `{"handle":"ann"}`, "")))
	tr.step("indexes", fqGet("/admin/indexes", "tok"))
	return tr.String()
}

func fqScenarioReferences(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	decl := func(name, kind, field, parent, onDelete string) fqCase {
		return fqSend("POST", "/admin/references", "tok", fmt.Sprintf(`{"name":%q,"kind":%q,"field":%q,"parent_kind":%q,"on_delete":%q}`, name, kind, field, parent, onDelete))
	}
	tr.step("needs the child index", decl("c_to_p", "C", "p", "P", "cascade"))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"c_p","kind":"C","field":"p"}`))
	l.ok(fqSend("POST", "/node", "tok", fqNode("C:orphan", "C", `{"p":"P:none"}`, `,"public":true`)))
	tr.step("existing data breaks it", decl("c_to_p", "C", "p", "P", "cascade"))
	l.ok(fqSend("DELETE", "/node/C:orphan", "tok", ""))
	tr.step("declared", decl("c_to_p", "C", "p", "P", "cascade"))
	tr.step("re-declared the same", decl("c_to_p", "C", "p", "P", "cascade"))
	tr.step("re-declared differently", decl("c_to_p", "C", "p", "P", "restrict"))
	tr.step("bad on_delete", decl("c_to_p2", "C", "p", "P", "explode"))
	tr.step("long name", decl(strings.Repeat("n", 65), "C", "p", "P", "cascade"))
	tr.step("its index cannot be dropped", fqSend("DELETE", "/admin/indexes/c_p", "tok", ""))
	tr.step("missing parent refused", fqSend("POST", "/node", "tok", fqNode("C:1", "C", `{"p":"P:1"}`, `,"public":true`)))
	l.ok(fqSend("POST", "/node", "tok", fqNode("P:1", "P", `{}`, `,"public":true`)))
	l.ok(fqSend("POST", "/node", "tok", fqNode("P:2", "P", `{}`, `,"public":true`)))
	for i := 1; i <= 4; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("C:%d", i), "C", fmt.Sprintf(`{"p":"P:%d"}`, 1+i%2), `,"public":true`)))
	}
	tr.step("parent-then-child in one batch", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("C:5", "C", `{"p":"P:3"}`, ""), fqInsertOp("P:3", "P", `{}`, ""))))
	tr.step("child without parent in a batch", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("C:6", "C", `{"p":"P:4"}`, ""))))
	// A grandchild level, so the cascade is a graph.
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"g_c","kind":"G","field":"c"}`))
	tr.step("grandchild reference", decl("g_to_c", "G", "c", "C", "cascade"))
	l.ok(fqSend("POST", "/node", "tok", fqNode("G:1", "G", `{"c":"C:1"}`, `,"public":true`)))
	l.ok(fqSend("POST", "/node", "tok", fqNode("G:2", "G", `{"c":"C:3"}`, `,"public":true`)))
	tr.step("cascade", fqSend("DELETE", "/node/P:1", "tok", ""))
	tr.step("after cascade C", fqGet("/nodes?kind=C", "tok"))
	tr.step("after cascade G", fqGet("/nodes?kind=G", "tok"))
	tr.step("after cascade P", fqGet("/nodes?kind=P", "tok"))
	tr.step("archived C:1 history via a re-insert", fqSend("POST", "/node", "tok", fqNode("C:1", "C", `{"p":"P:2"}`, "")))
	tr.step("history C:1", fqGet("/node/C:1/history", "tok"))
	tr.step("bulk delete cascades", fqSend("POST", "/transaction", "tok", fqTx(`{"type":"delete_where","kind":"P","where":`+fqLit(`true`)+`}`)))
	tr.step("after bulk C", fqGet("/nodes?kind=C", "tok"))
	tr.step("after bulk G", fqGet("/nodes?kind=G", "tok"))
	// restrict and set_null
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"r_p","kind":"R","field":"p"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"s_p","kind":"S","field":"p"}`))
	tr.step("restrict declared", decl("r_to_p", "R", "p", "P", "restrict"))
	tr.step("set_null declared", decl("s_to_p", "S", "p", "P", "set_null"))
	l.ok(fqSend("POST", "/node", "tok", fqNode("P:9", "P", `{}`, `,"public":true`)))
	l.ok(fqSend("POST", "/node", "tok", fqNode("R:1", "R", `{"p":"P:9"}`, `,"public":true`)))
	l.ok(fqSend("POST", "/node", "tok", fqNode("S:1", "S", `{"p":"P:9","keep":1}`, `,"public":true`)))
	tr.step("restrict refuses", fqSend("DELETE", "/node/P:9", "tok", ""))
	tr.step("restrict via batch", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("P:10", "P", `{}`, ""), `{"type":"delete_node","address":"P:9"}`)))
	tr.step("nothing applied", fqGet("/nodes?kind=P", "tok"))
	l.ok(fqSend("DELETE", "/node/R:1", "tok", ""))
	tr.step("set_null clears", fqSend("DELETE", "/node/P:9", "tok", ""))
	tr.step("child kept", fqGet("/node/S:1", "tok"))
	tr.step("child history", fqGet("/node/S:1/history", "tok"))
	tr.step("parent and child together", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("P:11", "P", `{}`, ""), fqInsertOp("C:11", "C", `{"p":"P:11"}`, ""))))
	tr.step("both deleted together", fqSend("POST", "/transaction", "tok", fqTx(`{"type":"delete_node","address":"P:11"}`, `{"type":"delete_node","address":"C:11"}`)))
	tr.step("history C:11", fqGet("/node/C:11/history", "tok"))
	// A cycle: X references Y references X.
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"x_y","kind":"X","field":"y"}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"y_x","kind":"Y","field":"x"}`))
	tr.step("cycle 1", decl("x_to_y", "X", "y", "Y", "cascade"))
	tr.step("cycle 2", decl("y_to_x", "Y", "x", "X", "cascade"))
	tr.step("cycle nodes", fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("X:1", "X", `{"y":"Y:1"}`, ""), fqInsertOp("Y:1", "Y", `{"x":"X:1"}`, ""))))
	tr.step("cycle delete terminates", fqSend("DELETE", "/node/X:1", "tok", ""))
	tr.step("cycle gone", fqGet("/nodes?kind=Y", "tok"))
	// Referencing by a data field through the parent's unique index.
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"p_slug","kind":"P","field":"slug","unique":true}`))
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"t_ps","kind":"T","field":"ps"}`))
	tr.step("by parent field", fqSend("POST", "/admin/references", "tok", `{"name":"t_to_p","kind":"T","field":"ps","parent_kind":"P","parent_field":"slug","on_delete":"cascade"}`))
	l.ok(fqSend("POST", "/node", "tok", fqNode("P:20", "P", `{"slug":"twenty"}`, `,"public":true`)))
	tr.step("child by slug", fqSend("POST", "/node", "tok", fqNode("T:1", "T", `{"ps":"twenty"}`, `,"public":true`)))
	tr.step("child by missing slug", fqSend("POST", "/node", "tok", fqNode("T:2", "T", `{"ps":"none"}`, `,"public":true`)))
	tr.step("cascade by slug", fqSend("DELETE", "/node/P:20", "tok", ""))
	tr.step("T after", fqGet("/nodes?kind=T", "tok"))
	tr.step("references", fqGet("/admin/references", "tok"))
	tr.step("drop", fqSend("DELETE", "/admin/references/t_to_p", "tok", ""))
	tr.step("drop again", fqSend("DELETE", "/admin/references/t_to_p", "tok", ""))
	tr.step("now its index can go", fqSend("DELETE", "/admin/indexes/t_ps", "tok", ""))
	l.restart()
	tr.step("still cascades after a restart", fqSend("DELETE", "/node/P:2", "tok", ""))
	tr.step("C after restart cascade", fqGet("/nodes?kind=C", "tok"))
	tr.step("references after restart", fqGet("/admin/references", "tok"))
	return tr.String()
}

func fqScenarioCascadeBound(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_MAX_TRANSACTION_OPS=12")
	tr := &fqTranscript{l: l}
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"cb_child","kind":"CbChild","field":"parent"}`))
	l.ok(fqSend("POST", "/admin/references", "tok", `{"name":"cb","kind":"CbChild","field":"parent","parent_kind":"CbParent","on_delete":"cascade"}`))
	l.ok(fqSend("POST", "/node", "tok", fqNode("CbParent:small", "CbParent", `{}`, `,"public":true`)))
	for i := 0; i < 2; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("CbChild:s%d", i), "CbChild", `{"parent":"CbParent:small"}`, `,"public":true`)))
	}
	tr.step("fits", fqSend("DELETE", "/node/CbParent:small", "tok", ""))
	tr.step("children gone", fqGet("/nodes?kind=CbChild", "tok"))
	l.ok(fqSend("POST", "/node", "tok", fqNode("CbParent:big", "CbParent", `{}`, `,"public":true`)))
	for i := 0; i < 9; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("CbChild:b%d", i), "CbChild", `{"parent":"CbParent:big"}`, `,"public":true`)))
	}
	tr.step("too large", fqSend("DELETE", "/node/CbParent:big", "tok", ""))
	tr.step("parent still there", fqGet("/node/CbParent:big", "tok"))
	tr.step("children count", fqSend("POST", "/nodes/count", "tok", `{"kind":"CbChild"}`))
	tr.step("too large via batch", fqSend("POST", "/transaction", "tok", fqTx(`{"type":"delete_node","address":"CbParent:big"}`)))
	tr.step("a batch past the bound outright", fqSend("POST", "/transaction", "tok", fqTx(strings.Split(strings.Repeat(`{"type":"clear_kind","kind":"Nothing"}|`, 13), "|")[:13]...)))
	return tr.String()
}

func fqScenarioSequences(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	tr.step("fresh starts at one", fqSend("POST", "/sequence/s1/next", "tok", `{}`))
	tr.step("counts up", fqSend("POST", "/sequence/s1/next", "tok", `{}`))
	tr.step("separate", fqSend("POST", "/sequence/s2/next", "tok", `{}`))
	tr.step("block", fqSend("POST", "/sequence/s1/next", "tok", `{"count":100}`))
	tr.step("after block", fqSend("POST", "/sequence/s1/next", "tok", `{}`))
	tr.step("belongs to alice", fqSend("POST", "/sequence/s1/next", "utok", `{}`))
	tr.step("bob's own", fqSend("POST", "/sequence/s1b/next", "utok", `{}`))
	tr.step("admin on bob's", fqSend("POST", "/sequence/s1b/next", "tok", `{}`))
	tr.step("block too big", fqSend("POST", "/sequence/s1/next", "tok", `{"count":100001}`))
	tr.step("block at the bound", fqSend("POST", "/sequence/s1/next", "tok", `{"count":100000}`))
	tr.step("count zero", fqSend("POST", "/sequence/s1/next", "tok", `{"count":0}`))
	tr.step("count negative", fqSend("POST", "/sequence/s1/next", "tok", `{"count":-1}`))
	tr.step("count float", fqSend("POST", "/sequence/s1/next", "tok", `{"count":1.5}`))
	tr.step("no body", fqCase{method: "POST", path: "/sequence/s1/next", key: "tok"})
	tr.step("name with colon", fqSend("POST", "/sequence/a:b/next", "tok", `{}`))
	tr.step("long name", fqSend("POST", "/sequence/"+strings.Repeat("n", 129)+"/next", "tok", `{}`))
	tr.step("name at the bound", fqSend("POST", "/sequence/"+strings.Repeat("n", 128)+"/next", "tok", `{}`))
	tr.step("escaped name", fqSend("POST", "/sequence/a%20b/next", "tok", `{}`))
	tr.step("the sequence node is a node", fqGet("/node/_sequence:s1", "tok"))
	tr.step("kind listing", fqGet("/nodes?kind=_sequence", "tok"))
	l.restart()
	tr.step("survives a restart", fqSend("POST", "/sequence/s1/next", "tok", `{}`))
	tr.step("and so does the other", fqSend("POST", "/sequence/s2/next", "tok", `{}`))
	return tr.String()
}

// ── server scenarios ───────────────────────────────────────────────────

func fqScenarioMaxSubscribers(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_MAX_SUBSCRIBERS=2")
	open := func(key string) net.Conn {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", l.port))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "GET /events HTTP/1.1\r\nHost: x\r\nx-api-key: %s\r\n\r\n", key)
		return c
	}
	head := func(c net.Conn) string {
		var out []byte
		buf := make([]byte, 4096)
		for !strings.Contains(string(out), "\r\n\r\n") {
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				break
			}
		}
		return fqNorm(string(out))
	}
	tr := &fqTranscript{l: l}
	a, b := open("tok"), open("utok")
	tr.note("first\n%s", head(a))
	tr.note("second\n%s", head(b))
	c := open("tok")
	third := fqRaw(t, l.port, "GET /events HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\n\r\n", 0)
	tr.note("third (over the cap)\n%s", fqNorm(third))
	c.Close()
	tr.step("a request is not a subscriber", fqGet("/nodes", "tok"))
	a.Close()
	time.Sleep(500 * time.Millisecond)
	fourth := open("tok")
	tr.note("after one left\n%s", head(fourth))
	fourth.Close()
	b.Close()
	return tr.String()
}

func fqScenarioPersistentUsers(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	mint := func(body string) string {
		raw := l.raw(fqSend("POST", "/admin/users", "tok", body))
		tr.note("mint %s\n%s", body, fqNorm(raw))
		m := fqMintedTokenRE.FindStringSubmatch(raw)
		if m == nil {
			return "none"
		}
		return m[1]
	}
	carol := mint(`{"owner":"carol"}`)
	dave := mint(`{"owner":"dave","role":"admin"}`)
	tr.step("carol again", fqSend("POST", "/admin/users", "tok", `{"owner":"carol"}`))
	tr.step("empty owner", fqSend("POST", "/admin/users", "tok", `{"owner":""}`))
	tr.step("owner with colon", fqSend("POST", "/admin/users", "tok", `{"owner":"a:b"}`))
	tr.step("bad role", fqSend("POST", "/admin/users", "tok", `{"owner":"x","role":"god"}`))
	tr.step("no owner", fqSend("POST", "/admin/users", "tok", `{"role":"admin"}`))
	tr.step("carol reads", fqGet("/nodes", carol))
	tr.step("carol writes", fqSend("POST", "/node", carol, fqNode("C:1", "C", `{}`, "")))
	tr.step("carol owns it", fqGet("/node/C:1", carol))
	tr.step("carol is no admin", fqGet("/admin/users", carol))
	tr.stepUsers("dave is", fqGet("/admin/users", dave))
	tr.step("dave's ?key=", fqGet("/nodes?key="+dave, ""))
	tr.step("a wrong minted-looking token", fqGet("/nodes", strings.Repeat("0", 64)))
	l.restart()
	tr.step("carol after a restart", fqGet("/node/C:1", carol))
	tr.stepUsers("list after a restart", fqGet("/admin/users", "tok"))
	tr.step("revoke carol", fqSend("DELETE", "/admin/users/carol", "tok", ""))
	tr.step("carol refused", fqGet("/nodes", carol))
	tr.step("her node stays", fqGet("/node/C:1", "tok"))
	tr.step("revoke again", fqSend("DELETE", "/admin/users/carol", "tok", ""))
	tr.step("revoke an env identity", fqSend("DELETE", "/admin/users/bob", "tok", ""))
	tr.step("bob still works", fqGet("/nodes", "utok"))
	tr.step("dave revokes himself", fqSend("DELETE", "/admin/users/dave", dave, ""))
	tr.step("dave refused", fqGet("/nodes", dave))
	l.restart()
	tr.step("revocations persist", fqGet("/nodes", carol))
	tr.stepUsers("list at the end", fqGet("/admin/users", "tok"))
	return tr.String()
}

func fqScenarioCorsAllowlist(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_ALLOWED_ORIGINS=https://app.example, https://other.example", "FACETQL_ENV=production", "FACETQL_ALLOW_PLAINTEXT=1")
	tr := &fqTranscript{l: l}
	req := func(label, method, path, origin, extra string) {
		raw := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\nOrigin: %s\r\n%s\r\n", method, path, origin, extra)
		tr.note("%s\n%s", label, fqNorm(fqRaw(t, l.port, raw, 0)))
	}
	req("allowed origin", "GET", "/nodes", "https://app.example", "")
	req("second allowed origin", "GET", "/nodes", "https://other.example", "")
	req("unknown origin", "GET", "/nodes", "https://evil.example", "")
	req("preflight allowed", "OPTIONS", "/node", "https://app.example", "Access-Control-Request-Method: POST\r\nAccess-Control-Request-Headers: x-api-key, content-type\r\n")
	req("preflight unknown", "OPTIONS", "/node", "https://evil.example", "Access-Control-Request-Method: POST\r\n")
	req("preflight bad method", "OPTIONS", "/node", "https://app.example", "Access-Control-Request-Method: PATCH\r\n")
	req("banner with origin", "GET", "/", "https://app.example", "")
	return tr.String()
}

func fqScenarioEventsAndChanges(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	tr.step("changes before anything", fqGet("/changes", "tok"))
	tr.stepStream("events resume before anything", fqGet("/events?after=0", "tok"))
	for i := 0; i < 5; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("E:%d", i), "E", `{}`, `,"public":true`)))
	}
	l.ok(fqSend("POST", "/node", "utok", fqNode("E:bob", "E", `{}`, "")))
	l.ok(fqSend("PUT", "/node/E:1", "tok", `{"data":"v2"}`))
	l.ok(fqSend("DELETE", "/node/E:2", "tok", ""))
	l.ok(fqSend("POST", "/transaction", "tok", fqTx(fqInsertOp("E:tx1", "E", `{}`, ""), fqInsertOp("E:tx2", "E", `{}`, ""), `{"type":"delete_node","address":"E:tx1"}`)))
	l.ok(fqSend("POST", "/edge", "tok", `{"from":"E:0","to":"E:1","kind":"e"}`))
	l.ok(fqSend("POST", "/publish", "tok", `{"payload":"note"}`))
	_ = l.status(fqSend("POST", "/node/E:4/claim", "utok", ""))
	tr.step("changes all", fqGet("/changes", "tok"))
	tr.step("changes as bob", fqGet("/changes", "utok"))
	tr.step("changes paged", fqGet("/changes?limit=3", "tok"))
	tr.step("changes bad after", fqGet("/changes?after=x", "tok"))
	tr.step("changes negative", fqGet("/changes?after=-1", "tok"))
	tr.step("changes limit past the cap", fqGet("/changes?limit=5000", "tok"))
	tr.step("changes far ahead", fqGet("/changes?after=999999", "tok"))
	tr.stepStream("events far ahead", fqGet("/events?after=999999", "tok"))
	tr.step("events bad after", fqGet("/events?after=x", "tok"))
	tr.stepStream("events as bob from the start", fqGet("/events?after=0", "utok"))
	tr.step("publish too large", fqSend("POST", "/publish", "tok", `{"payload":"`+strings.Repeat("p", 65537)+`"}`))
	tr.step("publish at the bound", fqSend("POST", "/publish", "tok", `{"payload":"`+strings.Repeat("p", 65536-12)+`"}`))
	tr.step("publish empty", fqSend("POST", "/publish", "tok", `{"payload":""}`))
	tr.step("publish no payload", fqSend("POST", "/publish", "tok", `{}`))
	tr.step("publish with channel", fqSend("POST", "/publish", "tok", `{"channel":"c","payload":"x"}`))
	l.restart()
	tr.step("changes after a restart", fqGet("/changes", "tok"))
	tr.stepStream("events after a restart", fqGet("/events?after=0", "tok"))
	tr.stepStream("events after a restart, later", fqGet("/events?after=5", "tok"))
	return tr.String()
}

// fqScenarioProbeDuringTransaction: a large transaction is in flight on
// one connection; meanwhile the banner, a read and /stats are answered
// within a bound — the engine's writer is one task among several, not
// the server (Rust's writer mutex blocks writers, never a probe or a
// snapshot read). The transcript says whether each probe made it in time
// and what it answered; the transaction's own answer follows.
func fqScenarioProbeDuringTransaction(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	l.ok(fqSend("POST", "/node", "tok", fqNode("seed:1", "Seed", `{"n":1}`, `,"public":true`)))
	var ops []string
	for i := 0; i < 600; i++ {
		ops = append(ops, fqInsertOp(fmt.Sprintf("Big:%04d", i), "Big", fmt.Sprintf(`{"i":%d}`, i), `,"public":true`))
	}
	tx := fqSend("POST", "/transaction", "tok", fqTx(ops...))
	c, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(tx.raw())); err != nil {
		t.Fatal(err)
	}
	// The transaction is running once its first bytes are read; give the
	// server a moment to be inside it, then probe.
	time.Sleep(150 * time.Millisecond)
	tr := &fqTranscript{l: l}
	const budget = 2 * time.Second
	for _, p := range []struct {
		label string
		c     fqCase
	}{
		{"banner", fqGet("/", "")},
		{"read", fqGet("/node/seed:1", "tok")},
		{"list", fqGet("/nodes?kind=Seed", "utok")},
		{"stats", fqGet("/stats", "tok")},
	} {
		t0 := time.Now()
		resp := l.raw(p.c)
		took := time.Since(t0)
		status := 0
		fmt.Sscanf(resp, "HTTP/1.1 %d", &status)
		tr.note("%s during the transaction: status %d, within %v: %v", p.label, status, budget, took < budget)
	}
	// Then the transaction's own answer, read to its end.
	_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
	var out []byte
	buf := make([]byte, 65536)
	for {
		i := strings.Index(string(out), "\r\n\r\n")
		if i >= 0 {
			m := regexp.MustCompile(`content-length: (\d+)`).FindStringSubmatch(strings.ToLower(string(out[:i])))
			if m != nil {
				var n int
				fmt.Sscan(m[1], &n)
				if len(out)-(i+4) >= n {
					break
				}
			}
		}
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	tr.note("transaction\n%s", fqNorm(string(out)))
	tr.step("all there", fqSend("POST", "/nodes/count", "tok", `{"kind":"Big"}`))
	return tr.String()
}

// fqScenarioSplitsAndPaging drives the B+tree through many leaf and branch
// splits (1,200 inserts in 20-op transactions over an indexed field),
// cell replacements of a different size (overwrites), removals (deletes)
// and so compaction, then reads every access path the result is visible
// through — ordered pages walked by cursor, counts, a grouped aggregate —
// before and after a reopen.
func fqScenarioSplitsAndPaging(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	l.ok(fqSend("POST", "/admin/indexes", "tok", `{"name":"sp_score","kind":"Sp","field":"score"}`))
	for b := 0; b < 60; b++ {
		var ops []string
		for i := 0; i < 20; i++ {
			n := b*20 + i
			ops = append(ops, fqInsertOp(fmt.Sprintf("Sp:%05d", (n*7919)%1200), "Sp", fmt.Sprintf(`{"score":%d,"pad":"%s"}`, (n*31)%97, strings.Repeat("p", n%40)), `,"public":true`))
		}
		l.ok(fqSend("POST", "/transaction", "tok", fqTx(ops...)))
	}
	var ops []string
	for i := 0; i < 1200; i += 3 {
		ops = append(ops, fqInsertOp(fmt.Sprintf("Sp:%05d", i), "Sp", fmt.Sprintf(`{"score":%d,"pad":"%s"}`, i%13, strings.Repeat("q", 60)), `,"public":true`))
		if len(ops) == 20 {
			l.ok(fqSend("POST", "/transaction", "tok", fqTx(ops...)))
			ops = nil
		}
	}
	if len(ops) > 0 {
		l.ok(fqSend("POST", "/transaction", "tok", fqTx(ops...)))
	}
	ops = nil
	for i := 1; i < 1200; i += 4 {
		ops = append(ops, fmt.Sprintf(`{"type":"delete_node","address":"Sp:%05d"}`, i))
		if len(ops) == 20 {
			l.ok(fqSend("POST", "/transaction", "tok", fqTx(ops...)))
			ops = nil
		}
	}
	if len(ops) > 0 {
		l.ok(fqSend("POST", "/transaction", "tok", fqTx(ops...)))
	}
	tr := &fqTranscript{l: l}
	read := func(label string) {
		tr.step(label+" count", fqSend("POST", "/nodes/count", "tok", `{"kind":"Sp"}`))
		tr.step(label+" grouped", fqSend("POST", "/nodes/count_by", "tok", `{"kind":"Sp","group_by":"score","values":[0,5,12,50,96]}`))
		for _, order := range []string{`"order":"score","desc":true`, `"order":"score"`, `"order":"pad"`} {
			after := ""
			for page := 0; page < 6; page++ {
				body := `{"kind":"Sp",` + order + `,"limit":37`
				if after != "" {
					body += `,"after":` + after
				}
				resp := l.body(fqSend("POST", "/nodes/query", "tok", body+`}`))
				var m map[string]any
				_ = json.Unmarshal([]byte(resp), &m)
				var addrs []string
				nodes, _ := m["nodes"].([]any)
				for _, n := range nodes {
					addrs = append(addrs, fmt.Sprint(n.(map[string]any)["address"]))
				}
				tr.note("%s %s page %d: %s", label, order, page, strings.Join(addrs, ","))
				nx, ok := m["next"]
				if !ok || nx == nil {
					break
				}
				b, _ := json.Marshal(nx)
				after = string(b)
			}
		}
		tr.step(label+" by address", fqGet("/nodes?kind=Sp&limit=25&offset=400", "tok"))
	}
	read("live")
	l.restart()
	read("reopened")
	return tr.String()
}

// fqScenarioTelemetryWhileSaturated: with every in-flight slot held (a
// write whose body never finishes arriving), ordinary work is refused with
// the concurrency 503 while `/stats` and the `/` liveness probe still
// answer — telemetry is not admitted work (limits.rs's is_telemetry), so a
// supervisor can see the instance it most needs to see.
func fqScenarioTelemetryWhileSaturated(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), "FACETQL_MAX_CONCURRENT_REQUESTS=1", "FACETQL_REQUEST_TIMEOUT_SECS=30")
	c, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("POST /node HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"address\":")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	tr := &fqTranscript{l: l}
	tr.step("work while saturated", fqGet("/nodes", "tok"))
	tr.step("liveness probe while saturated", fqGet("/", ""))
	tr.step("HEAD probe while saturated", fqCase{method: "HEAD", path: "/"})
	raw := l.raw(fqGet("/stats", "tok"))
	status := 0
	fmt.Sscanf(raw, "HTTP/1.1 %d", &status)
	var m map[string]any
	if i := strings.Index(raw, "\r\n\r\n"); i >= 0 {
		_ = json.Unmarshal([]byte(raw[i+4:]), &m)
	}
	rt, _ := m["runtime"].(map[string]any)
	req, _ := rt["requests"].(map[string]any)
	tr.note("stats while saturated: status %d, in_flight %v, max_concurrent %v", status, req["in_flight"], req["max_concurrent"])
	tr.step("a POST to / is not telemetry", fqSend("POST", "/", "tok", "{}"))
	return tr.String()
}

// fqScenarioNonHTTPStart: bytes that cannot begin a request are answered
// as they arrive — hyper (httparse) parses a head incrementally, so a TLS
// ClientHello sent to a plain listener (it has no blank line to wait for)
// gets hyper's bare 400 and a close at once, and the client fails fast
// instead of both sides waiting on each other. A valid head arriving in
// pieces is still waited for.
func fqScenarioNonHTTPStart(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	send := func(label string, pieces ...string) {
		c, err := fqDial(l.port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i, p := range pieces {
			if i > 0 {
				time.Sleep(300 * time.Millisecond)
			}
			if _, err := c.Write([]byte(p)); err != nil {
				t.Fatal(err)
			}
		}
		var out []byte
		buf := make([]byte, 4096)
		closed := false
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				closed = err == io.EOF || !strings.Contains(err.Error(), "timeout")
				break
			}
		}
		tr.note("%s: closed=%v\n%s", label, closed, fqNorm(string(out)))
	}
	// A TLS 1.2/1.3 ClientHello's record header and handshake header.
	send("tls ClientHello to a plain listener", "\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03")
	send("a method byte that is no token character", "GE(T / HTTP/1.1\r\nHost: x")
	send("a valid head arriving in pieces", "GET / HT", "TP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	return tr.String()
}

// fqScenarioShutdownUnderLoad: SIGTERM while work is in flight — a write
// whose body is still arriving, a keep-alive connection sitting idle after
// an answered request, and a burst of writers before it. The write in
// flight is finished and answered; the idle connection is not served
// again; a new connection finds nothing listening; the process checkpoints,
// says so and exits 0; and everything acknowledged is there on the next
// start.
func fqScenarioShutdownUnderLoad(t *testing.T, which string) string {
	dir := t.TempDir()
	l := fqBoot(t, which, dir)
	tr := &fqTranscript{l: l}
	body := fqNode("load:slow", "L", `{"v":1}`, "")
	slow, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	head := fmt.Sprintf("POST /node HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
	if _, err := slow.Write([]byte(head + body[:10])); err != nil {
		t.Fatal(err)
	}
	idle, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	readAnswer := func(c net.Conn) string {
		var out []byte
		buf := make([]byte, 65536)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if i := strings.Index(string(out), "\r\n\r\n"); i >= 0 {
				m := regexp.MustCompile(`(?i)content-length: (\d+)`).FindStringSubmatch(string(out[:i]))
				if m != nil {
					var want int
					fmt.Sscan(m[1], &want)
					if len(out)-(i+4) >= want {
						return fqNorm(string(out))
					}
				}
			}
			if err != nil {
				if len(out) == 0 {
					return "closed without an answer"
				}
				return fqNorm(string(out))
			}
		}
	}
	if _, err := idle.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	tr.note("idle connection's first answer\n%s", readAnswer(idle))
	var wg sync.WaitGroup
	burst := make([]string, 8)
	for i := range burst {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			burst[i] = fqNorm(fqRaw(t, l.port, fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("load:%d", i), "L", `{"v":1}`, "")).raw(), 0))
		}(i)
	}
	wg.Wait()
	created := 0
	for _, b := range burst {
		if strings.HasPrefix(b, "HTTP/1.1 201") {
			created++
		}
	}
	tr.note("burst: %d of %d created", created, len(burst))
	time.Sleep(200 * time.Millisecond)
	if err := l.p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", l.port), time.Second); err != nil {
		tr.note("a new connection while draining: refused")
	} else {
		_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		tr.note("a new connection while draining: accepted, %s", readAnswer(c))
		c.Close()
	}
	if _, err := idle.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		tr.note("the idle connection while draining: write failed")
	} else {
		tr.note("the idle connection while draining: %s", readAnswer(idle))
	}
	if _, err := slow.Write([]byte(body[10:])); err != nil {
		t.Fatal(err)
	}
	tr.note("the write in flight\n%s", readAnswer(slow))
	code := l.p.fqExit(t)
	var lines []string
	for _, ln := range strings.Split(l.p.stdout.String()+l.p.stderr.String(), "\n") {
		if strings.HasPrefix(ln, "FacetQL: ") {
			lines = append(lines, ln)
		}
	}
	tr.note("exit %d\n%s", code, strings.Join(lines, "\n"))
	l.start()
	tr.step("the write in flight, after a restart", fqGet("/node/load:slow", "tok"))
	tr.step("the burst, after a restart", fqGet("/nodes?kind=L&limit=100", "tok"))
	return tr.String()
}

// fqScenarioShutdownHeldBySubscriber: a live event stream never ends by
// itself, so the drain is on a clock (main.rs's SHUTDOWN_GRACE): SIGTERM
// with a subscriber attached waits the fifteen seconds, says the
// connection is still open, then checkpoints and exits 0.
func fqScenarioShutdownHeldBySubscriber(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	sub, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := sub.Write([]byte("GET /events HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	t0 := time.Now()
	if err := l.p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code := l.p.fqExit(t)
	took := time.Since(t0)
	window := "outside 14-17s"
	if took >= 14*time.Second && took <= 17*time.Second {
		window = "within 14-17s"
	}
	var lines []string
	for _, ln := range strings.Split(l.p.stdout.String()+l.p.stderr.String(), "\n") {
		if strings.HasPrefix(ln, "FacetQL: ") {
			lines = append(lines, ln)
		}
	}
	sort.Strings(lines)
	tr.note("exit %d, %s of the signal\n%s", code, window, strings.Join(lines, "\n"))
	return tr.String()
}

// fqProbeBound is what a liveness or /stats probe is held to while the
// engine works: the budget a supervisor gives it.
const fqProbeBound = 500 * time.Millisecond

// fqScenarioProbesUnderLoad: the `/` liveness probe and `/stats` answer
// within fqProbeBound while the engine is busy — writers committing small
// transactions back to back, a mover's large batches (500 inserts a
// transaction) with its subscription, change-log paging and multigets, and
// a crowd of admin reads the loop itself runs — for every probe, not on
// average: telemetry must not queue behind the work it reports on. (A
// fabric daemon ages every node's heartbeat by the time one /stats poll
// takes: a poll slower than its silence budget reads as a lost node.)
func fqScenarioProbesUnderLoad(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir(), os.Getenv("FQ_PROBE_EXTRA_ENV"))
	tr := &fqTranscript{l: l}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := map[string]int{}
	work := func(name string, ops func(i int) []string) {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			r := fqRaw(t, l.port, fqSend("POST", "/transaction", "tok", fqTx(ops(i)...)).raw(), 0)
			if !strings.HasPrefix(r, "HTTP/1.1 200") {
				mu.Lock()
				failures[name]++
				mu.Unlock()
			}
		}
	}
	for w := 0; w < 4; w++ {
		w := w
		wg.Add(1)
		go work("writer", func(i int) []string {
			var ops []string
			for k := 0; k < 20; k++ {
				ops = append(ops, fqInsertOp(fmt.Sprintf("w%d:%d:%d", w, i, k), "W", `{"v":1}`, ""))
			}
			return ops
		})
	}
	wg.Add(1)
	go work("mover", func(i int) []string {
		var ops []string
		for k := 0; k < 500; k++ {
			ops = append(ops, fqInsertOp(fmt.Sprintf("m%d:%d", i, k), "M", `{"payload":"`+strings.Repeat("x", 200)+`"}`, ""))
		}
		return ops
	})
	// the mover's other traffic: a live subscription, change-log paging
	// and multigets of what it copies
	sub, err := fqDial(l.port)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if _, err := sub.Write([]byte("GET /events HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 65536)
		for {
			if _, err := sub.Read(buf); err != nil {
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = fqRaw(t, l.port, fqGet(fmt.Sprintf("/changes?after=%d&limit=500", i*50), "tok").raw(), 0)
			var addrs []string
			for k := 0; k < 200; k++ {
				addrs = append(addrs, fmt.Sprintf("%q", fmt.Sprintf("m%d:%d", i%10, k)))
			}
			_ = fqRaw(t, l.port, fqSend("POST", "/nodes/multiget", "tok", `{"addresses":[`+strings.Join(addrs, ",")+`]}`).raw(), 0)
		}
	}()
	// and a crowd of reads the loop itself runs (the admin listings), so
	// its ordinary inbox always holds a backlog
	for r := 0; r < 16; r++ {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths := []string{"/admin/indexes", "/admin/users", "/admin/references"}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = fqRaw(t, l.port, fqGet(paths[(i+r)%3], "tok").raw(), 0)
			}
		}()
	}
	time.Sleep(time.Second)
	probe := func(c fqCase) (time.Duration, bool) {
		t0 := time.Now()
		r := fqRaw(t, l.port, c.raw(), 0)
		return time.Since(t0), strings.HasPrefix(r, "HTTP/1.1 200")
	}
	var worstHome, worstStats time.Duration
	homeOver, statsOver, bad, n := 0, 0, 0, 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, ok := probe(fqGet("/", ""))
		if !ok {
			bad++
		}
		if d > worstHome {
			worstHome = d
		}
		if d > fqProbeBound {
			homeOver++
		}
		d2, ok2 := probe(fqGet("/stats", "tok"))
		if !ok2 {
			bad++
		}
		if d2 > worstStats {
			worstStats = d2
		}
		if d2 > fqProbeBound {
			statsOver++
		}
		n++
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	t.Logf("%s: %d probe pairs; worst / %v, worst /stats %v; over %v: / %d, /stats %d; failed transactions %v",
		which, n, worstHome, worstStats, fqProbeBound, homeOver, statsOver, failures)
	tr.note("probes answered 200: %v; every / within %v: %v; every /stats within %v: %v; the work committed: %v",
		bad == 0, fqProbeBound, homeOver == 0, fqProbeBound, statsOver == 0, len(failures) == 0)
	return tr.String()
}

// fqLowered is a predicate testing op(lower(item.field), lit).
func fqLowered(op, field, lit string) string {
	l, _ := json.Marshal(lit)
	return fmt.Sprintf(`{"kind":"bin","op":%q,"l":{"kind":"call","name":"lower","args":[{"kind":"get","field":%q,"obj":{"kind":"ref","name":"item"}}]},"r":{"kind":"lit","val":%s}}`, op, field, l)
}

// fqScenarioFoldedTextIndex: a folded inverted index — the one a
// case-insensitive search uses — as the admin surface declares, lists and
// refuses it; the lowered searches it serves, answered the same with and
// without it, over text whose lowercase is not ASCII folding; `lower` of
// other values and its refusals; and the index surviving a crash before any
// checkpoint, still folded and still serving.
func fqScenarioFoldedTextIndex(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	tr := &fqTranscript{l: l}
	bodies := []string{"Hello World", "HELLO WORLD", "ÉCOLE Normale", "école normale", "İSTANBUL nights", "istanbul NIGHTS", "\u212aELVIN scale", "ΟΔΟΣ", "abcxbcd"}
	for i, b := range bodies {
		d, _ := json.Marshal(map[string]any{"body": b, "n": i, "flag": i%2 == 0})
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Fold:%02d", i), "Fold", string(d), `,"public":true`)))
	}
	for i := 0; i < 60; i++ {
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Fold:hay%02d", i), "Fold", fmt.Sprintf(`{"body":"ordinary post %d"}`, i), `,"public":true`)))
	}
	queries := []struct{ label, where string }{
		{"contains hello", fqLowered("contains", "body", "hello")},
		{"contains école", fqLowered("contains", "body", "école")},
		{"contains istanbul", fqLowered("contains", "body", "istanbul")},
		{"contains kelvin", fqLowered("contains", "body", "kelvin")},
		{"contains οδοσ", fqLowered("contains", "body", "οδοσ")},
		{"contains abcd", fqLowered("contains", "body", "abcd")},
		{"contains HELLO", fqLowered("contains", "body", "HELLO")},
		{"contains zz", fqLowered("contains", "body", "zz")},
		{"starts_with école", fqLowered("starts_with", "body", "école")},
		{"ends_with scale", fqLowered("ends_with", "body", "scale")},
		{"lower of a number", fqLowered("contains", "n", "7")},
		{"lower of a bool", fqLowered("starts_with", "flag", "tru")},
		{"lower of an absent field", `{"kind":"bin","op":"==","l":{"kind":"call","name":"lower","args":[{"kind":"get","field":"none","obj":{"kind":"ref","name":"item"}}]},"r":{"kind":"lit","val":""}}`},
		{"lower with two arguments", `{"kind":"bin","op":"==","l":{"kind":"call","name":"lower","args":[{"kind":"lit","val":"a"},{"kind":"lit","val":"b"}]},"r":{"kind":"lit","val":"a"}}`},
		{"a call to upper", `{"kind":"call","name":"upper","args":[{"kind":"lit","val":"a"}]}`},
		{"a folded search beside a bare one", `{"kind":"bin","op":"&&","l":` + fqLowered("contains", "body", "hello") + `,"r":{"kind":"bin","op":"contains","l":{"kind":"get","field":"body","obj":{"kind":"ref","name":"item"}},"r":{"kind":"lit","val":"HELLO"}}}`},
	}
	ask := func(phase string) {
		for _, q := range queries {
			tr.step(phase+": "+q.label, fqSend("POST", "/nodes/query", "tok", `{"kind":"Fold","where":`+q.where+`,"limit":100}`))
		}
	}
	ask("no index")
	tr.step("declare folded", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_body","kind":"Fold","field":"body","mode":"folded"}`))
	tr.step("declare folded again", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_body","kind":"Fold","field":"body","mode":"folded"}`))
	tr.step("the same name, unfolded", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_body","kind":"Fold","field":"body","mode":"text"}`))
	tr.step("a second folded over the field", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_body2","kind":"Fold","field":"body","mode":"FOLDED"}`))
	tr.step("an unfolded one beside it", fqSend("POST", "/admin/indexes", "tok", `{"name":"text_body","kind":"Fold","field":"body","mode":"text"}`))
	tr.step("a unique folded", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_u","kind":"Fold","field":"n","mode":"folded","unique":true}`))
	tr.step("an unknown mode", fqSend("POST", "/admin/indexes", "tok", `{"name":"fold_x","kind":"Fold","field":"n","mode":"fuzzy"}`))
	tr.step("listed", fqGet("/admin/indexes", "tok"))
	ask("folded")
	// written after the index: maintained, and a crash before any
	// checkpoint must bring the index back — folded — from the logs
	l.ok(fqSend("POST", "/node", "tok", fqNode("Fold:late", "Fold", `{"body":"Late HELLO from İzmir"}`, `,"public":true`)))
	l.kill()
	l.start()
	tr.step("listed after a crash", fqGet("/admin/indexes", "tok"))
	ask("after a crash")
	tr.step("izmir after a crash", fqSend("POST", "/nodes/query", "tok", `{"kind":"Fold","where":`+fqLowered("contains", "body", "izmir")+`,"limit":10}`))
	tr.step("drop folded", fqSend("DELETE", "/admin/indexes/fold_body", "tok", ""))
	tr.step("listed after the drop", fqGet("/admin/indexes", "tok"))
	return tr.String()
}

// fqScenarioLowerIsGosLower: `lower` in a predicate is the fct language's
// lower — Go's strings.ToLower — for every code point it changes: nodes
// hold runs of all of them, and each matches `lower(item.t) == "<what Go
// makes of it>"` and not the unlowered text.
func fqScenarioLowerIsGosLower(t *testing.T, which string) string {
	l := fqBoot(t, which, t.TempDir())
	var upper []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if strings.ToLower(string(r)) != string(r) {
			upper = append(upper, r)
		}
	}
	var clauses []string
	for i := 0; i < len(upper); i += 64 {
		end := min(i+64, len(upper))
		run := string(upper[i:end]) + " Mixed ΣΑΣ tail"
		d, _ := json.Marshal(map[string]any{"t": run})
		l.ok(fqSend("POST", "/node", "tok", fqNode(fmt.Sprintf("Low:%03d", i/64), "Low", string(d), `,"public":true`)))
		want, _ := json.Marshal(strings.ToLower(run))
		clauses = append(clauses, fmt.Sprintf(`{"kind":"bin","op":"==","l":{"kind":"call","name":"lower","args":[{"kind":"get","field":"t","obj":{"kind":"ref","name":"item"}}]},"r":{"kind":"lit","val":%s}}`, want))
	}
	where := clauses[0]
	for _, c := range clauses[1:] {
		where = `{"kind":"bin","op":"||","l":` + where + `,"r":` + c + `}`
	}
	tr := &fqTranscript{l: l}
	tr.note("%d code points lowered by Go, in %d nodes", len(upper), len(clauses))
	tr.step("every node matches its Go-lowered text", fqSend("POST", "/nodes/count", "tok", `{"kind":"Low","where":`+where+`}`))
	return tr.String()
}

// ── the table ──────────────────────────────────────────────────────────

func fqContractRows() []fqContractRow {
	both := "TestFqServerBothWays"
	return []fqContractRow{
		// routing, auth, limits — api/routes.rs, auth.rs, api/limits.rs
		{area: "http", name: "GET / answers the banner without auth", source: "routes.rs:home", provedBy: both},
		{area: "http", name: "unknown path is 404 before auth; wrong method is 405 with allow", source: "routes.rs:create_router", provedBy: both},
		{area: "http", name: "x-api-key or ?key= resolves an identity; a bad key is 401", source: "auth.rs:auth_middleware", provedBy: both},
		{area: "http", name: "/admin/* needs an admin identity (403)", source: "routes.rs:require_admin", provedBy: both},
		{area: "http", name: "HEAD answers as GET without a body", source: "routes.rs", provedBy: both},
		{area: "http", name: "CORS preflight in the development posture is permissive", source: "routes.rs:cors_layer", provedBy: both},
		{area: "http", name: "CORS allowlist in the production posture", source: "routes.rs:cors_layer", run: fqScenarioCorsAllowlist},
		{area: "http", name: "axum's JSON extractor rejections, serde's messages included", source: "routes.rs (Json extractor)", provedBy: both},
		{area: "http", name: "per-identity token buckets answer 429 with Retry-After", source: "limits.rs:rate_limit", provedBy: "TestFqServerLimitsBothWays"},
		{area: "http", name: "body limit answers 413 only when a handler reads the body", source: "limits.rs:body_limit_layer", provedBy: "TestFqServerLimitsBothWays"},
		{area: "http", name: "in-flight cap answers 503; the request timeout 408", source: "limits.rs:concurrency, request_timeout_layer", provedBy: "TestFqServerInFlightAndTimeoutBothWays"},
		{area: "http", name: "/stats and the liveness probe answer while every in-flight slot is held", source: "limits.rs:concurrency, is_telemetry", run: fqScenarioTelemetryWhileSaturated},
		{area: "http", name: "bytes that cannot start a request (a ClientHello on a plain port) are refused as they arrive", source: "hyper h1 (httparse) incremental head parse", run: fqScenarioNonHTTPStart},
		{area: "http", name: "live subscribers are capped (503)", source: "limits.rs:subscriber_permit", run: fqScenarioMaxSubscribers},
		{area: "http", name: "a probe, a read and /stats are answered while a large transaction is in flight", source: "database.rs:with_engine_mut (the writer mutex), engine.rs:pin_read", run: fqScenarioProbeDuringTransaction},
		{area: "http", name: "every liveness probe and /stats answers within 500ms while writers and a mover's batches run", source: "limits.rs:is_telemetry; the supervisor's probe budget", run: fqScenarioProbesUnderLoad},
		{area: "http", name: "TLS from a PKCS#12 identity", source: "tls_server.rs", provedBy: "TestFqServerTLSBothWays"},
		{area: "http", name: "an idle event stream carries the keep-alive comment", source: "routes.rs:subscribe_events", provedBy: "TestFqServerStreamKeepAliveBothWays"},
		{area: "http", name: "/stats: the engine half identical, the runtime half in shape", source: "routes.rs:stats, metrics.rs", provedBy: both},
		{area: "http", name: "requests are classified read/write/bulk/admin for metrics", source: "metrics.rs:classify", provedBy: both},

		// endpoints — api/routes.rs handlers
		{area: "nodes", name: "POST /node creates, upserts, honours if_absent, public and edges", source: "routes.rs:create_node", provedBy: both},
		{area: "nodes", name: "GET/PUT/DELETE /node/:address with ownership and visibility", source: "routes.rs:get_node, update_node, delete_node", provedBy: both},
		{area: "nodes", name: "GET /node/:address/history archives every overwrite", source: "routes.rs:get_node_history", provedBy: both},
		{area: "nodes", name: "GET /node/:address/owned and POST /node/:address/claim", source: "routes.rs:list_owned, claim_node", provedBy: both},
		{area: "nodes", name: "GET /nodes filters by kind/owner, pages, refuses bad limits", source: "routes.rs:query_nodes", provedBy: both},
		{area: "nodes", name: "POST /nodes/multiget: order asked, absent skipped, visibility, bound", source: "engine.rs:multi_get", run: fqScenarioMultiGet},
		{area: "nodes", name: "POST /nodes/query with where/order/desc/limit/offset", source: "routes.rs:query_nodes_where", provedBy: both},
		{area: "nodes", name: "keyset cursor paging (after/next), with and without an index", source: "engine.rs:query_where, MAX_CURSOR_LEN", run: fqScenarioKeysetPaging},
		{area: "nodes", name: "every predicate operator, its refusals and its bounds", source: "core/predicate.rs", run: fqScenarioPredicates},
		{area: "nodes", name: "POST /nodes/count, count_by, aggregate, aggregate_by", source: "routes.rs:count_nodes …", provedBy: both},
		{area: "nodes", name: "count_by/aggregate_by values, every func, the empty cases, the bound", source: "engine.rs:count_by, aggregate_by, MAX_GROUP_VALUES", run: fqScenarioGrouping},
		{area: "nodes", name: "POST /sequence/:name/next blocks, ownership, bounds", source: "engine.rs:sequence_next", run: fqScenarioSequences},
		{area: "edges", name: "POST/DELETE /edge and GET /node/:address/edges/{in,out}", source: "routes.rs:create_edge …", provedBy: both},
		{area: "tx", name: "POST /transaction: every op, ownership stamping, serde precedence, sequence forms", source: "routes.rs:execute_transaction, TxOpRequest", provedBy: both},
		{area: "tx", name: "a cascade too large to stage atomically is refused and applies nothing", source: "engine.rs:MAX_TRANSACTION_OPS", run: fqScenarioCascadeBound},
		{area: "feed", name: "GET /events: audience filtering, SSE framing, resume, 410", source: "routes.rs:subscribe_events, database.rs:feed", provedBy: both},
		{area: "feed", name: "GET /changes and POST /publish: paging, bounds, audience, across a restart", source: "routes.rs:scan_changes, publish_event", run: fqScenarioEventsAndChanges},
		{area: "admin", name: "users: create, list, revoke, roles, refusals", source: "routes.rs:create_user …", provedBy: both},
		{area: "admin", name: "persistent users: minted tokens authenticate, survive a restart, revocation sticks", source: "engine.rs:insert_user, revoke_user, find_user_by_hash", run: fqScenarioPersistentUsers},
		{area: "admin", name: "indexes: create (hash/text/unique), list, drop, refusals", source: "routes.rs:create_index …", provedBy: both},
		{area: "admin", name: "folded text indexes: declared, listed, refused, survive a crash, serve op(lower(item.f), …) as the scan does", source: "text.rs (TextIndexDef::folded, PutFolded), wal.rs (CreateFoldedTextIndex), predicate.rs (lower, lowered_substring_literals)", run: fqScenarioFoldedTextIndex},
		{area: "nodes", name: "lower in a predicate is the fct language's lower (Go's strings.ToLower), every code point", source: "predicate.rs:go_lower, go_lower_table.rs", run: fqScenarioLowerIsGosLower},
		{area: "admin", name: "unique indexes: duplicates refused, moves in a batch, declared over duplicates refused, survive a reopen", source: "tests/unique_constraint.rs", run: fqScenarioUniqueIndex},
		{area: "admin", name: "references: cascade/restrict/set_null, cycles, missing parents, needed indexes, by parent field, across a restart", source: "tests/referential_integrity.rs, reference_restart.rs", run: fqScenarioReferences},

		// durability — storage/{wal,recovery,checkpoint,lock}.rs, tests/crash_recovery.rs
		{area: "durability", name: "every acknowledged write survives a SIGKILL", source: "tests/crash_recovery.rs", run: fqScenarioCrashAcknowledged},
		{area: "durability", name: "a transaction is all-or-nothing across a crash", source: "tests/crash_recovery.rs", run: fqScenarioCrashAtomic},
		{area: "durability", name: "repeated crashes never lose a confirmed write", source: "tests/crash_recovery.rs", run: fqScenarioCrashRepeated},
		{area: "durability", name: "a clean stop checkpoints and everything is there on the next start", source: "main.rs (shutdown), tests/crash_recovery.rs", run: fqScenarioCleanRestart},
		{area: "durability", name: "SIGTERM: in-flight finishes, the last checkpoint, exit 0, the same words", source: "main.rs", provedBy: "TestFqServerShutdownBothWays"},
		{area: "durability", name: "SIGTERM under load: the write in flight is answered, an idle keep-alive is not served again, nothing new is accepted, all of it durable", source: "main.rs (with_graceful_shutdown, SHUTDOWN_GRACE)", run: fqScenarioShutdownUnderLoad},
		{area: "durability", name: "SIGTERM with a live subscriber: the drain waits SHUTDOWN_GRACE, says so, checkpoints, exits 0", source: "main.rs (SHUTDOWN_GRACE)", run: fqScenarioShutdownHeldBySubscriber},
		// the operator CLI — main.rs, cli/mod.rs, cli/client.rs, cli/output.rs
		{area: "cli", name: "the command line: help screens, --version, clap's usage errors, argument checks, the token, the confirmation prompt, ENOCHIAN_* warnings", source: "main.rs (Cli), cli/mod.rs", provedBy: "TestFqCliCommandLineBothWays"},
		{area: "cli", name: "init, backup and restore of a data directory", source: "main.rs:run_backup, run_restore, data_files", provedBy: "TestFqCliDataDirectoryBothWays"},
		{area: "cli", name: "user, index, reference, get, put, delete, query, stats and routes against a running server", source: "cli/mod.rs, cli/client.rs, cli/output.rs", provedBy: "TestFqCliClientBothWays"},
		{area: "cli", name: "no command is `start`, with start's flags and environment", source: "main.rs:bare_start", provedBy: "TestFqCliCommandLineBothWays"},
		{area: "durability", name: "a frame whose COMMIT never landed leaves every structure as it was", source: "storage/recovery.rs, tests/crash_recovery.rs", run: fqScenarioCommitDropped},
		{area: "durability", name: "the same frame with its COMMIT replays all of it", source: "storage/recovery.rs", run: fqScenarioCommitReplayed},
		{area: "durability", name: "a committed frame missing its BEGIN refuses to start", source: "storage/recovery.rs, main.rs:report_startup_failure", run: fqScenarioNoBegin},
		{area: "durability", name: "the data directory admits only one process", source: "storage/lock.rs", run: fqScenarioSingleProcess},
		{area: "durability", name: "a checkpoint on every write: indexes, sequences, history and edges reopen", source: "storage/checkpoint.rs, tests/sequences.rs", run: fqScenarioCheckpointEveryWrite},
		{area: "durability", name: "WAL rotation keeps every record and moves the /changes horizon", source: "storage/wal.rs (rotate), storage/changes.rs", run: fqScenarioWalRotation},
		{area: "storage", name: "many splits, resized overwrites and deletes read back identically on every path, live and reopened", source: "storage/btree.rs, page.rs, pager.rs, engine.rs:query_sorted", run: fqScenarioSplitsAndPaging},
		{area: "durability", name: "records larger than a page survive the overflow chain and a reopen", source: "storage/heap.rs, tests/heap_records.rs", run: fqScenarioOverflowRecords},
		{area: "durability", name: "a data directory written by facetql opens under fqserver.fct", source: "storage/*.rs formats", cross: "rust"},
		{area: "durability", name: "a data directory written by fqserver.fct opens under facetql", source: "storage/*.rs formats", cross: "fct"},
		{area: "durability", name: "a wrong master key or a corrupt record refuses to start (exit 4); the posture refusal (exit 7)", source: "main.rs:report_startup_failure", provedBy: "TestFqServerRefusalsBothWays"},

		// the engine's parts, byte for byte — storage/*.rs
		{area: "storage", name: "WAL frames encrypt and decode bit-exactly", source: "storage/wal.rs, crypto.rs", provedBy: "TestWalGoldenFramesRoundTripBitExact"},
		{area: "storage", name: "WAL recovery classification", source: "storage/recovery.rs", provedBy: "TestWalRecoveryContract"},
		{area: "storage", name: "AES-GCM matches", source: "crypto.rs", provedBy: "TestAesGcmMatchesGo"},
		{area: "storage", name: "pages encode and decode as Rust's", source: "storage/page.rs", provedBy: "TestPageScriptsMatchRust"},
		{area: "storage", name: "the pager reads Rust's files and Rust reads its", source: "storage/pager.rs", provedBy: "TestPagerReadsRustFile"},
		{area: "storage", name: "Rust reads fct-written pages", source: "storage/pager.rs", provedBy: "TestRustReadsFctPages"},
		{area: "storage", name: "the checkpoint file contract", source: "storage/checkpoint.rs", provedBy: "TestCheckpointContract"},
		{area: "storage", name: "record frames", source: "storage/binary.rs", provedBy: "TestRecordFrame"},
		{area: "storage", name: "the users log", source: "storage/engine.rs (users)", provedBy: "TestUsersLog"},
		{area: "storage", name: "the catalog", source: "storage/catalog.rs", provedBy: "TestCatalogBothWays"},
		{area: "storage", name: "the B+tree", source: "storage/btree.rs", provedBy: "TestFqBTreeBothWays"},
		{area: "storage", name: "the heap", source: "storage/heap.rs", provedBy: "TestFqHeapBothWays"},
		{area: "storage", name: "the JSON reader/writer", source: "serde_json as facetql uses it", provedBy: "TestFqJsonBothWays"},
		{area: "storage", name: "secondary and text indexes", source: "storage/index.rs, text.rs", provedBy: "TestFqIndexBothWays"},
		{area: "storage", name: "the engine over its files", source: "storage/engine.rs", provedBy: "TestFqEngineBothWays"},
		{area: "storage", name: "the query planner and evaluator", source: "storage/engine.rs:query_where", provedBy: "TestFqQueryBothWays"},
		{area: "storage", name: "transactions and staging", source: "storage/transaction.rs, commit.rs", provedBy: "TestFqTxBothWays"},
		{area: "storage", name: "staged-frame guards", source: "storage/commit.rs", provedBy: "TestCommitStagedFrameGuards"},
		{area: "storage", name: "engine stats", source: "storage/engine.rs:stats", provedBy: "TestFqEngineReportBothWays"},
		{area: "storage", name: "the event feed ring: positions, resume, horizon, audience", source: "database.rs", provedBy: "TestDatabaseEventFeedResumeBeforeHorizonIsRefused"},

		// the whole application suite
		{area: "suite", name: "the integration suite passes against the fct engine", source: "fct/integration", provedBy: "TestSuiteRunsTheShippedEngine"},
	}
}

// fqTestExists reports whether a test function is defined in this package
// or in ../integration.
func fqTestExists(t *testing.T, name string) bool {
	t.Helper()
	for _, dir := range []string{".", "../integration"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		for _, f := range files {
			src, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if strings.Contains(string(src), "\nfunc "+name+"(t *testing.T)") {
				return true
			}
		}
	}
	return false
}

func TestFqContract(t *testing.T) {
	rows := fqContractRows()
	seen := map[string]bool{}
	for _, row := range rows {
		row := row
		key := row.area + ": " + row.name
		if seen[key] {
			t.Fatalf("duplicate row %q", key)
		}
		seen[key] = true
		t.Run(row.area+"/"+row.name, func(t *testing.T) {
			if row.notPorted != "" {
				t.Skip("not ported: " + row.notPorted)
			}
			switch {
			case row.provedBy != "":
				if !fqTestExists(t, row.provedBy) {
					t.Fatalf("proved by %s, which no longer exists", row.provedBy)
				}
			case row.cross != "":
				fqCrossEngine(t, row.cross)
			case row.run != nil:
				fqBothWays(t, row.run)
			default:
				t.Fatalf("row %q has no proof", key)
			}
		})
	}
}
