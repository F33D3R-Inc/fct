package selfhost

import (
	"bufio"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// Conformance tests for the leaf-B fabric ports: fabric_migration_plan.fct
// (fabric-migration), fabric_replication.fct (fabric-replication replica /
// placement / set), fabric.fct (frontdoor keyspace.rs + failover.rs),
// fabric_table.fct (fabric-routing table.rs), fabric_plan.fct (frontdoor
// plan.rs) and fabric_stats.fct (sample.rs + wire.rs EngineStats).
//
// Each testdata/fabric_leafb/*_check.fct app imports one port and runs, step
// for step, the same scenarios as a scratch Rust binary that linked the real
// crates by path (every #[test] in those crates ported input for input, plus
// traces through the public functions on representative inputs). That binary
// printed one `field<TAB>digest` line per scenario; those lines are the
// testdata/fabric_leafb/*_rust.tsv goldens, unedited. A mismatch in any field
// is a behavioural divergence from the Rust crate.

func lbLoadCheckApp(t *testing.T, file string) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("testdata", "fabric_leafb", file))
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func lbReadGolden(t *testing.T, file string) [][2]string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "fabric_leafb", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("%s: malformed golden line %q", file, line)
		}
		out = append(out, [2]string{k, v})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s: empty golden", file)
	}
	return out
}

func lbRunCheck(t *testing.T, app, action, golden string) {
	t.Helper()
	ts := lbLoadCheckApp(t, app)
	d := postJSON(t, ts, action)
	for _, kv := range lbReadGolden(t, golden) {
		got, _ := d[kv[0]].(string)
		if got != kv[1] {
			t.Errorf("%s: %s", kv[0], firstDiff(got, kv[1]))
		}
	}
}

// fabric-migration: phase.rs's edge table and predicates, plan.rs, progress.rs,
// and the stateful Migration through every migration.rs #[test] plus restart /
// budget / wrong-phase / terminal traces (each step's Result rendered through
// MigrationError's Display, each state through every public accessor and the
// serde-visible private marks).
func TestFabricLeafBMigration(t *testing.T) {
	lbRunCheck(t, "migration_check.fct", "runMigrationCheck", "migration_rust.tsv")
}

// fabric-replication: replica.rs (lifecycle graph, labels, lag thresholds,
// from_observation/unreachable reports, silence), set.rs (every #[test] plus
// refusal/unreachable/fail traces, each step's Result through Display and
// each set through every accessor), placement.rs (the three #[test]s plus a
// spread x factor sweep over scrambled candidates and a set with failed,
// lagging and surplus copies) and failover.rs (the four #[test]s plus empty,
// no-in-sync and ranked-candidate assessments).
func TestFabricLeafBReplication(t *testing.T) {
	lbRunCheck(t, "replication_check.fct", "runReplicationCheck", "replication_rust.tsv")
}

// fabric-routing table.rs: every #[test] plus topology ordering (the
// BTreeMap node/shard order), availability, forget, require-proven-health,
// read-preference, range and scan traces — each step's route or error
// (Display + is_retryable) and each table through every accessor.
func TestFabricLeafBTable(t *testing.T) {
	lbRunCheck(t, "table_check.fct", "runTableCheck", "table_rust.tsv")
}

// lbRunCases sends the golden lines' inputs (hex, the first column) to a
// check app's batch action, 100 per request, and compares each reply line
// with the line's second column.
func lbRunCases(t *testing.T, app, action, field, golden string) {
	t.Helper()
	ts := lbLoadCheckApp(t, app)
	lines := lbReadGolden(t, golden)
	bad := 0
	for start := 0; start < len(lines); start += 100 {
		end := start + 100
		if end > len(lines) {
			end = len(lines)
		}
		var hexes []string
		for _, kv := range lines[start:end] {
			hexes = append(hexes, kv[0])
		}
		tag := strconv.Itoa(start)
		d := postJSON(t, ts, action, tag, strings.Join(hexes, ","))
		got, _ := d[field].(string)
		if !strings.HasPrefix(got, tag+"|") {
			t.Fatalf("batch %s: no reply (%q)", tag, got)
		}
		results := strings.Split(strings.TrimPrefix(got, tag+"|"), "\n")
		if len(results) != end-start {
			t.Fatalf("batch %s: %d replies for %d cases", tag, len(results), end-start)
		}
		for i, kv := range lines[start:end] {
			if results[i] != kv[1] {
				bad++
				if bad <= 20 {
					t.Errorf("case %d (input hex %s):\n got: %s\nwant: %s", start+i, kv[0], results[i], kv[1])
				}
			}
		}
	}
	if bad > 20 {
		t.Errorf("... %d mismatching cases in all", bad)
	}
}

// fabric_json.fct: serde_json::from_slice::<Value> (sjDecode "value") then to_string (sjValueText), over a
// corpus of literals, every syntax error path (message and line/column),
// invalid UTF-8, surrogate escapes, the recursion limit, the RawValue key,
// u64/i64/f64 classification, and 900 generated floats through
// f64_from_parts and the zmij writer.
func TestFabricLeafBSerde(t *testing.T) {
	lbRunCases(t, "serde_check.fct", "runSerdeCases", "serdeResult", "serde_rust.tsv")
}

// fabric.fct's keyspace (frontdoor keyspace.rs): rule/keyspace construction
// errors, for_kind / for_address / spanning_key, and resolve over every
// kind x address combination, on six keyspaces.
func TestFabricLeafBKeyspace(t *testing.T) {
	lbRunCheck(t, "plan_check.fct", "runKeyspaceCheck", "keyspace_rust.tsv")
}

// fabric_plan.fct (frontdoor plan.rs): every plan.rs #[test] request plus
// front_door.rs's, over 2166 method/path/query/body/keyspace/read-preference
// combinations — each plan's keys, intents and origins, or its refusal's
// status and Display (serde_json's own body-error text included).
func TestFabricLeafBPlan(t *testing.T) {
	lbRunCases(t, "plan_check.fct", "runPlanCases", "planResult", "plan_rust.tsv")
}

// fabric_stats.fct's EngineStats decoder (wire.rs, serde derive over
// serde_json) against serde_json::from_slice::<EngineStats>: the Rust ->
// fct direction. The corpus includes 60 bodies serde_json itself encoded.
func TestFabricLeafBStatsDecode(t *testing.T) {
	lbRunCases(t, "stats_check.fct", "runStatsDecodeCases", "statsResult", "stats_decode_rust.tsv")
}

// sample.rs: StatsSample::difference over every #[test] input plus
// fractional intervals, CPU, queue-pressure and per-cell cases.
func TestFabricLeafBSample(t *testing.T) {
	lbRunCheck(t, "stats_check.fct", "runSampleCheck", "sample_rust.tsv")
}

var (
	lbCheckOnce sync.Once
	lbCheckBin  string
	lbCheckErr  string
)

// lbFabricCheck builds testdata/fabric_check once (the shared Rust harness
// that links the real fabric crates) and returns its path, skipping the
// calling test only when cargo is not installed.
func lbFabricCheck(t *testing.T) string {
	t.Helper()
	lbCheckOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			lbCheckErr = "cargo is not installed"
			return
		}
		target := os.Getenv("FCT_FABRIC_CHECK_TARGET")
		if target == "" {
			target = filepath.Join(os.TempDir(), "fct-fabric-check")
		}
		cmd := exec.Command(cargo, "build", "--release", "--quiet")
		cmd.Dir = filepath.Join("testdata", "fabric_check")
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			lbCheckErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		lbCheckBin = filepath.Join(target, "release", "fabric_check")
	})
	if lbCheckBin == "" {
		if strings.HasPrefix(lbCheckErr, "cargo build failed") {
			t.Fatal(lbCheckErr)
		}
		t.Skip("fabric_check unavailable: " + lbCheckErr)
	}
	return lbCheckBin
}

// The fct -> Rust direction: EngineStats values built in fct, encoded by
// statsJsonEngineStats, decoded by the real wire.rs type through
// serde_json; the Rust digest must equal the digest of fct's own decode of
// the same text (serde_json's default float parsing is not round-trip
// exact, so the text, not the original value, is the common ground).
func TestFabricLeafBStatsEncodeToRust(t *testing.T) {
	bin := lbFabricCheck(t)
	ts := lbLoadCheckApp(t, "stats_check.fct")
	d := postJSON(t, ts, "runStatsEncode")
	out, _ := d["encodeResult"].(string)
	lines := strings.Split(out, "\n")
	if len(lines) < 40 {
		t.Fatalf("expected 40 encoded samples, got %d", len(lines))
	}
	var bodies, want []string
	for _, l := range lines {
		body, digest, ok := strings.Cut(l, "\t")
		if !ok {
			t.Fatalf("malformed encode line %q", l)
		}
		bodies = append(bodies, body)
		want = append(want, "ok:"+digest)
	}
	cmd := exec.Command(bin, "leafb-stats-decode")
	cmd.Stdin = strings.NewReader(strings.Join(bodies, "\n") + "\n")
	res, err := cmd.Output()
	if err != nil {
		t.Fatalf("fabric_check leafb-stats-decode: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(res), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("%d decodes for %d bodies", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d: body %s\n rust: %s\n  fct: %s", i, bodies[i], got[i], want[i])
		}
	}
}

// fabric_json.fct's float writer (sjF64Text, the zmij layout serde_json
// 1.0.151 writes f64 with) against serde_json::to_string for 415 bit
// patterns: random, whole-number, subnormal and boundary values.
func TestFabricLeafBFloatTextToRust(t *testing.T) {
	bin := lbFabricCheck(t)
	ts := lbLoadCheckApp(t, "stats_check.fct")
	d := postJSON(t, ts, "runFloatTexts")
	out, _ := d["floatResult"].(string)
	lines := strings.Split(out, "\n")
	var bits, want []string
	for _, l := range lines {
		b, text, ok := strings.Cut(l, "\t")
		if !ok {
			t.Fatalf("malformed float line %q", l)
		}
		bits = append(bits, b)
		want = append(want, text)
	}
	cmd := exec.Command(bin, "leafb-f64-text")
	cmd.Stdin = strings.NewReader(strings.Join(bits, "\n") + "\n")
	res, err := cmd.Output()
	if err != nil {
		t.Fatalf("fabric_check leafb-f64-text: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(res), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("%d texts for %d floats", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bits %s: serde_json %s, fct %s", bits[i], got[i], want[i])
		}
	}
}

// lbPathCorpus: bodies for the path/category check — each seed (valid
// stats bodies from the golden corpus, the protocol message samples the
// real crate encodes, and assorted values) plus every truncation of the
// short ones and single-byte substitutions that break a value's type.
func lbPathCorpus(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	add := func(kind, body string) { out = append(out, [2]string{kind, body}) }
	unhex := func(h string) string {
		b := make([]byte, len(h)/2)
		for i := range b {
			v, _ := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
			b[i] = byte(v)
		}
		return string(b)
	}
	for i, kv := range lbReadGolden(t, "stats_decode_rust.tsv") {
		if i%3 == 0 {
			add("stats", unhex(kv[0]))
		}
	}
	for i, kv := range lbReadGolden(t, "serde_rust.tsv") {
		if i%2 == 0 {
			add("value", unhex(kv[0]))
		}
	}
	seeds := []string{
		`{"Heartbeat":{"node_id":5}}`, `{"Nope":{}}`, `{bad}`, `{}`, `"Acknowledged"`, `[]`,
		`{"RegisterNode":{"protocol":"p","node_id":"n9","software_version":"v","region":"eu"}}`,
		`{"Heartbeat":{"node_id":"n","timestamp_ms":7,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"n","timestamp_ms":-7,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"n","timestamp_ms":7,"healthy":1,"x":[1,{"y":2}]}}`,
		`{"Heartbeat":{"node_id":"n","node_id":"m"}}`, `{"Heartbeat":["n",7]}`, `{"Heartbeat":["n",7,true,4]}`,
		`{"Heartbeat":{"node_id":"n","timestamp_ms":7,"healthy":true}} x`, `{"Heartbeat":null}`, `{"Heartbeat":{}}`,
	}
	for _, s := range seeds {
		add("message", s)
		for cut := 1; cut < len(s); cut += 3 {
			add("message", s[:cut])
		}
	}
	v := `{"a":[1,{"b":[true,"x\u0001"]}],"c":{"d":{"e":[[],[{"f":null}]]}}}`
	for cut := 1; cut < len(v); cut++ {
		add("value", v[:cut])
	}
	st := `{"node_count":1,"edge_count":2,"user_count":3,"history_entries":4,"kinds":[{"kind":"K","count":1},{"kind":"L","count":2}],"reads_total":5,"writes_total":6,"storage":{"page_size":1,"segments":2,"pages":3,"obsolete_bytes":4},"runtime":{"window":{"read_latency":{"count":1,"p99_us":2}},"process":{"cpu_cores":3}},"cells":{"cells":[{"x":1},{"x":2,"reads":5}]}}`
	for cut := 1; cut < len(st); cut += 2 {
		add("stats", st[:cut])
	}
	for i := 0; i < len(st); i++ {
		if st[i] >= '0' && st[i] <= '9' {
			add("stats", st[:i]+`"s"`+st[i+1:])
			add("stats", st[:i]+`-1`+st[i+1:])
			add("stats", st[:i]+`1.5`+st[i+1:])
			add("stats", st[:i]+`999999999999`+st[i+1:])
		}
		if st[i] == '"' && i > 0 && st[i-1] == '{' {
			add("stats", st[:i]+`"zz":0,`+st[i:])
		}
	}
	return out
}

// fabric_json.fct's SjDecoded.category / .path against
// serde_path_to_error::deserialize + Error::classify over the real types
// (serde_json::Value, EngineStats, FabricMessage): exactly what axum 0.8's
// Json extractor reports.
func TestFabricLeafBPathToError(t *testing.T) {
	bin := lbFabricCheck(t)
	ts := lbLoadCheckApp(t, "path_check.fct")
	corpus := lbPathCorpus(t)
	byKind := map[string][]int{}
	for i, c := range corpus {
		byKind[c[0]] = append(byKind[c[0]], i)
	}
	want := make([]string, len(corpus))
	for kind, idxs := range byKind {
		var lines []string
		for _, i := range idxs {
			lines = append(lines, hex.EncodeToString([]byte(corpus[i][1])))
		}
		cmd := exec.Command(bin, "leafb-path-"+kind)
		cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
		res, err := cmd.Output()
		if err != nil {
			t.Fatalf("fabric_check leafb-path-%s: %v", kind, err)
		}
		got := strings.Split(strings.TrimSuffix(string(res), "\n"), "\n")
		if len(got) != len(idxs) {
			t.Fatalf("%s: %d results for %d bodies", kind, len(got), len(idxs))
		}
		for j, i := range idxs {
			want[i] = got[j]
		}
	}
	bad := 0
	for start := 0; start < len(corpus); start += 100 {
		end := start + 100
		if end > len(corpus) {
			end = len(corpus)
		}
		var items []string
		for _, c := range corpus[start:end] {
			items = append(items, c[0]+":"+hex.EncodeToString([]byte(c[1])))
		}
		tag := strconv.Itoa(start)
		d := postJSON(t, ts, "runPathCases", tag, strings.Join(items, ","))
		got, _ := d["pathResult"].(string)
		results := strings.Split(strings.TrimPrefix(got, tag+"|"), "\n")
		if len(results) != end-start {
			t.Fatalf("batch %s: %d replies for %d cases", tag, len(results), end-start)
		}
		for i, r := range results {
			if r != want[start+i] {
				bad++
				if bad <= 20 {
					t.Errorf("%s body %q:\n  fct: %s\n rust: %s", corpus[start+i][0], corpus[start+i][1], r, want[start+i])
				}
			}
		}
	}
	if bad > 20 {
		t.Errorf("... %d mismatches of %d", bad, len(corpus))
	}
}
