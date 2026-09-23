package selfhost

// fabric_protocol_server.fct ports fabric-protocol's HTTP listener (the axum
// router + token middleware in transport.rs and the facet-protocol binary).
// Every request in fabricProtoRequests is sent, raw, on its own connection to
// the port running as a command (runtime.RunMain, the way `facet exec` runs
// it), and its answer — status line, headers other than `date`, body — must
// equal what the real facet-protocol binary answered.
// testdata/fabric_protocol_server_golden.json holds the real binary's
// answers; with cargo the binary is built (--locked, fabric/ untouched) and
// the golden is checked against it live. Both listen on 127.0.0.1:7700, as
// the binary hard-codes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

const fabricProtoToken = "tok-3f9"

type fabricProtoCase struct {
	Name     string `json:"name"`
	Request  string `json:"-"`
	Response string `json:"response"`
}

func fabricProtoRequests() []fabricProtoCase {
	post := func(name, headers, body string) fabricProtoCase {
		return fabricProtoCase{Name: name, Request: "POST /v1/message HTTP/1.1\r\nHost: x\r\n" + headers + "Content-Length: " + itoaProto(len(body)) + "\r\n\r\n" + body}
	}
	auth := "x-api-key: " + fabricProtoToken + "\r\n"
	ct := "Content-Type: application/json\r\n"
	hb := `{"Heartbeat":{"node_id":"n","timestamp_ms":1,"healthy":true}}`
	reg := `{"RegisterNode":{"protocol":"facet/1","node_id":"db-7","software_version":"0.13.0","region":"us-east"}}`
	topo := `{"Topology":{"node_id":"db-7","timestamp_ms":5,"placements":[{"coordinate":{"x":1,"y":2},"dbms_id":"db-7","region":"r"}]}}`
	tel := `{"Telemetry":{"timestamp_ms":9,"node_id":"n1","shard":{"id":4,"workload_domain":"d"},"samples":[{"coordinate":{"x":2,"y":1},"operations_per_second":5.0,"read_ratio":0.5,"write_ratio":0.5,"read_latency_us":1.0,"write_latency_us":2.0,"cpu_utilization":0.9,"memory_utilization":0.9,"queue_depth":20000}]}}`
	wl := `{"Workload":{"node_id":"n","coordinate":{"x":0,"y":0},"timestamp_ms":3,"operations_per_second":1.5,"read_ratio":1.0,"write_ratio":0.0}}`
	big := `{"Heartbeat":{"node_id":"` + strings.Repeat("a", 2*1024*1024+10) + `","timestamp_ms":1,"healthy":true}}`
	cs := []fabricProtoCase{
		post("heartbeat", ct+auth, hb),
		post("register", ct+auth, reg),
		post("topology", ct+auth, topo),
		post("telemetry", ct+auth, tel),
		post("workload", ct+auth, wl),
		post("no token", ct, hb),
		post("wrong token", ct+"x-api-key: tok-3f\r\n", hb),
		post("longer token", ct+"x-api-key: tok-3f9x\r\n", hb),
		post("empty token", ct+"x-api-key: \r\n", hb),
		post("two token headers, first wrong", ct+"x-api-key: nope\r\n"+auth, hb),
		post("no content type", auth, hb),
		post("text/json", "Content-Type: text/json\r\n"+auth, hb),
		post("json charset", "Content-Type: application/json; charset=utf-8\r\n"+auth, hb),
		post("vnd+json", "Content-Type: application/vnd.api+json\r\n"+auth, hb),
		post("uppercase json", "Content-Type: APPLICATION/JSON\r\n"+auth, hb),
		post("empty body", ct+auth, ""),
		post("syntax", ct+auth, "{bad}"),
		post("unknown variant", ct+auth, `{"Nope":{}}`),
		post("response as message", ct+auth, `"Acknowledged"`),
		post("wrong field type", ct+auth, `{"Heartbeat":{"node_id":5}}`),
		post("missing field", ct+auth, `{"Heartbeat":{"node_id":"n"}}`),
		post("truncated key", ct+auth, reg[:58]),
		post("empty object", ct+auth, "{}"),
		post("array then garbage", ct+auth, "[] x"),
		post("trailing garbage", ct+auth, hb+" x"),
		post("nested path", ct+auth, `{"Topology":{"node_id":"db-7","timestamp_ms":5,"placements":[{"coordinate":{"x":1,"y":2},"dbms_id":"db-7","region":"r"},{"coordinate":{"x":300,"y":2},"dbms_id":"d","region":"r"}]}}`),
		post("too large", ct+auth, big),
		post("too large, no token", ct, big),
		{Name: "get authorized", Request: "GET /v1/message HTTP/1.1\r\nHost: x\r\n" + auth + "\r\n"},
		{Name: "get unauthorized", Request: "GET /v1/message HTTP/1.1\r\nHost: x\r\n\r\n"},
		{Name: "head authorized", Request: "HEAD /v1/message HTTP/1.1\r\nHost: x\r\n" + auth + "\r\n"},
		{Name: "other path", Request: "POST /other HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n"},
		{Name: "trailing slash", Request: "POST /v1/message/ HTTP/1.1\r\nHost: x\r\n" + auth + "Content-Length: 0\r\n\r\n"},
		{Name: "query string", Request: "POST /v1/message?x=1 HTTP/1.1\r\nHost: x\r\n" + ct + auth + "Content-Length: " + itoaProto(len(hb)) + "\r\n\r\n" + hb},
		{Name: "chunked body", Request: "POST /v1/message HTTP/1.1\r\nHost: x\r\n" + ct + auth + "Transfer-Encoding: chunked\r\n\r\n" + hexProto(len(hb)) + "\r\n" + hb + "\r\n0\r\n\r\n"},
	}
	return cs
}

func itoaProto(n int) string { return strconv.Itoa(n) }
func hexProto(n int) string {
	const d = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{d[n%16]}, b...)
		n /= 16
	}
	return string(b)
}

// fabricProtoExchange sends one raw request on a fresh connection and
// returns the response with its `date` header removed and the remaining
// headers sorted (hyper and the port may order them differently).
func fabricProtoExchange(t *testing.T, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:7700")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	go func() { io.WriteString(c, raw) }()
	var buf bytes.Buffer
	tmp := make([]byte, 65536)
	for {
		n, err := c.Read(tmp)
		buf.Write(tmp[:n])
		if head, body, ok := bytes.Cut(buf.Bytes(), []byte("\r\n\r\n")); ok {
			want := -1
			for _, line := range strings.Split(string(head), "\r\n") {
				if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "content-length") {
					want = atoiProto(strings.TrimSpace(v))
				}
			}
			isHead := strings.HasPrefix(raw, "HEAD ")
			if (want >= 0 && len(body) >= want) || (isHead && want >= 0) {
				break
			}
		}
		if err != nil {
			break
		}
	}
	head, body, _ := bytes.Cut(buf.Bytes(), []byte("\r\n\r\n"))
	lines := strings.Split(string(head), "\r\n")
	var hs []string
	for _, l := range lines[1:] {
		if strings.HasPrefix(strings.ToLower(l), "date:") {
			continue
		}
		hs = append(hs, strings.ToLower(l))
	}
	sort.Strings(hs)
	return lines[0] + "\n" + strings.Join(hs, "\n") + "\n\n" + string(body)
}

func atoiProto(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func fabricProtoWaitPort(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", "127.0.0.1:7700"); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("nothing listening on 127.0.0.1:7700")
}

func fabricProtoGolden(t *testing.T) []fabricProtoCase {
	t.Helper()
	b, err := os.ReadFile("testdata/fabric_protocol_server_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cs []fabricProtoCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

func fabricProtoLoad(t *testing.T) *runtime.Server {
	t.Helper()
	g, err := compile.File("fabric_protocol_server.fct")
	if err != nil {
		t.Fatalf("compile selfhost/fabric_protocol_server.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// The port's listener runs for the rest of the test process (a command's
// main serves until the process ends), so it is started once.
var (
	fabricProtoPortOnce sync.Once
	fabricProtoPortOut  bytes.Buffer
)

func fabricProtoStartPort(t *testing.T) {
	t.Helper()
	fabricProtoPortOnce.Do(func() {
		os.Setenv("FABRIC_PROTOCOL_TOKEN", fabricProtoToken)
		srv := fabricProtoLoad(t)
		srv.SetStdio(strings.NewReader(""), &fabricProtoPortOut, io.Discard)
		go srv.RunMain(nil)
	})
	fabricProtoWaitPort(t)
}

// TestFabricProtocolServerGoldenMatchesRealBinary replays the corpus against
// the real facet-protocol (rewriting the golden under
// FCT_REGEN_FABRIC_PROTOCOL_GOLDEN=1). It must run before the port takes the
// port, so it is declared before TestFabricProtocolServer (tests run in
// source order) and skipped if the port is already up in this process.
func TestFabricProtocolServerGoldenMatchesRealBinary(t *testing.T) {
	bin := fabricProtoRealBinary(t)
	if bin == "" {
		t.Skip("real facet-protocol unavailable: " + fabricProtoBinErr)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:7700"); err == nil {
		c.Close()
		t.Skip("127.0.0.1:7700 is already taken (the port runs in this process); run this test on its own")
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "FABRIC_PROTOCOL_TOKEN="+fabricProtoToken)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	fabricProtoWaitPort(t)
	var live []fabricProtoCase
	for _, c := range fabricProtoRequests() {
		live = append(live, fabricProtoCase{Name: c.Name, Request: c.Request, Response: fabricProtoExchange(t, c.Request)})
	}
	if os.Getenv("FCT_REGEN_FABRIC_PROTOCOL_GOLDEN") == "1" {
		b, _ := json.MarshalIndent(live, "", "  ")
		if err := os.WriteFile("testdata/fabric_protocol_server_golden.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden := fabricProtoGolden(t)
	if len(golden) != len(live) {
		t.Fatalf("golden has %d cases, the real binary answered %d", len(golden), len(live))
	}
	for i := range live {
		if golden[i].Name != live[i].Name || golden[i].Response != live[i].Response {
			t.Errorf("golden %s is stale:\n golden %q\n   real %q", live[i].Name, golden[i].Response, live[i].Response)
		}
	}
	if got := out.String(); got != "Facet Protocol listening on 127.0.0.1:7700\n" {
		t.Errorf("real banner %q", got)
	}
}

func TestFabricProtocolServer(t *testing.T) {
	golden := fabricProtoGolden(t)
	cases := fabricProtoRequests()
	if len(golden) != len(cases) {
		t.Fatalf("golden has %d cases, the test %d — regenerate with FCT_REGEN_FABRIC_PROTOCOL_GOLDEN=1", len(golden), len(cases))
	}
	fabricProtoStartPort(t)
	for i, c := range cases {
		if golden[i].Name != c.Name {
			t.Fatalf("golden case %d is %q, the test's is %q", i, golden[i].Name, c.Name)
		}
		if got := fabricProtoExchange(t, c.Request); got != golden[i].Response {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, golden[i].Response)
		}
	}
	if got := fabricProtoPortOut.String(); got != "Facet Protocol listening on 127.0.0.1:7700\n" {
		t.Errorf("banner %q", got)
	}
}

// TestFabricProtocolServerRefusesWithoutToken: the binary's fail-closed
// start — stderr and exit code, before anything listens.
func TestFabricProtocolServerRefusesWithoutToken(t *testing.T) {
	t.Setenv("FABRIC_PROTOCOL_TOKEN", "")
	srv := fabricProtoLoad(t)
	var out, errOut bytes.Buffer
	srv.SetStdio(strings.NewReader(""), &out, &errOut)
	code, err := srv.RunMain(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "facet-protocol: environment variable FABRIC_PROTOCOL_TOKEN is not set: this listener accepts node registrations, heartbeats, topology and telemetry reports, so it is never served unauthenticated\n"
	if code != 1 || out.String() != "" || errOut.String() != want {
		t.Fatalf("got code %d stdout %q stderr %q", code, out.String(), errOut.String())
	}
	if bin := fabricProtoRealBinary(t); bin != "" {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "FABRIC_PROTOCOL_TOKEN=")
		var rOut, rErr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &rOut, &rErr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || rErr.String() != want || rOut.String() != "" {
			t.Fatalf("the real binary: %v stdout %q stderr %q", err, rOut.String(), rErr.String())
		}
	}
}

var (
	fabricProtoBinOnce sync.Once
	fabricProtoBin     string
	fabricProtoBinErr  string
)

func fabricProtoRealBinary(t *testing.T) string {
	t.Helper()
	fabricProtoBinOnce.Do(func() {
		cargo, err := exec.LookPath("cargo")
		if err != nil {
			fabricProtoBinErr = "cargo is not installed"
			return
		}
		target := filepath.Join(os.TempDir(), "fct-fabric-protocol")
		cmd := exec.Command(cargo, "build", "--locked", "--quiet", "--manifest-path", "../../fabric/crates/fabric-protocol/Cargo.toml", "--bin", "facet-protocol")
		cmd.Env = append(os.Environ(), "CARGO_TARGET_DIR="+target)
		if out, err := cmd.CombinedOutput(); err != nil {
			fabricProtoBinErr = "cargo build failed: " + err.Error() + "\n" + string(out)
			return
		}
		fabricProtoBin = filepath.Join(target, "debug", "facet-protocol")
	})
	if strings.HasPrefix(fabricProtoBinErr, "cargo build failed") {
		t.Fatal(fabricProtoBinErr)
	}
	return fabricProtoBin
}
