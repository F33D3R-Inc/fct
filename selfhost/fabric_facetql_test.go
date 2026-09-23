package selfhost

// fabric_facetql_{wire,client,placement}.fct against the real
// fabric-facetql crate.
//
//   - Unit: error.rs's, endpoint.rs's, client.rs's and placement.rs's
//     #[test]s, input for input.
//   - Wire, both ways (testdata/fabric_check `fql-*` modes): the same JSON
//     specs encoded by the real Serialize types and by the port must match
//     byte for byte; the same bodies decoded by the real Deserialize types
//     and by the port must render identically (or fail with the same serde
//     message); and what the port encodes is fed back to the real decoders.
//   - Client trace: the real FacetqlClient/PlacementStore and the port run
//     the same script against a recording Go server that answers both with
//     the same canned responses. The requests each sent (method, target,
//     headers, body) and the results each reported must be identical.
//
// Error texts are compared whole — serde_json's " at line L column C"
// included — with one named normalization: a Transport error is compared
// up to "{context} on {base_url}: " (after that is reqwest's own error
// text).
// The recorded `connection` header is left out: the port opens one
// connection per request and says `connection: close`; reqwest pools.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func fqltApp(t *testing.T, file string) *httptest.Server {
	t.Helper()
	g, err := compile.File(file)
	if err != nil {
		t.Fatalf("compile selfhost/%s: %v", file, err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

var (
	fqltLastMu sync.Mutex
	fqltLast   = map[string]string{}
)

// fqltCall runs action and returns the state cell out (remembered, since
// an unchanged cell is absent from the deltas).
func fqltCall(t *testing.T, ts *httptest.Server, out, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	fqltLastMu.Lock()
	defer fqltLastMu.Unlock()
	key := ts.URL + "|" + out
	if v, ok := d[out].(string); ok {
		fqltLast[key] = v
	}
	return fqltLast[key]
}

func fqltCompare(t *testing.T, what string, inputs, port, rust []string) {
	t.Helper()
	if len(port) != len(inputs) || len(rust) != len(inputs) {
		t.Fatalf("%s: %d inputs, %d port lines, %d rust lines\nport: %q\nrust: %q", what, len(inputs), len(port), len(rust), port, rust)
	}
	for i := range inputs {
		if port[i] != rust[i] {
			t.Errorf("%s #%d %s\n port: %s\n rust: %s", what, i, inputs[i], port[i], rust[i])
		}
	}
}

// fqltWire runs one wire mode on both sides over inputs.
func fqltWire(t *testing.T, ts *httptest.Server, mode string, inputs []string) (port, rust []string) {
	t.Helper()
	rust = laRust(t, "fql-"+mode, inputs)
	port = strings.Split(fqltCall(t, ts, "fqlWireOut", "fqlWireRun", mode, strings.Join(inputs, "\n")), "\n")
	return port, rust
}

var fqltEncodeQuerySpecs = []string{
	`{}`,
	`{"kind":"__fabric_placement","limit":100,"after":"Y3Vyc29y"}`,
	`{"kind":"__fabric_placement","limit":500}`,
	`{"kind":"k","owner":"o","where":{"op":"and","args":[{"b":1,"a":2}],"z":null},"item_var":"it","order":"-x","desc":true,"after":"é\"\\\n\u0001","limit":0}`,
	`{"desc":false,"where":null,"owner":"😀"}`,
	`{"where":[1,2.5,-0.0,1e300,"s",true,1e-7,123456789012345678]}`,
}

var fqltEncodeCreateSpecs = []string{
	`{"address":"Entity:1","kind":"Entity","x":0,"y":0,"z":0,"q":0,"data":"{}","public":false,"if_absent":false}`,
	`{"address":"p:\u0001\u007f😀","kind":"K","x":255,"y":1,"z":2,"q":3,"data":"{\"a\":[1,2]}","public":true,"if_absent":true}`,
}

var fqltEncodeTxSpecs = []string{
	`[{"op":"insert_node","address":"Entity:1","kind":"Entity","x":0,"y":0,"z":0,"q":0,"data":"{}","public":false},{"op":"delete_node","address":"Entity:1"},{"op":"insert_edge","from":"a","to":"b","kind":"rel"},{"op":"delete_edge","from":"a","to":"b","kind":"rel"},{"op":"clear_kind","kind":"Entity"},{"op":"delete_where","kind":"Entity"}]`,
	`[{"op":"delete_where","kind":"__session","where":{"op":"<","left":"x","right":1}}]`,
	`[{"op":"set_if","address":"__cron:nightly","field":"next_run","expect":{"le":1000.0},"set":{"version":2}},{"op":"set_if","address":"p:1","field":"version","expect":{"eq":1},"set":{"version":2}},{"op":"set_if","address":"p:1","field":"version","expect":{"absent":true},"set":{"version":2}}]`,
	`[{"op":"set_if","address":"a","field":"f","expect":{"le":0.1},"set":{}},{"op":"set_if","address":"a","field":"f","expect":{"le":1e21},"set":{"z":1,"a":{"y":[],"b":null}}},{"op":"set_if","address":"a","field":"f","expect":{"eq":{"k":"v","a":[1.5]}},"set":{}},{"op":"set_if","address":"a","field":"f","expect":{"absent":false},"set":{}},{"op":"set_if","address":"é","field":"\"","expect":{"le":-3},"set":{"\n":"\u0000"}}]`,
	`[]`,
}

const fqltNodeA = `{"address":"p:1","coordinate":{"x":1,"y":2,"z":3,"q":4},"value":7,"kind":"__fabric_placement","data":"{\"a\":\"é\\n\"}","owner":"fabric","claimed_by":null,"visibility":"Private"}`

var fqltDecodeNodeBodies = []string{
	fqltNodeA,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","claimed_by":"w","visibility":"Public"}`,
	`{"visibility":"Public","owner":"o","data":"d","kind":"k","value":0,"coordinate":{"q":0,"z":0,"y":0,"x":0},"address":"a","extra":[1,{"x":2}]}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":"public"}`,
	`{"address":1}`,
	`{"value":-1,"address":"a"}`,
	`{"value":1.5}`,
	`{"address":"a","coordinate":{"x":300,"y":0,"z":0,"q":0}}`,
	`{"coordinate":{"x":1,"y":0,"z":0}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o"}`,
	`{}`,
	`[]`,
	`["a",[1,2,3,4],5,"k","d","o",null,"Private"]`,
	`["a",[1,2,3,4],5,"k","d","o",null,"Private",9]`,
	`["a",[1,2,3],5]`,
	`"node"`,
	`null`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","claimed_by":5,"visibility":"Private"}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":3}`,
	`not json`,
	`{"address":"a",}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":{"Public":null}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":{"Public":1}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":{"Nope":null}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":{}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":null}`,
	`{"address":"a","address":"b"}`,
	`{"coordinate":{"x":1,"x":2}}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":18446744073709551615,"kind":"k","data":"d","owner":"o","visibility":"Private"}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":18446744073709551616,"kind":"k","data":"d","owner":"o","visibility":"Private"}`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","visibility":"Private"} x`,
	`{"address":"a","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":1,"kind":"k","data":"d","owner":"o","claimed_by":"\ud800","visibility":"Private"}`,
	`{"address":"a","coordinate":[1,2,3,4],"value":1,"kind":"k","data":"d","owner":"o","claimed_by":null,"visibility":{"Public":null}}`,
}

var fqltDecodePageBodies = []string{
	`{"nodes":[` + fqltNodeA + `,` + fqltNodeA + `],"next":"Y3Vyc29y"}`,
	`{"nodes":[],"next":""}`,
	`{"nodes":[]}`,
	`{"next":"x"}`,
	`{"nodes":{},"next":""}`,
	`{"nodes":[{"address":"a"}],"next":""}`,
	`[[],"c"]`,
}

const fqltStatsOld = `{"node_count":3,"edge_count":1,"user_count":2,"history_entries":4,"kinds":[{"kind":"Post","count":3}],"reads_total":10,"writes_total":5,"storage":{"page_size":4096,"segments":1,"pages":9,"obsolete_bytes":0}}`

const fqltStatsFull = `{"node_count":3,"edge_count":1,"user_count":2,"history_entries":4,"kinds":[{"kind":"Post","count":3}],"reads_total":10,"writes_total":5,"storage":{"page_size":4096,"segments":1,"pages":9,"obsolete_bytes":0},"version":"1.2.3","runtime":{"uptime_seconds":120,"requests":{"total":50,"read":30,"write":20,"excluded":1,"unclassified":0,"in_flight":400,"max_concurrent":512,"write_queue_depth":3,"write_queue_contended_total":9},"window":{"duration_ms":1000,"age_ms":5,"cpu_utilization":0.42,"read_latency":{"count":10,"p50_us":100,"p99_us":900,"max_us":950},"write_latency":{"count":0,"p50_us":null,"p99_us":null,"max_us":null}},"process":{"cpu_seconds_total":12.5,"cpu_cores":8,"resident_bytes":1048576,"memory_limit_bytes":4194304,"memory_limit_source":"cgroup","memory_utilization":0.25}},"cells":{"capacity":256,"tracked":2,"overflow_reads":0,"overflow_writes":0,"unattributed_writes":1,"cells":[{"x":1,"y":2,"z":3,"q":4,"reads":100,"writes":10,"bytes_read":1000,"bytes_written":100},{"x":0,"y":0,"z":0,"q":0,"reads":5,"writes":1,"bytes_read":50,"bytes_written":5}]}}`

var fqltDecodeStatsBodies = []string{
	fqltStatsOld,
	fqltStatsFull,
	`{"node_count":3,"edge_count":1,"user_count":2,"history_entries":4,"kinds":[],"reads_total":10,"writes_total":5}`,
	strings.Replace(fqltStatsOld, `"page_size":4096`, `"page_size":4294967296`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"version":5`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"runtime":null`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"cells":{"cells":[{"x":300}]}`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"runtime":{"window":{"cpu_utilization":"x"}}`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"runtime":{"window":{"cpu_utilization":1,"read_latency":[5,6]}},"cells":[7,8]`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"runtime":{"process":{"cpu_cores":1.0}}`, 1),
	strings.Replace(fqltStatsOld, `{"kind":"Post","count":3}`, `["Post",3]`, 1),
	strings.Replace(fqltStatsOld, `{"kind":"Post","count":3}`, `{"kind":"Post"}`, 1),
	`[1,2,3,4,[],5,6,[4096,1,9,0]]`,
	`[1,2,3,4,[],5,6]`,
	strings.Replace(fqltStatsOld, `"node_count":3`, `"node_count":3,"node_count":4`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":18446744073709551615`, 1),
	strings.Replace(fqltStatsOld, `"reads_total":10`, `"reads_total":10,"runtime":{"window":{"read_latency":{"p99_us":null,"count":3}}}`, 1),
}

func TestFabricFacetqlWireAgainstRust(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_wire.fct")
	for _, c := range []struct {
		mode   string
		inputs []string
	}{
		{"encode-query", fqltEncodeQuerySpecs},
		{"encode-create", fqltEncodeCreateSpecs},
		{"encode-tx", fqltEncodeTxSpecs},
		{"decode-node", fqltDecodeNodeBodies},
		{"decode-page", fqltDecodePageBodies},
		{"decode-stats", fqltDecodeStatsBodies},
	} {
		port, rust := fqltWire(t, ts, c.mode, c.inputs)
		fqltCompare(t, c.mode, c.inputs, port, rust)
	}

	// fct -> Rust: the port's own encodings of decoded nodes are read back
	// by the real Node decoder and come out unchanged.
	port, _ := fqltWire(t, ts, "decode-node", fqltDecodeNodeBodies[:3])
	back := laRust(t, "fql-decode-node", port)
	fqltCompare(t, "decode-node(port encoding)", port, back, port)
}

// wire.rs's #[test]s whose literals are the spec, input for input.
func TestFabricFacetqlWireRustUnitTests(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_wire.fct")
	run := func(mode, line string) string {
		return fqltCall(t, ts, "fqlWireOut", "fqlWireRun", mode, line)
	}
	// transaction_ops_match_the_canonical_contract
	if got, want := run("encode-tx", fqltEncodeTxSpecs[0]), `{"operations":[{"type":"insert_node","address":"Entity:1","kind":"Entity","x":0,"y":0,"z":0,"q":0,"data":"{}","public":false},{"type":"delete_node","address":"Entity:1"},{"type":"insert_edge","from":"a","to":"b","kind":"rel"},{"type":"delete_edge","from":"a","to":"b","kind":"rel"},{"type":"clear_kind","kind":"Entity"},{"type":"delete_where","kind":"Entity"}]}`; got != want {
		t.Errorf("canonical ops:\n got %s\nwant %s", got, want)
	}
	// delete_where_carries_its_predicate_under_the_key_where
	if got := run("encode-tx", fqltEncodeTxSpecs[1]); !strings.Contains(got, `"type":"delete_where"`) || !strings.Contains(got, `"where":{"left":"x","op":"<","right":1}`) {
		t.Errorf("delete_where: %s", got)
	}
	// each_set_if_expectation_serializes_as_exactly_one_key
	if got, want := run("encode-tx", fqltEncodeTxSpecs[2]), `{"operations":[{"type":"set_if","address":"__cron:nightly","field":"next_run","expect_le":1000.0,"set":{"version":2}},{"type":"set_if","address":"p:1","field":"version","expect_eq":1,"set":{"version":2}},{"type":"set_if","address":"p:1","field":"version","expect_absent":true,"set":{"version":2}}]}`; got != want {
		t.Errorf("set_if:\n got %s\nwant %s", got, want)
	}
	// a_query_request_omits_what_it_does_not_set
	if got, want := run("encode-query", fqltEncodeQuerySpecs[1]), `{"kind":"__fabric_placement","after":"Y3Vyc29y","limit":100}`; got != want {
		t.Errorf("query request: got %s want %s", got, want)
	}
	// client.rs a_query_request_encodes_to_the_contract_body
	if got, want := run("encode-query", fqltEncodeQuerySpecs[2]), `{"kind":"__fabric_placement","limit":500}`; got != want {
		t.Errorf("contract body: got %s want %s", got, want)
	}
	// stats_and_nodes_decode_from_facetqls_own_shape: an older FacetQL's
	// stats decode with tolerant defaults, and a page with next "" is last.
	if got := run("decode-stats", fqltStatsOld); !strings.Contains(got, `"version":null`) || !strings.Contains(got, `"cpu_utilization":null`) || !strings.Contains(got, `"uptime_seconds":0`) || !strings.Contains(got, `"page_size":4096`) || !strings.Contains(got, `"kinds":[{"count":3,"kind":"Post"}]`) {
		t.Errorf("old stats: %s", got)
	}
	if got := run("decode-page", `{"nodes":[{"address":"p:1","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":0,"kind":"__fabric_placement","data":"{}","owner":"fabric","claimed_by":null,"visibility":"Private"}],"next":""}`); !strings.HasSuffix(got, `"visibility":"Private"}]||false`) {
		t.Errorf("page: %s", got)
	}
	// a_current_stats_response_decodes_every_added_field
	got := run("decode-stats", fqltStatsFull)
	for _, want := range []string{`"version":"1.2.3"`, `"in_flight":400`, `"max_concurrent":512`, `"cpu_utilization":0.42`, `"p99_us":900`, `"write_latency":{"count":0,"max_us":null,"p50_us":null,"p99_us":null}`, `"cpu_seconds_total":12.5`, `"cpu_cores":8`, `"memory_limit_source":"cgroup"`, `"capacity":256`, `"unattributed_writes":1`, `{"bytes_read":1000,"bytes_written":100,"q":4,"reads":100,"writes":10,"x":1,"y":2,"z":3}`} {
		if !strings.Contains(got, want) {
			t.Errorf("full stats missing %s: %s", want, got)
		}
	}
}

// error.rs's, endpoint.rs's and client.rs's #[test]s, plus both-way checks
// of the endpoint constructor and every error variant's rendering.
func TestFabricFacetqlClientUnitsAgainstRust(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_client.fct")
	endpointInputs := []string{
		"db-a\thttp://127.0.0.1:8892/\ts3cret",
		"db-a\thttp://127.0.0.1:8892\t",
		"db-a\t127.0.0.1:8892\tt",
		"db-a\t  https://h.example//  \tt",
		"q\"b\\s\x01é\thttp://x\tt",
		"db\tftp://x\tt",
	}
	var port []string
	for _, in := range endpointInputs {
		f := strings.Split(in, "\t")
		port = append(port, fqltCall(t, ts, "fqlClientOut", "fqlEndpointRun", f[0], f[1], f[2]))
	}
	fqltCompare(t, "endpoint", endpointInputs, port, laRust(t, "fql-endpoint", endpointInputs))

	// debug_never_prints_the_token / an_empty_token_is_refused_at_construction
	// / a_non_http_base_url_is_refused
	if !strings.Contains(port[0], "<redacted>") || strings.Contains(port[0], "s3cret") {
		t.Errorf("Debug leaked the token: %s", port[0])
	}
	if !strings.HasPrefix(port[1], "ERR Configuration ") || !strings.Contains(port[1], "no API token") {
		t.Errorf("empty token: %s", port[1])
	}
	if !strings.Contains(port[2], "not http(s)") {
		t.Errorf("non-http base: %s", port[2])
	}
	// from_env, including a_missing_env_var_names_the_variable_not_a_value:
	// set, set to "", and unset — the variable's name in the error, never a
	// value.
	t.Setenv("FQLT_TOKEN_SET", "s3cret")
	t.Setenv("FQLT_TOKEN_EMPTY", "")
	envInputs := []string{
		"db-a\thttp://127.0.0.1:8892/\tFQLT_TOKEN_SET",
		"db-a\thttp://127.0.0.1:8892\tFQLT_TOKEN_EMPTY",
		"db-a\thttp://127.0.0.1:8892\tFABRIC_TEST_TOKEN_THAT_IS_NOT_SET",
	}
	var envPort []string
	for _, in := range envInputs {
		f := strings.Split(in, "\t")
		envPort = append(envPort, fqltCall(t, ts, "fqlClientOut", "fqlEndpointEnvRun", f[0], f[1], f[2]))
	}
	fqltCompare(t, "endpoint from_env", envInputs, envPort, laRust(t, "fql-endpoint-env", envInputs))
	if !strings.Contains(envPort[2], "FABRIC_TEST_TOKEN_THAT_IS_NOT_SET") || strings.Contains(envPort[0], "s3cret") {
		t.Errorf("from_env: %q", envPort)
	}

	// a_trailing_slash_does_not_double_up
	if got := fqltCall(t, ts, "fqlClientOut", "fqlTrailingSlashRun"); got != "true" {
		t.Errorf("trailing slash: %s", got)
	}
	// debug_of_a_client_does_not_print_the_token
	if got := fqltCall(t, ts, "fqlClientOut", "fqlClientDebugRun", "db-a", "http://127.0.0.1:8892", "s3cret"); strings.Contains(got, "s3cret") || got == "" {
		t.Errorf("client Debug: %q", got)
	}

	errorInputs := []string{
		"Configuration\t0\tno token\t",
		"Transport\t0\trefused\t",
		"Unauthorized\t403\tadmin only\t",
		"PreconditionFailed\t0\tstale\t",
		"Conflict\t0\texists\t",
		"NotFound\t0\tgone\t",
		"Status\t500\tdisk\t",
		"Decode\t0\tGET /stats\tmissing field `x`",
	}
	port = nil
	for _, in := range errorInputs {
		f := strings.Split(in, "\t")
		var status int
		fmt.Sscan(f[1], &status)
		port = append(port, fqltCall(t, ts, "fqlClientOut", "fqlErrorRun", f[0], status, f[2], f[3]))
	}
	fqltCompare(t, "error", errorInputs, port, laRust(t, "fql-error", errorInputs))
	// a_lost_race_does_not_condemn_the_instance /
	// unreachable_and_unauthenticated_both_fail_closed
	for i, want := range []string{"true", "true", "true", "false", "false", "false", "true", "true"} {
		if got := strings.Fields(port[i])[2]; got != want {
			t.Errorf("%s implies_unhealthy = %s, want %s", errorInputs[i], got, want)
		}
	}
}

// placement.rs's #[test]s, input for input.
func TestFabricFacetqlPlacementUnitTests(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_placement.fct")
	if got := fqltCall(t, ts, "placementOut", "placementRunDemos"); got != "true,true,true,true,true" {
		t.Fatalf("placement demos: %s", got)
	}
}

// fqltRecorder answers requests from a fixed list of canned responses, in
// order, and records every request it saw.
type fqltRecorder struct {
	mu      sync.Mutex
	replies []fqltReply
	next    int
	seen    []string
}

type fqltReply struct {
	status int
	body   string
}

func (r *fqltRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	var hs []string
	for k, vs := range req.Header {
		if k == "Connection" {
			continue
		}
		hs = append(hs, strings.ToLower(k)+": "+strings.Join(vs, ","))
	}
	sort.Strings(hs)
	r.mu.Lock()
	r.seen = append(r.seen, fmt.Sprintf("%s %s host=%s [%s] %s", req.Method, req.RequestURI, req.Host, strings.Join(hs, "; "), body))
	reply := fqltReply{599, "no canned reply"}
	if r.next < len(r.replies) {
		reply = r.replies[r.next]
	}
	r.next++
	r.mu.Unlock()
	w.WriteHeader(reply.status)
	io.WriteString(w, reply.body)
}

func (r *fqltRecorder) reset() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := r.seen
	r.seen, r.next = nil, 0
	return seen
}

func fqltPlacementNode(shard, x, y int, dbms, region string, version int) string {
	data := fmt.Sprintf(`{"dbms_id":%q,"shard_id":%d,"x":%d,"y":%d,"region":%q,"version":%d}`, dbms, shard, x, y, region, version)
	return fmt.Sprintf(`{"address":"__fabric_placement:%d:%d:%d","coordinate":{"x":0,"y":0,"z":0,"q":0},"value":0,"kind":"__fabric_placement","data":%q,"owner":"fabric","claimed_by":null,"visibility":"Private"}`, shard, x, y, data)
}

// fqltTraceReplies: one canned reply per request of the script in
// fql.rs's `trace` / fabric_facetql_placement.fct's fqlTraceScript.
func fqltTraceReplies() []fqltReply {
	nodeB := strings.Replace(fqltNodeA, `"p:1"`, `"p:2"`, 1)
	p1 := fqltPlacementNode(9001, 1, 2, "db-b", "eu-west", 2)
	p2 := fqltPlacementNode(9002, 3, 4, "db-c", "ap-south", 1)
	bad := strings.Replace(fqltPlacementNode(9003, 0, 0, "x", "y", 1), `"data":"{`, `"data":"not json{`, 1)
	page := func(next string, nodes ...string) string {
		return `{"nodes":[` + strings.Join(nodes, ",") + `],"next":"` + next + `"}`
	}
	return []fqltReply{
		{200, fqltStatsFull},
		{200, fqltStatsOld},
		{200, "[" + fqltNodeA + "]"},
		{200, "[]"},
		{200, "[]"},
		{200, page("abc", fqltNodeA)},
		{200, page("c1", fqltNodeA)},
		{200, page("", nodeB)},
		{200, fqltNodeA},
		{404, "node not found"},
		{200, "[" + nodeB + "]"},
		{200, `{"count":42}`},
		{409, "address exists"},
		{412, "stale"},
		{200, `{"claimed":true}`},
		{409, "already claimed"},
		{404, "nope"},
		{403, "admin only"},
		{500, "disk"},
		{200, "{\n  \"address\": \"a\",\n  \"value\": \"7\"\n}"},
		{200, "{}"},
		{200, `{"nodes":[{"address":1}],"next":""}`},
		{200, `{"count":-1}`},
		{200, "{}"},
		{200, `{"ok":true}`},
		{412, "version mismatch"},
		{200, page("", p1, p2)},
		{200, page("", p2, p1)},
		{200, page("", p1)},
		{200, page("", p1, bad)},
		{200, page("", p1, p2)},
		{404, "no such route"},
		{200, "data: {}\n\n"},
	}
}

func TestFabricFacetqlClientTraceAgainstRust(t *testing.T) {
	bin := laCheck(t)
	rec := &fqltRecorder{}
	upstream := httptest.NewServer(rec)
	defer upstream.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	rec.replies = fqltTraceReplies()
	out, err := exec.Command(bin, "fql-trace", upstream.URL, "tr4ce-tok", dead).Output()
	if err != nil {
		t.Fatalf("fabric_check fql-trace: %v", err)
	}
	rustLines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	rustSeen := rec.reset()

	rec.replies = fqltTraceReplies()
	ts := fqltApp(t, "fabric_facetql_placement.fct")
	portLines := strings.Split(fqltCall(t, ts, "fqlTraceOut", "fqlTraceRun", upstream.URL, "tr4ce-tok", dead), "\n")
	portSeen := rec.reset()

	if len(rustSeen) != len(fqltTraceReplies()) {
		t.Errorf("the Rust client made %d requests, the script expects %d", len(rustSeen), len(fqltTraceReplies()))
	}
	fqltCompare(t, "requests", rustSeen, portSeen, rustSeen)

	// The last line is the unreachable instance: compare it up to where
	// reqwest's own error text begins.
	transport := "ERR Transport true facetql unreachable: GET /stats on " + dead + ": "
	last := len(rustLines) - 1
	if last < 0 || len(portLines) != len(rustLines) {
		t.Fatalf("result lines: port %d, rust %d\nport: %q\nrust: %q", len(portLines), len(rustLines), portLines, rustLines)
	}
	if !strings.HasPrefix(rustLines[last], transport) || !strings.HasPrefix(portLines[last], transport) {
		t.Errorf("transport line:\n port: %s\n rust: %s", portLines[last], rustLines[last])
	}
	steps := make([]string, last)
	for i := range steps {
		steps[i] = fmt.Sprintf("step %d", i+1)
	}
	fqltCompare(t, "results", steps, portLines[:last], rustLines[:last])
}

// poller.rs's #[test]s, input for input.
func TestFabricFacetqlPollerUnitTests(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_poller.fct")
	if got := fqltCall(t, ts, "pollerOut", "pollerRunDemos"); got != "true,true,true,true" {
		t.Fatalf("poller demos: %s", got)
	}
	m := fqlLiveFields(t, fqltCall(t, ts, "pollerOut", "pollerRunUnreachable"))
	for k, v := range map[string]string{"outcomes": "1", "healthy": "false", "failure": "Transport", "registered": "true", "reportedHealthy": "false", "health": "degraded", "analyzer": "0", "observations": "0"} {
		if m[k] != v {
			t.Errorf("unreachable instance: %s = %q, want %q (all: %v)", k, m[k], v, m)
		}
	}
}

// mover.rs's #[test]s, input for input.
func TestFabricFacetqlMoverUnitTests(t *testing.T) {
	ts := fqltApp(t, "fabric_facetql_mover.fct")
	if got := fqltCall(t, ts, "moverOut", "moverRunDemos"); got != "true,true,true,true,true,true,true" {
		t.Fatalf("mover demos: %s", got)
	}
}

// The mover's change feed against a source whose /events refuses, and one
// whose stream ends after two frames: both are fatal and both are said.
func TestFabricFacetqlMoverFeedFailures(t *testing.T) {
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("X-Api-Key") != "tok" {
			http.Error(w, "unexpected request", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "id: 1\ndata: {\"event\":\"node_updated\",\"address\":\"Post:1\"}\n\n")
		f.Flush()
		io.WriteString(w, "id: 2\ndata: {\"event\":\"transaction_committed\",\"addresses\":[\"b\",\"a\",\"b\"]}\n\n: keep-alive\n\n")
		f.Flush()
	}))
	defer src.Close()
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "admin only", http.StatusForbidden)
	}))
	defer denied.Close()
	ts := fqltApp(t, "fabric_facetql_mover.fct")

	m := fqlLiveFields(t, fqltCall(t, ts, "moverOut", "moverRunLive", "feed", src.URL, "", "tok", ""))
	broken := "the source's change feed is no longer complete (the stream from 'src' ended), so writes landing during the copy can no longer be seen"
	for k, v := range map[string]string{"dirty": "Post:1,a,b", "observed": "4", "catchUp": broken, "verify": "failed " + broken} {
		if m[k] != v {
			t.Errorf("%s = %q, want %q", k, m[k], v)
		}
	}
	got := fqltCall(t, ts, "moverOut", "moverRunLive", "feed", denied.URL, "", "tok", "")
	if got != "subscribe='src': facetql refused the token (403): admin only\n" {
		t.Errorf("refused subscription: %q", got)
	}
}
