package selfhost

// fqserver.fct against the real FacetQL server, over the wire.
//
// Both servers are started the way an operator starts them — the Rust one
// as facetql_check's `serve` mode (Database::new + create_router, i.e.
// `facetql start`'s server path), the fct one as `facet exec
// selfhost/fqserver.fct` — against empty data directories and the same
// environment. The same raw HTTP/1.1 requests go to each, in the same
// order, and every answer must match: status line, headers (the `date`
// aside) and body, byte for byte — auth, routing (404/405 with `allow`),
// CORS, every handler's success and refusal texts, axum's extractor
// rejections (serde's messages and positions included), the event feed's
// sequence numbers and SSE framing, /changes, and the engine half of
// /stats. The only normalisations are the ones that are not the server's
// choice: a freshly minted token, a wall-clock archive time, and the order
// of GET /admin/users (a HashMap's iteration order in Rust).

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	fqServerFacetOnce sync.Once
	fqServerFacetBin  string
	fqServerFacetErr  error
)

// fqServerFacet builds this tree's `facet` command once.
func fqServerFacet(t *testing.T) string {
	t.Helper()
	fqServerFacetOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fqserver-facet-")
		if err != nil {
			fqServerFacetErr = err
			return
		}
		fqServerFacetBin = filepath.Join(dir, "facet")
		cmd := exec.Command("go", "build", "-o", fqServerFacetBin, "facet/cmd/facet")
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			fqServerFacetErr = fmt.Errorf("%v\n%s", err, out)
		}
	})
	if fqServerFacetErr != nil {
		t.Fatalf("building facet: %v", fqServerFacetErr)
	}
	return fqServerFacetBin
}

func fqFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// fqServerPair starts the Rust and the fct server with the same extra
// environment and returns their ports once both answer.
func fqServerPair(t *testing.T, env ...string) (int, int) {
	t.Helper()
	return fqServerPairWith(t, nil, env...)
}

// fqServerPairWith: fqServerPair, with perServer adding environment for
// each server given its own data directory (a file one of them must read,
// which the fct server's sandbox wants inside its data directory).
func fqServerPairWith(t *testing.T, perServer func(dir string) []string, env ...string) (int, int) {
	t.Helper()
	rust := facetqlCheck(t)
	facet := fqServerFacet(t)
	server, err := filepath.Abs("fqserver.fct")
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"FACETQL_TOKENS=tok:alice:admin,utok:bob", "FACETQL_ENV=development", "FACETQL_MASTER_KEY=" + facetqlCheckKey}
	start := func(name string, argv []string, dir string, port int, extra ...string) {
		log, err := os.Create(dir + ".log")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = append(append(append(os.Environ(), base...), env...), append(extra, "FACETQL_DATA_DIR="+dir, fmt.Sprintf("FACETQL_PORT=%d", port))...)
		if perServer != nil {
			cmd.Env = append(cmd.Env, perServer(dir)...)
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting %s: %v", name, err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			log.Close()
		})
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := fqDial(port); err == nil {
				c.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		out, _ := os.ReadFile(dir + ".log")
		t.Fatalf("%s never listened:\n%s", name, out)
	}
	root := t.TempDir()
	rdir, fdir := filepath.Join(root, "rust"), filepath.Join(root, "fct")
	for _, d := range []string{rdir, fdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rp, fp := fqFreePort(t), fqFreePort(t)
	start("facetql", []string{rust, "serve"}, rdir, rp)
	start("fqserver.fct", []string{facet, "exec", server}, fdir, fp, "FACET_DATA_DIR="+fdir)
	return rp, fp
}

// fqRaw sends one raw request on a fresh connection and reads the answer:
// to its content-length, or (a stream) for `linger` after the head.
func fqRaw(t *testing.T, port int, raw string, linger time.Duration) string {
	t.Helper()
	c, err := fqDial(port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	head := strings.HasPrefix(raw, "HEAD ")
	var out []byte
	buf := make([]byte, 65536)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if i := strings.Index(string(out), "\r\n\r\n"); i >= 0 {
			h := strings.ToLower(string(out[:i]))
			m := regexp.MustCompile(`content-length: (\d+)`).FindStringSubmatch(h)
			if head || strings.Contains(h, " 204 ") {
				break
			}
			if m != nil {
				var n int
				fmt.Sscan(m[1], &n)
				if len(out)-(i+4) >= n {
					break
				}
			} else if strings.Contains(h, "transfer-encoding: chunked") && linger > 0 {
				deadline = time.Now().Add(linger)
				linger = 0
			}
		}
		_ = c.SetReadDeadline(deadline)
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "timeout") {
				break
			}
			t.Fatalf("reading answer: %v", err)
		}
	}
	return string(out)
}

// fqUseTLS makes every connection these tests open a TLS one (the TLS test
// sets it while it runs).
var fqUseTLS bool

func fqDial(port int) (net.Conn, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if fqUseTLS {
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	}
	return net.Dial("tcp", addr)
}

var (
	fqTokenRE = regexp.MustCompile(`"token":"[0-9a-f]{64}"`)
	fqTimeRE  = regexp.MustCompile(`"archived_at_unix":\d+`)
)

// fqNorm: the status line, the headers sorted (no `date`), the body.
func fqNorm(resp string) string {
	i := strings.Index(resp, "\r\n\r\n")
	if i < 0 {
		return "<no head>" + resp
	}
	lines := strings.Split(resp[:i], "\r\n")
	var hs []string
	for _, l := range lines[1:] {
		if !strings.HasPrefix(strings.ToLower(l), "date:") {
			hs = append(hs, strings.ToLower(l))
		}
	}
	sort.Strings(hs)
	body := fqTokenRE.ReplaceAllString(resp[i+4:], `"token":"<minted>"`)
	body = fqTimeRE.ReplaceAllString(body, `"archived_at_unix":<now>`)
	return lines[0] + "\n" + strings.Join(hs, "\n") + "\n\n" + body
}

type fqCase struct {
	method, path, key, ctype, body string
	hasBody                        bool
}

func (c fqCase) raw() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: x\r\n", c.method, c.path)
	if c.key != "" {
		fmt.Fprintf(&b, "x-api-key: %s\r\n", c.key)
	}
	if c.hasBody {
		if c.ctype != "" {
			fmt.Fprintf(&b, "Content-Type: %s\r\n", c.ctype)
		}
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(c.body))
	}
	b.WriteString("\r\n")
	b.WriteString(c.body)
	return b.String()
}

func fqGet(path, key string) fqCase { return fqCase{method: "GET", path: path, key: key} }
func fqSend(method, path, key, body string) fqCase {
	return fqCase{method: method, path: path, key: key, ctype: "application/json", body: body, hasBody: true}
}

func fqNode(addr, kind, data string, extra string) string {
	d, _ := json.Marshal(data)
	return fmt.Sprintf(`{"address":%q,"kind":%q,"x":1,"y":2,"z":3,"q":4,"data":%s%s}`, addr, kind, d, extra)
}

// fqServerScript: every route, as the owner-admin alice (tok) and the
// plain user bob (utok), successes and refusals, in an order whose later
// requests see the state the earlier ones left.
func fqServerScript() []fqCase {
	pred := `{"kind":"binop","op":">","l":{"kind":"field","obj":{"kind":"var","name":"item"},"name":"n"},"r":{"kind":"lit","val":2}}`
	ins := func(addr, data, extra string) string {
		d, _ := json.Marshal(data)
		return fmt.Sprintf(`{"type":"insert_node","address":%q,"kind":"T","x":0,"y":0,"z":0,"q":0,"data":%s%s}`, addr, d, extra)
	}
	tx := func(ops ...string) string { return `{"operations":[` + strings.Join(ops, ",") + `]}` }
	return []fqCase{
		fqGet("/", ""), fqGet("/nope", ""), fqGet("/node/", ""), fqGet("/node/x/", "tok"),
		{method: "POST", path: "/stats"}, {method: "PUT", path: "/nodes"}, {method: "PATCH", path: "/node/x", key: "tok"},
		fqGet("/stats", "bad"), fqGet("/admin/users", "utok"), {method: "OPTIONS", path: "/node"}, {method: "OPTIONS", path: "/nope"},
		{method: "HEAD", path: "/node/zz", key: "tok"},
		fqSend("POST", "/node", "tok", fqNode("p:1", "Post", `{"n":1,"t":"hello world"}`, `,"public":true`)),
		fqSend("POST", "/node", "tok", fqNode("p:2", "Post", `{"n":2,"t":"second"}`, "")),
		fqSend("POST", "/node", "tok", fqNode("p:3", "Post", `{"n":3.5,"t":"third"}`, `,"edges":[{"to":"p:1","kind":"REL"}]`)),
		fqSend("POST", "/node", "tok", fqNode("p:1", "Post", `{"n":10}`, `,"if_absent":true`)),
		fqSend("POST", "/node", "utok", fqNode("b:1", "Post", `{"n":7}`, "")),
		fqSend("POST", "/node", "utok", fqNode("p:1", "Post", `{"n":8}`, "")),
		fqSend("POST", "/node", "utok", fqNode("b:2", "Post", `{"n":9}`, `,"edges":[{"to":"nope","kind":"X"}]`)),
		fqGet("/node/p:1", "tok"), fqGet("/node/p:2", "utok"), fqGet("/node/p:1", "utok"), fqGet("/node/b:1", "utok"),
		fqSend("PUT", "/node/p:1", "tok", `{"data":"{\"n\":11}"}`), fqSend("PUT", "/node/p:1", "utok", `{"data":"x"}`),
		fqSend("PUT", "/node/zz", "tok", `{"data":"x"}`), fqSend("PUT", "/node/b:1", "utok", `{"data":"{\"n\":70}","public":true}`),
		fqGet("/node/p:1/history", "tok"), fqGet("/node/p:2/history", "utok"), fqGet("/node/zz/history", "tok"),
		fqGet("/node/p:1/owned", "utok"), fqGet("/node/b:1/owned", "tok"),
		fqGet("/nodes?kind=Post", "tok"), fqGet("/nodes?kind=Post", "utok"), fqGet("/nodes?kind=Post&limit=1&offset=1", "tok"),
		fqGet("/nodes?owner=bob", "tok"), fqGet("/nodes?kind=Post&kind=X", "tok"), fqGet("/nodes?limit=abc", "tok"),
		fqGet("/nodes?limit=-1", "tok"), fqGet("/nodes?limit=99999999999999999999999", "tok"),
		fqSend("POST", "/edge", "tok", `{"from":"p:2","to":"p:1","kind":"LINK"}`),
		fqSend("POST", "/edge", "utok", `{"from":"b:1","to":"p:2","kind":"LINK"}`),
		fqSend("POST", "/edge", "utok", `{"from":"p:1","to":"b:1","kind":"LINK"}`),
		fqSend("POST", "/edge", "utok", `{"from":"b:1","to":"p:1","kind":"LINK"}`),
		fqSend("POST", "/edge", "tok", `{"from":"x","to":"p:1","kind":"LINK"}`),
		fqGet("/node/p:1/edges/in", "tok"), fqGet("/node/p:2/edges/out", "tok"), fqGet("/node/p:2/edges/out", "utok"),
		fqSend("DELETE", "/edge", "utok", `{"from":"p:2","to":"p:1","kind":"LINK"}`),
		fqSend("DELETE", "/edge", "utok", `{"from":"b:1","to":"p:1","kind":"LINK"}`),
		fqSend("DELETE", "/edge", "tok", `{"from":"b:1","to":"p:1","kind":"LINK"}`),
		{method: "POST", path: "/node/p:2/claim", key: "utok"}, {method: "POST", path: "/node/p:1/claim", key: "utok"},
		{method: "POST", path: "/node/b:1/claim", key: "utok"}, {method: "POST", path: "/node/b:1/claim", key: "utok"},
		{method: "POST", path: "/node/new:1/claim", key: "tok"},
		fqSend("POST", "/sequence/ids/next", "tok", `{}`), fqSend("POST", "/sequence/ids/next", "tok", `{"count":10}`),
		fqSend("POST", "/sequence/ids/next", "tok", `{"count":0}`), fqSend("POST", "/sequence/ids/next", "utok", `{"count":5}`),
		fqSend("POST", "/sequence/a:b/next", "tok", `{}`),
		fqSend("POST", "/nodes/multiget", "utok", `{"addresses":["p:1","p:2","zz","b:1"]}`),
		fqSend("POST", "/nodes/query", "tok", `{"kind":"Post"}`),
		fqSend("POST", "/nodes/query", "tok", `{"kind":"Post","order":"n","desc":true,"limit":2}`),
		fqSend("POST", "/nodes/query", "tok", `{"kind":"Post","where":`+pred+`}`),
		fqSend("POST", "/nodes/query", "tok", `{"kind":"Post","where":{"kind":"bogus"}}`),
		fqSend("POST", "/nodes/count", "tok", `{"kind":"Post"}`), fqSend("POST", "/nodes/count", "utok", `{"kind":"Post"}`),
		fqSend("POST", "/nodes/count_by", "tok", `{"kind":"Post","group_by":"n"}`),
		fqSend("POST", "/nodes/count_by", "tok", `{"kind":"Post","group_by":""}`),
		fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Post","func":"sum","field":"n"}`),
		fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Post","func":"avg","field":"n"}`),
		fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Post","func":"median","field":"n"}`),
		fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Post","func":"sum"}`),
		fqSend("POST", "/nodes/aggregate", "tok", `{"kind":"Post","func":"count","field":"n"}`),
		fqSend("POST", "/nodes/aggregate_by", "tok", `{"kind":"Post","group_by":"t","func":"max","field":"n"}`),
		fqSend("POST", "/transaction", "tok", tx(ins("t:1", `{"v":1}`, ""), `{"type":"insert_edge","from":"t:1","to":"p:1","kind":"E"}`, `{"type":"set_if","address":"t:1","field":"v","expect_le":5,"set":{"v":2}}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"set_if","address":"t:1","field":"v","expect_eq":1,"set":{"v":3}}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"set_if","address":"t:1","field":"v","set":{"v":3}}`)),
		fqSend("POST", "/transaction", "utok", tx(ins("t:2", "{}", `,"owner":"x"`))),
		fqSend("POST", "/transaction", "utok", tx(`{"type":"delete_node","address":"p:1"}`)),
		fqSend("POST", "/transaction", "utok", tx(`{"type":"delete_node","address":"zz"}`)),
		fqSend("POST", "/transaction", "utok", tx(`{"type":"delete_edge","from":"t:1","to":"p:1","kind":"E"}`)),
		fqSend("POST", "/transaction", "utok", tx(`{"type":"delete_edge","from":"t:1","to":"p:1","kind":"NOPE"}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"delete_where","kind":"T","where":{"kind":"lit","val":true}}`, `{"type":"clear_kind","kind":"Nothing"}`)),
		fqSend("POST", "/transaction", "tok", tx(ins("t:3", "{}", ""), `{"type":"delete_node","address":"t:3"}`)),
		fqSend("POST", "/transaction", "tok", tx(ins("t:4", "{}", `,"public":true`), `{"type":"clear_kind","kind":"T"}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"bogus"}`, `{"type":"delete_node","address":7}`)),
		fqSend("POST", "/transaction", "tok", `{"operations":[{"type":"delete_node","address":7}, {"type":"x"}]}`),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"delete_node","type":"x","address":"a"}`)),
		fqSend("POST", "/transaction", "tok", tx(`5`)), fqSend("POST", "/transaction", "tok", tx(`{"type":3}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"address":"b"}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"insert_node","address":"b","kind":"k","x":1,"y":2,"z":3,"q":0}`)),
		fqSend("POST", "/transaction", "tok", tx(`{"type":"set_if","address":"a","field":"f","expect_le":"x"}`)),
		// serde's precedence: the first error in document order, whether
		// a type error or a syntax error; and the sequence forms.
		fqSend("POST", "/transaction", "tok", `{"operations":[{"type":"delete_node","address":7},{"type":"x"`),
		fqSend("POST", "/transaction", "tok", `{"operations":[{"type":"delete_node","address":"a"},{"type":"delete_node","address":tru}]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[{"type":"delete_node","type":"x"}, {"bad"}]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[["insert_node","s:1","S",1,2,3,4,"{}",true,null,null],["delete_node","s:1"],["clear_kind"]]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[["bogus"]]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[[]]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[["delete_node"]]}`),
		fqSend("POST", "/transaction", "tok", `[[{"type":"clear_kind","kind":"Nothing"}]]`),
		fqSend("POST", "/transaction", "tok", `[]`),
		fqSend("POST", "/transaction", "tok", `{"operations":[],"operations":[]}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[null, 3]}`),
		fqSend("POST", "/transaction", "tok", `{"x":[1,2,}`),
		fqSend("POST", "/transaction", "tok", `{"operations":[{"type":"clear_kind","kind":"Nothing"}]} x`),
		fqSend("POST", "/publish", "tok", `{"payload":"hello"}`), fqSend("POST", "/publish", "utok", `{"payload":"hi"}`),
		fqGet("/changes", "tok"), fqGet("/changes", "utok"), fqGet("/changes?after=3&limit=2", "tok"), fqGet("/changes?limit=0", "tok"),
		fqSend("POST", "/admin/users", "tok", `{"owner":"carol"}`), fqSend("POST", "/admin/users", "tok", `{"owner":"dave","role":"admin"}`),
		fqSend("POST", "/admin/users", "tok", `{"owner":"x","role":"root"}`), fqSend("POST", "/admin/users", "utok", `{"owner":"x"}`),
		fqGet("/admin/users", "tok"), {method: "DELETE", path: "/admin/users/carol", key: "tok"},
		{method: "DELETE", path: "/admin/users/carol", key: "tok"}, fqGet("/admin/users", "tok"),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"post_n","kind":"Post","field":"n"}`),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"post_n","kind":"Post","field":"n"}`),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"post_t","kind":"Post","field":"t","mode":"text"}`),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"u","kind":"Post","field":"t","mode":"text","unique":true}`),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"u2","kind":"Post","field":"q","mode":"hash"}`),
		fqSend("POST", "/admin/indexes", "tok", `{"name":"post_n2","kind":"Post","field":"n"}`),
		fqGet("/admin/indexes", "tok"), {method: "DELETE", path: "/admin/indexes/post_t", key: "tok"},
		{method: "DELETE", path: "/admin/indexes/post_t", key: "tok"}, fqGet("/admin/indexes", "tok"),
		fqSend("POST", "/admin/references", "tok", `{"name":"r1","kind":"Comment","field":"post","parent_kind":"Post","on_delete":"cascade"}`),
		fqSend("POST", "/admin/references", "tok", `{"name":"r1","kind":"Comment","field":"post","parent_kind":"Post","on_delete":"cascade"}`),
		fqSend("POST", "/admin/references", "tok", `{"name":"r2","kind":"Comment","field":"post","parent_kind":"Post","on_delete":"explode"}`),
		fqGet("/admin/references", "tok"), {method: "DELETE", path: "/admin/references/r1", key: "tok"},
		{method: "DELETE", path: "/admin/references/r1", key: "tok"},
		{method: "DELETE", path: "/node/p:2", key: "utok"}, {method: "DELETE", path: "/node/b:1", key: "utok"},
		{method: "DELETE", path: "/node/b:1", key: "utok"},
		fqGet("/node/p:3/history", "tok"), fqGet("/nodes?kind=T", "tok"),
		fqSend("POST", "/node", "tok", `not json at all`),
		{method: "POST", path: "/node", key: "tok", ctype: "text/plain", body: fqNode("a", "k", "d", ""), hasBody: true},
		fqSend("POST", "/node", "tok", `[1,2]`), fqSend("POST", "/node", "tok", `["a","k",1,2,3,4,"d"]`),
		fqSend("POST", "/node", "tok", `{"address":"a"} x`), fqSend("POST", "/node", "tok", fqNode("tr:1", "T", "{}", "")+` trailing`), fqSend("POST", "/node", "tok", ``),
		fqSend("POST", "/node", "tok", `{"address":"a","kind":"k","x":-1}`), fqSend("POST", "/node", "tok", `{"address":"a","kind":"k","x":1.5}`),
		fqSend("POST", "/node", "tok", `{"address":"a","kind":"k","x":1,"y":2,"z":300,"q":0,"data":"d","x":1}`),
		fqSend("POST", "/node", "tok", `{"address":"a","kind":"k","x":1,"y":2,"z":3,"q":0,"data":"d","x":1}`),
		fqSend("POST", "/node", "tok", `{"address":"a","kind":"k","x":1,"y":2,"z":3,"q":0,"data":"d","edges":[{"to":"b"}]}`),
		fqSend("POST", "/node", "tok", `{"address":5,"kind":`), fqSend("POST", "/node", "tok", `{"a":`),
		fqSend("POST", "/nodes/query", "tok", `{"where":{"kind":"lit","args":[{"nokind":1}]}}`),
		fqGet("/nodes?key=tok", ""), fqGet("/nodes?key=bad", ""),
		fqGet("/events?after=0", "tok"),
	}
}

func TestFqServerBothWays(t *testing.T) {
	rp, fp := fqServerPair(t, "FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off", "FACETQL_RATE_SUBSCRIBE=off")

	// Two live subscribers, opened before anything is published.
	openStream := func(port int, path, key string) net.Conn {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nx-api-key: %s\r\n\r\n", path, key)
		return c
	}
	streams := map[int][2]net.Conn{}
	for _, p := range []int{rp, fp} {
		streams[p] = [2]net.Conn{openStream(p, "/events", "tok"), openStream(p, "/events", "utok")}
	}
	time.Sleep(300 * time.Millisecond)

	for i, c := range fqServerScript() {
		raw := c.raw()
		want := fqNorm(fqRaw(t, rp, raw, 300*time.Millisecond))
		got := fqNorm(fqRaw(t, fp, raw, 300*time.Millisecond))
		if c.method == "GET" && c.path == "/admin/users" {
			want, got = fqSortedUsers(want), fqSortedUsers(got)
		}
		if got != want {
			t.Errorf("request %d: %s %s %s\n--- facetql\n%s\n--- fqserver.fct\n%s", i, c.method, c.path, c.body, want, got)
		}
	}

	// Every event each subscriber was sent, framed as axum frames it.
	drain := func(c net.Conn) string {
		var out []byte
		buf := make([]byte, 65536)
		for {
			_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				break
			}
		}
		c.Close()
		return fqNorm(string(out))
	}
	for k := 0; k < 2; k++ {
		want, got := drain(streams[rp][k]), drain(streams[fp][k])
		if got != want {
			t.Errorf("event stream %d:\n--- facetql\n%s\n--- fqserver.fct\n%s", k, want, got)
		}
		if !strings.Contains(want, "data: ") {
			t.Errorf("event stream %d carried no events:\n%s", k, want)
		}
	}
	// A resume replays under the same filter.
	for _, path := range []string{"/events?after=3", "/events?after=12"} {
		want := fqNorm(fqRaw(t, rp, "GET "+path+" HTTP/1.1\r\nHost: x\r\nx-api-key: utok\r\n\r\n", 500*time.Millisecond))
		got := fqNorm(fqRaw(t, fp, "GET "+path+" HTTP/1.1\r\nHost: x\r\nx-api-key: utok\r\n\r\n", 500*time.Millisecond))
		if got != want {
			t.Errorf("resume %s:\n--- facetql\n%s\n--- fqserver.fct\n%s", path, want, got)
		}
	}

	// GET /stats: the engine's half identically; the runtime half in the
	// same shape, with the same request counters.
	stats := func(port int) map[string]any {
		resp := fqRaw(t, port, "GET /stats HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\n\r\n", 0)
		var m map[string]any
		if err := json.Unmarshal([]byte(resp[strings.Index(resp, "\r\n\r\n")+4:]), &m); err != nil {
			t.Fatalf("stats: %v\n%s", err, resp)
		}
		return m
	}
	ws, gs := stats(rp), stats(fp)
	wr, gr := ws["runtime"].(map[string]any), gs["runtime"].(map[string]any)
	delete(ws, "runtime")
	delete(gs, "runtime")
	if !reflect.DeepEqual(ws, gs) {
		t.Errorf("stats:\n--- facetql\n%v\n--- fqserver.fct\n%v", ws, gs)
	}
	if !reflect.DeepEqual(wr["requests"], gr["requests"]) {
		t.Errorf("stats.runtime.requests:\n--- facetql\n%v\n--- fqserver.fct\n%v", wr["requests"], gr["requests"])
	}
	for _, k := range []string{"uptime_seconds", "window", "process"} {
		if _, ok := gr[k]; !ok {
			t.Errorf("stats.runtime lacks %q", k)
		}
	}
	// The process section is measured, not left null: the same fields,
	// each the same JSON type as facetql's.
	wp, gp := wr["process"].(map[string]any), gr["process"].(map[string]any)
	for k, v := range wp {
		if fmt.Sprintf("%T", gp[k]) != fmt.Sprintf("%T", v) {
			t.Errorf("stats.runtime.process.%s: facetql %v, fqserver.fct %v", k, v, gp[k])
		}
	}
}

// TestFqServerLimitsBothWays: the guards — the per-identity token buckets
// (429 with Retry-After) and the body limit, which axum applies only when a
// handler reads the body (so a GET with an oversized body is served, and
// the connection is still good afterwards).
func TestFqServerLimitsBothWays(t *testing.T) {
	rp, fp := fqServerPair(t, "FACETQL_RATE_WRITE=2:0.5", "FACETQL_MAX_BODY_BYTES=100")
	big := fqNode("a", "k", strings.Repeat("x", 200), "")
	cases := []fqCase{
		fqSend("POST", "/node", "tok", big),
		{method: "POST", path: "/node", key: "tok", ctype: "text/plain", body: big, hasBody: true},
		{method: "GET", path: "/nodes", key: "tok", body: big, hasBody: true},
		fqSend("POST", "/publish", "utok", `{"payload":"p"}`),
		fqSend("POST", "/publish", "utok", `{"payload":"p"}`),
		fqSend("POST", "/publish", "utok", `{"payload":"p"}`),
		fqSend("POST", "/edge", "utok", `{"from":"a","to":"b","kind":"c"}`),
		fqGet("/nodes", "utok"),
	}
	for i, c := range cases {
		want := fqNorm(fqRaw(t, rp, c.raw(), 0))
		got := fqNorm(fqRaw(t, fp, c.raw(), 0))
		if got != want {
			t.Errorf("request %d: %s %s\n--- facetql\n%s\n--- fqserver.fct\n%s", i, c.method, c.path, want, got)
		}
	}
	// Keep-alive through an oversized body: the answer, then the next
	// request on the same connection.
	pipelined := fqSend("POST", "/nodes/query", "tok", `{"kind":"`+strings.Repeat("k", 200)+`"}`).raw() + fqGet("/nodes", "tok").raw()
	for _, p := range []int{rp, fp} {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(c, pipelined)
		var out []byte
		buf := make([]byte, 65536)
		for strings.Count(string(out), "HTTP/1.1 ") < 2 || !strings.HasSuffix(string(out), "[]") {
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			n, err := c.Read(buf)
			out = append(out, buf[:n]...)
			if err != nil {
				t.Fatalf("port %d: %v after %q", p, err, out)
			}
		}
		c.Close()
		if !strings.Contains(string(out), "413 Payload Too Large") || !strings.Contains(string(out), "200 OK") {
			t.Errorf("port %d: pipelined answers %q", p, out)
		}
	}
}

// fqSortedUsers sorts a GET /admin/users answer by owner.
func fqSortedUsers(norm string) string {
	i := strings.Index(norm, "\n\n")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(norm[i+2:]), &rows); err != nil {
		return norm
	}
	sort.Slice(rows, func(a, b int) bool { return fmt.Sprint(rows[a]["owner"]) < fmt.Sprint(rows[b]["owner"]) })
	b, _ := json.Marshal(rows)
	return norm[:i+2] + string(b)
}

// TestFqServerInFlightAndTimeoutBothWays: a request holds its in-flight
// slot from admission until its answer — including while its handler waits
// for a body — so a second request is refused with the concurrency 503;
// and a request whose handler has not answered within the request timeout
// (here, a body that never finishes arriving) is answered 408, after which
// the slot is free again.
func TestFqServerInFlightAndTimeoutBothWays(t *testing.T) {
	rp, fp := fqServerPair(t, "FACETQL_MAX_CONCURRENT_REQUESTS=1", "FACETQL_REQUEST_TIMEOUT_SECS=1", "FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off")
	stalled := "POST /node HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"address\":"
	type pairResult struct{ busy, timedOut, after string }
	run := func(port int) pairResult {
		c, err := fqDial(port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.Write([]byte(stalled)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		busy := fqNorm(fqRaw(t, port, fqGet("/nodes", "tok").raw(), 0))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 4096)
		var got []byte
		for !strings.Contains(string(got), "\r\n\r\n") {
			n, err := c.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		// the 408's head, minus how each server leaves a connection whose
		// body it never read
		norm := fqNorm(string(got))
		var kept []string
		for _, l := range strings.Split(norm, "\n") {
			if !strings.HasPrefix(l, "connection:") {
				kept = append(kept, l)
			}
		}
		after := fqNorm(fqRaw(t, port, fqGet("/nodes", "tok").raw(), 0))
		return pairResult{busy, strings.Join(kept, "\n"), after}
	}
	want, got := run(rp), run(fp)
	if got != want {
		t.Errorf("in flight / timeout:\n--- facetql\n%+v\n--- fqserver.fct\n%+v", want, got)
	}
	if !strings.HasPrefix(want.busy, "HTTP/1.1 503") || !strings.HasPrefix(want.timedOut, "HTTP/1.1 408") || !strings.HasPrefix(want.after, "HTTP/1.1 200") {
		t.Errorf("unexpected facetql answers: %+v", want)
	}
}

// TestFqServerStreamKeepAliveBothWays: an idle event stream carries
// axum's keep-alive comment 15 s after the last thing written to it.
func TestFqServerStreamKeepAliveBothWays(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 15 s keep-alive")
	}
	rp, fp := fqServerPair(t)
	read := func(port int, out chan<- string) {
		out <- fqNorm(fqRaw(t, port, "GET /events HTTP/1.1\r\nHost: x\r\nx-api-key: tok\r\n\r\n", 16*time.Second))
	}
	wc, gc := make(chan string, 1), make(chan string, 1)
	go read(rp, wc)
	go read(fp, gc)
	want, got := <-wc, <-gc
	if got != want {
		t.Errorf("idle stream:\n--- facetql\n%q\n--- fqserver.fct\n%q", want, got)
	}
	if !strings.Contains(want, "3\r\n:\n\n\r\n") {
		t.Errorf("no keep-alive in %q", want)
	}
}

// TestFqServerTLSBothWays: served over TLS from a PKCS#12 identity, as
// `facetql start --tls-identity … --tls-identity-password …` serves it,
// the same answers come back.
func TestFqServerTLSBothWays(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	idDir := t.TempDir()
	key, cert, p12 := filepath.Join(idDir, "id.key"), filepath.Join(idDir, "id.crt"), filepath.Join(idDir, "id.p12")
	for _, args := range [][]string{
		{"req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", cert, "-days", "2", "-subj", "/CN=localhost"},
		{"pkcs12", "-export", "-inkey", key, "-in", cert, "-out", p12, "-passout", "pass:pw"},
	} {
		if out, err := exec.Command(openssl, args...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	raw, err := os.ReadFile(p12)
	if err != nil {
		t.Fatal(err)
	}
	fqUseTLS = true
	defer func() { fqUseTLS = false }()
	rp, fp := fqServerPairWith(t, func(dir string) []string {
		path := filepath.Join(dir, "identity.p12")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return []string{"FACETQL_TLS_IDENTITY=" + path, "FACETQL_TLS_IDENTITY_PASSWORD=pw"}
	})
	for i, c := range []fqCase{
		fqGet("/", ""), fqGet("/stats", "bad"),
		fqSend("POST", "/node", "tok", fqNode("t:1", "T", `{"n":1}`, "")),
		fqGet("/node/t:1", "tok"), fqGet("/nodes?kind=T", "utok"),
	} {
		want := fqNorm(fqRaw(t, rp, c.raw(), 0))
		got := fqNorm(fqRaw(t, fp, c.raw(), 0))
		if got != want {
			t.Errorf("request %d over TLS: %s %s\n--- facetql\n%s\n--- fqserver.fct\n%s", i, c.method, c.path, want, got)
		}
	}
}

// fqFacetqlStart: facetql's own `facetql start` (the release binary beside
// fct), for what only its main.rs does — the posture refusal, startup
// failures and the SIGTERM shutdown. Skipped when it is absent or older
// than its source.
func fqFacetqlStart(t *testing.T) string {
	t.Helper()
	bin, err := filepath.Abs("../../facetql/target/release/facetql")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Skip("no facetql release binary")
	}
	stale := ""
	_ = filepath.Walk("../../facetql/src", func(path string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(path, ".rs") && fi.ModTime().After(info.ModTime()) {
			stale = path
		}
		return nil
	})
	if stale != "" {
		t.Skipf("facetql release binary is older than %s", stale)
	}
	return bin
}

type fqProc struct {
	cmd            *exec.Cmd
	stdout, stderr *strings.Builder
	done           chan struct{}
}

// fqStartProc starts one server — "rust" (`facetql start`) or "fct" —
// in dir with env, without waiting for it.
func fqStartProc(t *testing.T, which, dir string, port int, env ...string) *fqProc {
	t.Helper()
	var cmd *exec.Cmd
	if which == "rust" {
		cmd = exec.Command(fqFacetqlStart(t), "start")
	} else {
		server, _ := filepath.Abs("fqserver.fct")
		cmd = exec.Command(fqServerFacet(t), "exec", server)
		env = append(env, "FACET_DATA_DIR="+dir)
	}
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), env...), "FACETQL_DATA_DIR="+dir, fmt.Sprintf("FACETQL_PORT=%d", port))
	p := &fqProc{cmd: cmd, stdout: &strings.Builder{}, stderr: &strings.Builder{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
	})
	return p
}

// fqExit waits for the process to end and answers its exit status.
func (p *fqProc) fqExit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("still running; stdout %q stderr %q", p.stdout.String(), p.stderr.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

func fqWaitListening(t *testing.T, p *fqProc, port int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return
		}
		select {
		case <-p.done:
			t.Fatalf("exited: %q %q", p.stdout.String(), p.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("never listened")
}

// TestFqServerShutdownBothWays: SIGTERM finishes what is in flight, takes
// the last checkpoint, says so, and exits 0; what was written is there on
// the next start.
func TestFqServerShutdownBothWays(t *testing.T) {
	env := []string{"FACETQL_TOKENS=tok:alice:admin", "FACETQL_ENV=development", "FACETQL_MASTER_KEY=" + facetqlCheckKey}
	outcome := map[string]string{}
	for _, which := range []string{"rust", "fct"} {
		dir := t.TempDir()
		port := fqFreePort(t)
		p := fqStartProc(t, which, dir, port, env...)
		fqWaitListening(t, p, port)
		if r := fqRaw(t, port, fqSend("POST", "/node", "tok", fqNode("s:1", "S", "{}", "")).raw(), 0); !strings.HasPrefix(r, "HTTP/1.1 201") {
			t.Fatalf("%s: %q", which, r)
		}
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		code := p.fqExit(t)
		var lines []string
		for _, l := range strings.Split(p.stdout.String(), "\n") {
			if strings.HasPrefix(l, "FacetQL: ") {
				lines = append(lines, l)
			}
		}
		port2 := fqFreePort(t)
		p2 := fqStartProc(t, which, dir, port2, env...)
		fqWaitListening(t, p2, port2)
		again := fqNorm(fqRaw(t, port2, fqGet("/node/s:1", "tok").raw(), 0))
		outcome[which] = fmt.Sprintf("exit %d\n%s\n%s", code, strings.Join(lines, "\n"), again)
	}
	t.Logf("facetql:\n%s", outcome["rust"])
	if outcome["fct"] != outcome["rust"] {
		t.Errorf("shutdown:\n--- facetql\n%s\n--- fqserver.fct\n%s", outcome["rust"], outcome["fct"])
	}
	if !strings.HasPrefix(outcome["rust"], "exit 0\nFacetQL: shutdown signal received") {
		t.Errorf("facetql: %s", outcome["rust"])
	}
}

// TestFqServerRefusalsBothWays: a server that will not start says why, in
// facetql's words, and exits with its status — 7 for a production posture
// with development credentials, 4 for data that does not authenticate
// under the configured key.
func TestFqServerRefusalsBothWays(t *testing.T) {
	outcome := map[string]string{}
	for _, which := range []string{"rust", "fct"} {
		dir := t.TempDir()
		p := fqStartProc(t, which, dir, fqFreePort(t))
		code := p.fqExit(t)
		posture := fmt.Sprintf("exit %d\n%s", code, p.stderr.String())

		env := []string{"FACETQL_TOKENS=tok:alice:admin", "FACETQL_ENV=development"}
		port := fqFreePort(t)
		w := fqStartProc(t, which, dir, port, append(env, "FACETQL_MASTER_KEY="+facetqlCheckKey)...)
		fqWaitListening(t, w, port)
		fqRaw(t, port, fqSend("POST", "/node", "tok", fqNode("k:1", "K", "{}", "")).raw(), 0)
		_ = w.cmd.Process.Signal(syscall.SIGTERM)
		w.fqExit(t)
		other := strings.Repeat("ab", 32)
		r := fqStartProc(t, which, dir, fqFreePort(t), append(env, "FACETQL_MASTER_KEY="+other)...)
		code2 := r.fqExit(t)
		stderr := strings.ReplaceAll(r.stderr.String(), dir, "<dir>")
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
		outcome[which] = posture + fmt.Sprintf("\n---\nexit %d\n%s", code2, strings.Join(failure, "\n"))
	}
	t.Logf("facetql:\n%s", outcome["rust"])
	if outcome["fct"] != outcome["rust"] {
		t.Errorf("refusals:\n--- facetql\n%s\n--- fqserver.fct\n%s", outcome["rust"], outcome["fct"])
	}
}
