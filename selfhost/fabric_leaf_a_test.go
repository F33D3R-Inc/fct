package selfhost

// Conformance tests for the leaf fabric ports: fabric_json.fct (serde_json
// as the fabric crates use it), fabric_protocol.fct, fabric_core.fct,
// fabric_telemetry.fct, fabric_ml.fct, fabric_optimizer.fct,
// fabric_routing.fct and fabric_cli_render.fct.
//
// Two kinds of expected value, both produced by the real Rust crates:
//
//   - Live, both ways: testdata/fabric_check links fabric-core,
//     fabric-telemetry and fabric-protocol as libraries. Corpora (floats,
//     wire messages, responses, malformed payloads) go through the port and
//     through the real serde_json/fabric-protocol code, line for line; and
//     what the port encodes is decoded (and re-encoded) by the real types.
//     These skip only when cargo is not installed; set
//     FCT_FABRIC_CHECK_TARGET to choose where it builds.
//   - Fixed digests: every #[test] in the ported crates, input for input,
//     plus digests of the public functions on representative inputs, with
//     the expected strings printed by a scratch Rust binary linking the same
//     crates by path (render.rs compiled from its own source file).

import (
	"encoding/json"
	"math"
	"math/rand"
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

func laLoad(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File("fabric_leaf_a.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabric_leaf_a.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// laRun runs a driver action over lines and returns its output lines.
func laRun(t *testing.T, ts *httptest.Server, action string, lines []string) []string {
	t.Helper()
	d := postExprJSON(t, ts, action, strings.Join(lines, "\n"))
	out, _ := d["leafAOut"].(string)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

var (
	laCheckOnce sync.Once
	laCheckBin  string
	laCheckErr  string
)

// laCheck builds testdata/fabric_check once and returns its path, or skips
// the calling test when cargo is not installed.
func laCheck(t *testing.T) string {
	t.Helper()
	laCheckOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			laCheckErr = "cargo is not installed"
			return
		}
		if _, err := os.Stat("../../fabric/crates/fabric-protocol/Cargo.toml"); err != nil {
			laCheckErr = "no fabric checkout beside fct"
			return
		}
		target := os.Getenv("FCT_FABRIC_CHECK_TARGET")
		if target == "" {
			target = filepath.Join(os.TempDir(), "fct-fabric-check")
		}
		cmd := exec.Command(cargo, "build", "--release", "--quiet")
		cmd.Dir = "testdata/fabric_check"
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			laCheckErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		laCheckBin = filepath.Join(target, "release", "fabric_check")
	})
	if laCheckBin == "" {
		if strings.HasPrefix(laCheckErr, "cargo build failed") {
			t.Fatal(laCheckErr)
		}
		t.Skip("fabric_check unavailable: " + laCheckErr)
	}
	return laCheckBin
}

// laRust runs fabric_check in mode over lines (one per stdin line).
func laRust(t *testing.T, mode string, lines []string) []string {
	t.Helper()
	bin := laCheck(t)
	cmd := exec.Command(bin, mode)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fabric_check %s: %v", mode, err)
	}
	s := strings.TrimSuffix(string(out), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func laCompare(t *testing.T, what string, inputs, port, rust []string) {
	t.Helper()
	if len(port) != len(inputs) || len(rust) != len(inputs) {
		t.Fatalf("%s: %d inputs, port answered %d, rust answered %d", what, len(inputs), len(port), len(rust))
	}
	bad := 0
	for i := range inputs {
		if port[i] != rust[i] {
			bad++
			if bad <= 10 {
				t.Errorf("%s %q:\n port %s\n rust %s", what, inputs[i], port[i], rust[i])
			}
		}
	}
	if bad > 10 {
		t.Errorf("%s: %d mismatches in all", what, bad)
	}
}

// laFloatCorpus: special values, boundaries of ryu's layouts, integers
// either side of 2^53 and 2^63, and seeded random bit patterns and decimals.
func laFloatCorpus() []float64 {
	fs := []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 0.2, 0.30000000000000004, 1.5, 100, 1e15, 1e16, 1e17,
		1e21, 1e22, 1e23, 123456789012345680000, 1234567.5, 1e-4, 1e-5, 1e-6, 1e-7, 0.001234, 5e-324,
		2.2250738585072014e-308, math.MaxFloat64, -math.MaxFloat64, 9007199254740992, 9007199254740993,
		9007199254740994, 9007199254740996, 18014398509481984, 9223372036854775807, 9223372036854775808,
		1 << 62, 4611686018427387904 + 1024, 1e18, 1e19, 1e20, 999999999999999999, 12345678901234567890,
		math.Inf(1), math.Inf(-1), math.NaN(), 1.0 / 3.0, 2.0 / 3.0, 1e300, 1.7976931348623157e300, 0.5, 0.25,
		1234.5678, 0.7, 12.5, 400, 1e-300, 3.14159, 2.718281828459045}
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 1500; i++ {
		fs = append(fs, math.Float64frombits(r.Uint64()))
	}
	for i := 0; i < 800; i++ {
		// integers in [2^53, 2^63): the directly-searched range.
		e := 53 + r.Intn(10)
		fs = append(fs, float64(int64(1)<<e+r.Int63n(int64(1)<<e)))
	}
	for i := 0; i < 800; i++ {
		fs = append(fs, float64(r.Int63n(1_000_000))/math.Pow(10, float64(r.Intn(12))))
	}
	for i := 0; i < 200; i++ {
		fs = append(fs, math.Ldexp(1, r.Intn(2098)-1074))
	}
	return fs
}

func TestFabricJsonF64TextMatchesSerde(t *testing.T) {
	var lines []string
	for _, f := range laFloatCorpus() {
		lines = append(lines, strconv.FormatInt(int64(math.Float64bits(f)), 10))
	}
	ts := laLoad(t)
	port := laRun(t, ts, "runLeafAF64Text", lines)
	rust := laRust(t, "f64-text", lines)
	laCompare(t, "f64 bits", lines, port, rust)
}

func TestFabricJsonF64ParseMatchesSerde(t *testing.T) {
	lines := []string{"0", "-0", "-0.0", "1", "1.0", "1e5", "1E5", "1e+5", "1e-5", "0.1", "12.5", "-12.5",
		"123456789012345678901234567890", "18446744073709551615", "18446744073709551616", "-9223372036854775808",
		"-9223372036854775809", "1.7976931348623157e308", "1.8e308", "1e400", "-1e400", "1e-400", "0e999999999999",
		"4.9e-324", "2.4703282292062328e-324", "0.1000000000000000055511151231257827021181583404541015625",
		"3.14159265358979323846264338327950288", "1e", "1.", ".5", "01", "-", "--1", "+1", "1e+", " 7 ", "7 x",
		"\"1\"", "null", "true", "[1]", "9007199254740993", "123.456e-7", "0.000001", "1e2147483648", "1e-2147483649",
		"0.0e2147483648", "12345678901234567890.123e-5", "1.00000000000000011102230246251565404236316680908203125"}
	r := rand.New(rand.NewSource(11))
	for i := 0; i < 500; i++ {
		lines = append(lines, strconv.FormatFloat(math.Float64frombits(r.Uint64()&^(1<<63)), 'g', -1, 64))
	}
	for i := 0; i < 300; i++ {
		lines = append(lines, strconv.Itoa(r.Intn(100000))+"."+strconv.Itoa(r.Intn(100000))+"e"+strconv.Itoa(r.Intn(40)-20))
	}
	ts := laLoad(t)
	port := laRun(t, ts, "runLeafAF64Parse", lines)
	rust := laRust(t, "f64-parse", lines)
	laCompare(t, "f64 text", lines, port, rust)
}

const laHB = `"node_id":"db-a","timestamp_ms":1,"healthy":true`

// laMessageCorpus: well-formed and malformed FabricMessage payloads, each
// probing one serde/serde_json rule (field order, unknown and duplicate
// fields, the array form of a struct, integer/float classification, u8 and
// u64 ranges, string escapes and surrogates, enum tagging, trailing input).
func laMessageCorpus() []string {
	return []string{
		`{"Heartbeat":{` + laHB + `}}`,
		` {"Heartbeat" : {"node_id" : "db-a" , "timestamp_ms" : 1 , "healthy" : true} } `,
		`{"Heartbeat":{"healthy":true,"timestamp_ms":1,"node_id":"db-a"}}`,
		`{"Heartbeat":{` + laHB + `,"extra":[1,{"a":null,"b":[true,false,1.5e3]}]}}`,
		`{"Heartbeat":{` + laHB + `,"x":1,"x":2}}`,
		`{"Heartbeat":{` + laHB + `,"node_id":"db-b"}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1}}`,
		`{"Heartbeat":["db-a",1,true]}`,
		`{"Heartbeat":["db-a",1]}`,
		`{"Heartbeat":["db-a",1,true,4]}`,
		`{"Heartbeat":[]}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":-1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1.0,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1e3,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":18446744073709551615,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":18446744073709551616,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":9223372036854775808,"healthy":false}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":-0,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":01,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healthy":1}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healthy":"true"}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healthy":null}}`,
		`{"Heartbeat":{"node_id":7,"timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":["db-a"],"timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\u00e9\ud83d\ude00\/\b\f\n\r\t\"\\\u0001\u001F","timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\ud83d","timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\ud83dx","timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\udc00","timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\x","timestamp_ms":1,"healthy":true}}`,
		`{"Heartbeat":{"node_id":"\u12","timestamp_ms":1,"healthy":true}}`,
		"{\"Heartbeat\":{\"node_id\":\"a\x01b\",\"timestamp_ms\":1,\"healthy\":true}}",
		"{\"Heartbeat\":{\"node_id\":\"a\x7fb é 😀\",\"timestamp_ms\":1,\"healthy\":true}}",
		`{"Heartbeat":{` + laHB + `,}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healthy":tru}}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healthy":true}`,
		`{"Heartbeat":{"node_id":"db-a","timestamp_ms":1,"healt`,
		`{"Heartbeat":{` + laHB + `}}x`,
		`{"Heartbeat":{` + laHB + `}}   `,
		`{"Heartbeat":{` + laHB + `}}{}`,
		`"Heartbeat"`,
		`{"Heartbeat":null}`,
		`{}`,
		`[]`,
		`null`,
		``,
		`{"Heartbeat":{` + laHB + `},"Workload":{}}`,
		`{"heartbeat":{` + laHB + `}}`,
		`{"Explode":{}}`,
		`{"RegisterNode":{"protocol":"facet/2","node_id":"n","software_version":"v","region":"r"}}`,
		`{"RegisterNode":{"protocol":"facet/1","node_id":"n","software_version":"v"}}`,
		`{"RegisterNode":["facet/1","n","v","r"]}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[{"coordinate":{"x":255,"y":0},"dbms_id":"db-2","region":"eu"}]}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[{"coordinate":{"x":256,"y":0},"dbms_id":"db-2","region":"eu"}]}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[{"coordinate":{"x":-1,"y":0},"dbms_id":"db-2","region":"eu"}]}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[{"coordinate":[3,4],"dbms_id":"db-2","region":"eu"}]}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[{"coordinate":{"x":3},"dbms_id":"db-2","region":"eu"}]}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":{}}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":null}}`,
		`{"Topology":{"node_id":"db-1","timestamp_ms":42,"placements":[]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"legacy","grid":[1]},"samples":[{"coordinate":{"x":1,"y":1},"operations_per_second":1,"read_ratio":1,"write_ratio":0,"read_latency_us":1,"write_latency_us":1,"cpu_utilization":0.1,"memory_utilization":0.1,"queue_depth":0}]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":[1,"legacy"],"samples":[]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":[1,"legacy",2],"samples":[]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1},"samples":[]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"x"},"samples":[{"coordinate":{"x":1,"y":1},"operations_per_second":1,"read_ratio":1,"write_ratio":0,"read_latency_us":1,"write_latency_us":1,"cpu_utilization":0.1,"memory_utilization":0.1,"queue_depth":0,"cell_breakdown":null}]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"x"},"samples":[{"coordinate":{"x":1,"y":1},"operations_per_second":1,"read_ratio":1,"write_ratio":0,"read_latency_us":1,"write_latency_us":1,"cpu_utilization":0.1,"memory_utilization":0.1,"queue_depth":0,"cell_breakdown_partial":null}]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"x"},"samples":[[[1,1],2.5,0.5,0.5,1,2,0.3,0.4,5,[[1,2,3,4,1,2,3,4]],true]]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"x"},"samples":[[[1,1],2.5,0.5,0.5,1,2,0.3,0.4,5]]}}`,
		`{"Telemetry":{"timestamp_ms":9,"node_id":"db-6","shard":{"id":1,"workload_domain":"x"},"samples":[{"coordinate":{"x":1,"y":1},"operations_per_second":1,"read_ratio":1,"write_ratio":0,"read_latency_us":1,"write_latency_us":1,"cpu_utilization":0.1,"memory_utilization":0.1,"queue_depth":0,"cell_breakdown":[{"x":1,"y":2,"z":3,"q":256,"reads_per_second":1,"writes_per_second":1,"bytes_read_per_second":1,"bytes_written_per_second":1}]}]}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":12,"read_ratio":-0,"write_ratio":1E2}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":123456789012345678901234567890,"read_ratio":0.1e-3,"write_ratio":-5e-324}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":1e400,"read_ratio":0,"write_ratio":0}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":"1","read_ratio":0,"write_ratio":0}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":.5,"read_ratio":0,"write_ratio":0}}`,
		`{"Workload":{"node_id":"db-4","coordinate":{"x":5,"y":6},"timestamp_ms":77,"operations_per_second":NaN,"read_ratio":0,"write_ratio":0}}`,
	}
}

// Rust -> fct and fct -> fct: every payload (the corpus, the real crate's
// own encoded messages, and the port's) decoded by both, then the decoded
// message re-encoded and answered by handle/encode.
func TestFabricProtocolDecodeMatchesRust(t *testing.T) {
	ts := laLoad(t)
	corpus := laMessageCorpus()
	corpus = append(corpus, laRust(t, "protocol-samples", nil)...)
	corpus = append(corpus, laRun(t, ts, "runLeafAProtocolSamples", nil)...)
	port := laRun(t, ts, "runLeafAProtocolDecode", corpus)
	rust := laRust(t, "protocol-decode", corpus)
	laCompare(t, "payload", corpus, port, rust)
	accepted := 0
	for _, r := range rust {
		if !strings.HasPrefix(r, "ERR|") {
			accepted++
		}
	}
	if accepted < 30 || accepted > len(corpus)-30 {
		t.Fatalf("corpus should mix accepted and refused payloads: %d of %d accepted", accepted, len(corpus))
	}
}

// fct -> Rust: what the port encodes is exactly what serde_json writes for
// the same values once the real types have decoded it.
func TestFabricProtocolEncodeDecodedByRust(t *testing.T) {
	ts := laLoad(t)
	mine := laRun(t, ts, "runLeafAProtocolSamples", nil)
	if len(mine) != 7 {
		t.Fatalf("port produced %d messages", len(mine))
	}
	rust := laRust(t, "protocol-decode", mine)
	for i, line := range mine {
		got, _, _ := strings.Cut(rust[i], "|")
		if got != line {
			t.Errorf("message %d:\n port %s\n rust %s", i, line, rust[i])
		}
	}
	responses := laRun(t, ts, "runLeafAResponseSamples", nil)
	back := laRust(t, "response-decode", responses)
	laCompare(t, "response", responses, responses, back)
}

func laResponseCorpus() []string {
	return []string{`"Acknowledged"`, `{"Acknowledged":null}`, `{"Acknowledged":{}}`, `{"Acknowledged":1}`,
		`"Registered"`, `{"Registered":{"node_id":"x"}}`, `{"Registered":["x"]}`, `{"Registered":{}}`,
		`{"Registered":{"node_id":"x","node_id":"y"}}`, `{"Registered":{"node_id":"x","other":[]}}`,
		`{"OptimizationProposal":{"coordinate":"(1,2)","action":"isolate","expected_gain":1,"estimated_cost":0.45,"confidence":-0}}`,
		`{"OptimizationProposal":["c","a",1,2.5,3e-3]}`, `{"OptimizationProposal":["c","a",1,2.5]}`,
		`{"OptimizationProposal":{"coordinate":"c","action":"a","expected_gain":true,"estimated_cost":0,"confidence":0}}`,
		`{"Rejected":{"reason":"r","extra":1}}`, `{"Rejected":"r"}`, `{"Rejected":{"reason":null}}`, `"Nope"`,
		`{"Nope":{}}`, `{}`, `7`, `{"Rejected":{"reason":"a"},"Registered":{"node_id":"b"}}`}
}

// Rust -> fct for responses: the real crate's encoded responses and a
// corpus, decoded by both and re-encoded.
func TestFabricProtocolResponsesMatchRust(t *testing.T) {
	ts := laLoad(t)
	corpus := append(laResponseCorpus(), laRust(t, "response-samples", nil)...)
	port := laRun(t, ts, "runLeafAResponseDecode", corpus)
	rust := laRust(t, "response-decode", corpus)
	laCompare(t, "response", corpus, port, rust)
}

// laDigestActions maps each digest to the driver action that computes it.
var laDigestActions = map[string]string{
	"core":      "runLeafACoreDigest",
	"telemetry": "runLeafATelemetryDigest",
	"ml":        "runLeafAMlDigest",
	"optimizer": "runLeafAOptimizerDigest",
	"routing":   "runLeafARoutingDigest",
	"cli":       "runLeafACliDigest",
	"protocol":  "runLeafAProtocolDigest",
}

// Each digest is one crate's #[test]s (input for input) and its public
// functions on representative inputs, sections separated by '#'. The
// expected strings (laWant, fabric_leaf_a_want_test.go) were printed by a
// scratch Rust binary linking the real crates by path.
func TestFabricLeafDigestsMatchRust(t *testing.T) {
	ts := laLoad(t)
	for name := range laWant {
		if _, ok := laDigestActions[name]; !ok {
			t.Errorf("%s: expected digest has no driver action", name)
		}
	}
	for name, action := range laDigestActions {
		d := postExprJSON(t, ts, action)
		got, _ := d["leafAOut"].(string)
		want, ok := laWant[name]
		if !ok {
			t.Errorf("%s: no expected digest", name)
			continue
		}
		if got != want {
			t.Errorf("%s: %s", name, firstDiff(got, want))
		}
	}
}

// laSessionCorpus: whole Vec<FabricMessage> payloads (fabric-cli session
// files), many spanning lines so serde_json's "at line L column C" is
// exercised: every message-corpus payload inside a list, pretty-printed
// real messages, and list-level errors.
func laSessionCorpus(t *testing.T) []string {
	samples := laRust(t, "protocol-samples", nil)
	out := []string{"[]", " [ ] ", "[\n]", "", "   \n  ", "{}", "null", "[1]", "[\"Heartbeat\"]", "[\n  \"Heartbeat\"\n]",
		"[\n  \"Heartbeat\",\n  1\n]", "[\n" + strings.Join(samples, ",\n") + "\n]",
		"[" + strings.Join(samples, ",") + "]", "[" + samples[2] + ",]", "[" + samples[2] + ",\n\n]", "[" + samples[2],
		"[" + samples[2] + "\n", "[" + samples[2] + "] x", "[" + samples[2] + "]\n\n  x", "[" + samples[2] + " " + samples[2] + "]",
		"[\r\n\t" + samples[2] + "\r\n]", "[\n  {\n    \"Explode\": {}\n  }\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\",\n \"timestamp_ms\": 1}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"node_id\" : \"b\", \"timestamp_ms\": 1, \"healthy\": true}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\",\n\"timestamp_ms\": 1.5, \"healthy\": true}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": [1, {\"y\": \"\\ud83d\"}, 2e999]}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": [1, {\"y\": \"z\" \"w\"}]}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": [1 2]}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": {\"k\" 1}}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": {1: 1}}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": [}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": \"\x01\"}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"\x01\", \"timestamp_ms\": 1, \"healthy\": true}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": 01}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": 1.}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": -}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": 1e}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": nul}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": [1,]}}\n]",
		"[\n  {\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true, \"x\": {\"a\":1,}}}\n]",
		"[{\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true}}, {\"Workload\": null}]",
		"[{\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true} , \"x\": 1}]",
		"[{\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 1, \"healthy\": true}\n",
		"[{\"Heartbeat\" {}}]", "[{\"Heartbeat\"", "[{", "[{ ", "[ {\n", "[\"", "[\"Heart", "[\"Nope\"]", "[{\"Heartbeat\":{}", "[{\"Heartbeat\":{}  ", "[{7: 1}]", "[{\"Heartbeat\": 7}]", "[{\"Heartbeat\": \"x\\u00e9\"}]",
		"[{\"Heartbeat\": [\"a\", 1, true], \"Workload\": 1}]", "[{\"Heartbeat\": [\"a\", 1]}]", "[{\"Heartbeat\": [\"a\", 1, true,]}]",
		"[{\"Heartbeat\": [\"a\", 1, true, 4]}]", "[{\"Heartbeat\": [\"a\", -1, true]}]", "[{\"Heartbeat\": [\"a\", 1e3, true]}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [{\"coordinate\": {\"x\": 300, \"y\": 0}, \"dbms_id\": \"d\", \"region\": \"r\"}]}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [{\"coordinate\": {\"x\": -3, \"y\": 0}, \"dbms_id\": \"d\", \"region\": \"r\"}]}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [{\"coordinate\": {\"x\": 3.5e1, \"y\": 0}, \"dbms_id\": \"d\", \"region\": \"r\"}]}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [{\"coordinate\": {\"x\": 18446744073709551616, \"y\": 0}, \"dbms_id\": \"d\", \"region\": \"r\"}]}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [{\"coordinate\": {\"x\": \"q\\n\\\"\", \"y\": 0}, \"dbms_id\": \"d\", \"region\": \"r\"}]}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": {\"a\": 1}}}]",
		"[{\"Topology\": {\"node_id\": \"n\", \"timestamp_ms\": 0, \"placements\": [true]}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": [1], \"samples\": []}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": 0.000001}, \"samples\": []}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": 12345678901234567890123.0}, \"samples\": []}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": -0.0}, \"samples\": []}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": false}, \"samples\": []}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": \"w\"}, \"samples\": [[[1,1],2.5,0.5,0.5,1,2,0.3,0.4,5,[],false,9]]}}]",
		"[{\"Telemetry\": {\"timestamp_ms\": 0, \"node_id\": \"n\", \"shard\": {\"id\": 1, \"workload_domain\": \"w\"}, \"samples\": [[[1,1],2.5,0.5,0.5,1,2,0.3,0.4]]}}]",
		"[{\"Workload\": {\"node_id\": \"n\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1e309, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"n\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1e99999999999, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"n\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 0e99999999999, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\uD83D\\uDE00\\u00E9\\/\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\uD83Dx\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\uD83D\\u0041\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\uDE00\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\uZZZZ\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0}}]",
		"[{\"Workload\": {\"node_id\": \"\\u12",
		"[{\"Workload\": {\"node_id\": \"\\q\", \"coordinate\": {\"x\": 1, \"y\": 1}}}]",
		"[{\"Workload\": {\"node_id\": \"abc",
		"[{\"Workload\": {\"node_id\": \"é😀\", \"coordinate\": {\"x\": 1, \"y\": 1}, \"timestamp_ms\": 0, \"operations_per_second\": 1, \"read_ratio\": 0, \"write_ratio\": 0, \"extra\": \"é😀\" }, }]",
		"[{\"Workload\": {\"node_id\": \"n\"}}] ",
		"[{\"RegisterNode\": {}}]",
		"[{\"RegisterNode\": {\"protocol\": \"p\", \"node_id\": \"n\", \"software_version\": \"v\", \"region\": \"r\"}}, {}]",
		"[{\"RegisterNode\": {\"protocol\": \"p\", \"node_id\": \"n\", \"software_version\": \"v\", \"region\": \"r\"}},\n{\"Heartbeat\": {\"node_id\": \"a\", \"timestamp_ms\": 18446744073709551615, \"healthy\": false}}]",
	}
	for _, m := range laMessageCorpus() {
		out = append(out, "[\n  "+m+"\n]")
	}
	return out
}

func laQuoteAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		b, _ := json.Marshal(x)
		out[i] = string(b)
	}
	return out
}

// fct <-> Rust for fabric-cli session files: acceptance, the decoded
// messages, and serde_json's error Display (with its line and column).
func TestFabricProtocolSessionMatchesRust(t *testing.T) {
	ts := laLoad(t)
	lines := laQuoteAll(laSessionCorpus(t))
	port := laRun(t, ts, "runLeafASessionDecode", lines)
	rust := laRust(t, "session-decode", lines)
	laCompare(t, "session", lines, port, rust)
}
