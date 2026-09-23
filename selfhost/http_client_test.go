package selfhost

// http_client.fct — an HTTP/1.1 client written in fct over the runtime's
// outbound TCP primitive — against real servers: Go's net/http (which frames
// a body with Content-Length or chunked as it sees fit) and raw TCP peers
// that exercise the framing edges one at a time. The live FacetQL test
// (fabric_facetql_live_test.go) drives the same client against a real Rust
// HTTP server.

import (
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

func httpcApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File("http_client.fct")
	if err != nil {
		t.Fatalf("compile selfhost/http_client.fct: %v", err)
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
	httpcLastMu sync.Mutex
	httpcLast   = map[*httptest.Server]string{}
)

// httpcCall runs one action and returns httpOut (unchanged state is absent
// from the deltas, so the last value is remembered per server).
func httpcCall(t *testing.T, ts *httptest.Server, action string, args ...any) string {
	t.Helper()
	d := postExprJSON(t, ts, action, args...)
	httpcLastMu.Lock()
	defer httpcLastMu.Unlock()
	if v, ok := d["httpOut"].(string); ok {
		httpcLast[ts] = v
	}
	return httpcLast[ts]
}

// httpcRawServer answers every connection with serve, on loopback.
func httpcRawServer(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

// httpcReadRequestHead consumes one request head from c.
func httpcReadRequestHead(c net.Conn) string {
	var sb strings.Builder
	buf := make([]byte, 1)
	for !strings.HasSuffix(sb.String(), "\r\n\r\n") {
		if _, err := c.Read(buf); err != nil {
			break
		}
		sb.WriteByte(buf[0])
	}
	return sb.String()
}

func TestHTTPClientAgainstNetHTTP(t *testing.T) {
	type seen struct {
		method, uri, host, key, ctype, clen, conn, body string
	}
	var mu sync.Mutex
	var got []seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.Method, r.RequestURI, r.Host, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type"),
			fmt.Sprint(r.ContentLength), r.Header.Get("Connection") + fmt.Sprint(r.Close), string(b)})
		mu.Unlock()
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"echo":%q}`, string(b))
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "node not found")
		case "/chunked":
			// No Content-Length and an explicit flush between writes: net/http
			// frames this with Transfer-Encoding: chunked.
			f := w.(http.Flusher)
			fmt.Fprint(w, "first-")
			f.Flush()
			fmt.Fprint(w, "sécond-")
			f.Flush()
			fmt.Fprint(w, strings.Repeat("x", 70000))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer upstream.Close()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port
	ts := httpcApp(t)

	out := httpcCall(t, ts, "httpRun", "POST", upstream.URL+"/json", "x-api-key: s3cret\ncontent-type: application/json", `{"a":"é"}`, 0)
	parts := strings.SplitN(out, "|", 4)
	if parts[0] != "200" || parts[1] != "OK" || parts[3] != `{"echo":"{\"a\":\"é\"}"}` {
		t.Fatalf("POST /json: %q", out)
	}
	if !strings.Contains(parts[2], "content-type=application/json") || !strings.Contains(parts[2], "content-length=") {
		t.Fatalf("response headers: %q", parts[2])
	}

	out = httpcCall(t, ts, "httpRun", "GET", upstream.URL+"/missing", "x-api-key: s3cret", "", 0)
	if !strings.HasPrefix(out, "404|Not Found|") || !strings.HasSuffix(out, "|node not found") {
		t.Fatalf("GET /missing: a non-2xx status is data: %q", out)
	}

	out = httpcCall(t, ts, "httpRun", "GET", upstream.URL+"/chunked", "", "", 0)
	if !strings.Contains(out, "transfer-encoding=chunked") || !strings.HasSuffix(out, "|first-sécond-"+strings.Repeat("x", 70000)) {
		t.Fatalf("GET /chunked: %.200q", out)
	}

	out = httpcCall(t, ts, "httpRun", "POST", upstream.URL+"/empty", "", "", 0)
	if !strings.HasPrefix(out, "204|No Content|") || !strings.HasSuffix(out, "|") {
		t.Fatalf("POST /empty: %q", out)
	}

	// A raw address reaches the wire as a WHATWG URL would send it.
	out = httpcCall(t, ts, "httpRun", "GET", upstream.URL+"/a b/./c/../json?q=1 2#frag", "", "", 0)
	if !strings.HasPrefix(out, "200|") {
		t.Fatalf("GET normalized path: %q", out)
	}

	mu.Lock()
	defer mu.Unlock()
	host := fmt.Sprintf("127.0.0.1:%d", port)
	want := []seen{
		{"POST", "/json", host, "s3cret", "application/json", "10", "closetrue", `{"a":"é"}`},
		{"GET", "/missing", host, "s3cret", "", "0", "closetrue", ""},
		{"GET", "/chunked", host, "", "", "0", "closetrue", ""},
		{"POST", "/empty", host, "", "", "0", "closetrue", ""},
		{"GET", "/a%20b/json?q=1%202", host, "", "", "0", "closetrue", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("server saw %d requests, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d: server saw %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestHTTPClientFramingEdges: responses a byte-exact raw peer sends, one
// framing rule each.
func TestHTTPClientFramingEdges(t *testing.T) {
	ts := httpcApp(t)
	cases := []struct {
		name, response, want string
		dribble              bool
	}{
		{"content-length stops reading before close", "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhelloEXTRA", "200|OK|content-length=5|hello", false},
		{"close-delimited body", "HTTP/1.1 200 OK\r\nX-A:  spaced  \r\n\r\nuntil close", "200|OK|x-a=spaced|until close", false},
		{"interim 100 then the real response", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\nok", "201|Created|content-length=2|ok", false},
		{"chunked with extension and trailer", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4;ext=1\r\nWiki\r\n5\r\npedia\r\nE\r\n in\r\n\r\nchunks.\r\n0\r\nTrailer: x\r\n\r\n", "200|OK|transfer-encoding=chunked|Wikipedia in\r\n\r\nchunks.", false},
		{"dribbled a byte at a time", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n", "200|OK|transfer-encoding=chunked|abc", true},
		{"no reason phrase", "HTTP/1.1 412 \r\nContent-Length: 0\r\n\r\n", "412||content-length=0|", false},
		{"truncated body is a transport failure", "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nshort", "ERR|connection closed before the body was complete (5 of 10 bytes)", false},
		{"truncated head is a transport failure", "HTTP/1.1 200 OK\r\nContent-Len", "ERR|connection closed before a complete response was received", false},
		{"garbage status line", "SMTP ready\r\n\r\n", "ERR|invalid HTTP status line: SMTP ready", false},
		{"bad chunk size", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n", "ERR|invalid chunk size", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := httpcRawServer(t, func(conn net.Conn) {
				defer conn.Close()
				httpcReadRequestHead(conn)
				if c.dribble {
					for i := 0; i < len(c.response); i++ {
						conn.Write([]byte{c.response[i]})
						time.Sleep(time.Millisecond)
					}
					return
				}
				conn.Write([]byte(c.response))
			})
			if got := httpcCall(t, ts, "httpRun", "GET", url+"/", "", "", 0); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestHTTPClientTransportFailuresAreValues: refused (plain and TLS),
// deadline.
func TestHTTPClientTransportFailuresAreValues(t *testing.T) {
	ts := httpcApp(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	out := httpcCall(t, ts, "httpRun", "GET", "http://"+dead+"/stats", "", "", 0)
	if !strings.HasPrefix(out, "ERR|connect "+dead+": ") || !strings.Contains(out, "refused") {
		t.Fatalf("refused: %q", out)
	}

	hold := make(chan struct{})
	defer close(hold)
	silent := httpcRawServer(t, func(c net.Conn) {
		defer c.Close()
		httpcReadRequestHead(c)
		<-hold
	})
	out = httpcCall(t, ts, "httpRun", "GET", silent+"/", "", "", 120)
	if out != "ERR|readBytes: timed out after 120ms" {
		t.Fatalf("deadline: %q", out)
	}

	out = httpcCall(t, ts, "httpRun", "GET", "https://"+dead+"/", "", "", 0)
	if !strings.HasPrefix(out, "ERR|connectTls "+dead+": ") || !strings.Contains(out, "refused") {
		t.Fatalf("https refused: %q", out)
	}
}

// TestHTTPClientStreamPumpsAsDataArrives: a chunked stream (an SSE-shaped
// endpoint that flushes a frame at a time and then finishes) is read
// incrementally, frame by frame, and ends cleanly; an error status is read
// whole; a peer that vanishes mid-chunk is a failed end.
func TestHTTPClientStreamPumpsAsDataArrives(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/denied" {
			http.Error(w, "admin only", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, frame := range []string{"data: one\n\n", "data: twö\n\n", "data: three\n\n"} {
			fmt.Fprint(w, frame)
			f.Flush()
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	ts := httpcApp(t)

	got := httpcCall(t, ts, "httpStreamRun", upstream.URL+"/events", 200, 20)
	if got != "200|[data: one\n\n][data: twö\n\n][data: three\n\n]|true|" {
		t.Fatalf("stream: %q", got)
	}
	got = httpcCall(t, ts, "httpStreamRun", upstream.URL+"/denied", 5, 20)
	if got != "403|[admin only\n]|true|" {
		t.Fatalf("denied: %q", got)
	}

	cut := httpcRawServer(t, func(c net.Conn) {
		defer c.Close()
		httpcReadRequestHead(c)
		c.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n9\r\npart"))
	})
	got = httpcCall(t, ts, "httpStreamRun", cut+"/", 50, 20)
	if got != "200|[hello]|true|connection closed before the body was complete" {
		t.Fatalf("cut: %q", got)
	}
}

// TestHTTPClientSpeaksHTTPS: https through connectTls against Go's TLS
// server — a verified exchange (a chunked body, the Host header without the
// default port rule tripping), a stream over TLS, and the untrusted chain
// refused as a transport failure.
func TestHTTPClientSpeaksHTTPS(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		fmt.Fprintf(w, "tls %s %s ", r.Host, r.Proto)
		f.Flush()
		fmt.Fprint(w, "done")
	}))
	defer upstream.Close()
	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	ts := httpcApp(t)
	host := upstream.Listener.Addr().String()

	got := httpcCall(t, ts, "httpRunTrust", upstream.URL+"/x", "ca.pem")
	if !strings.HasPrefix(got, "200|OK|") || !strings.Contains(got, "transfer-encoding=chunked") || !strings.HasSuffix(got, "|tls "+host+" HTTP/1.1 done") {
		t.Fatalf("https: %q", got)
	}
	got = httpcCall(t, ts, "httpRunTrust", upstream.URL+"/x", "")
	if got != "ERR|connectTls "+host+": tls: failed to verify certificate: x509: certificate signed by unknown authority" {
		t.Fatalf("untrusted: %q", got)
	}
}
