package selfhost

// http_server.fct — an HTTP/1.1 server written in fct — against Go's own
// HTTP/1.1 implementation: raw requests (pipelined, chunked, HTTP/1.0,
// oversized, malformed) read back with net/http's response parser, and
// net/http's client talking to it over a kept-alive connection. The fixture
// (testdata/http_server/echo.fct) answers every request with what was parsed
// out of it, served by a daemon that detaches each connection.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

func hsrvStart(t *testing.T) string {
	t.Helper()
	g, err := compile.File("testdata/http_server/echo.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	postExprJSON(t, ts, "echoStart", port)
	srv.StartJobs()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("the echo server never listened: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// hsrvConverse writes raw to a fresh connection and reads n responses.
func hsrvConverse(t *testing.T, addr, raw string, n int) ([]*http.Response, []string, *bufio.Reader, net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	var resps []*http.Response
	var bodies []string
	for i := 0; i < n; i++ {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("response %d body: %v", i, err)
		}
		resps = append(resps, resp)
		bodies = append(bodies, string(b))
	}
	return resps, bodies, r, c
}

func TestHTTPServerPipelinedKeepAlive(t *testing.T) {
	addr := hsrvStart(t)
	resps, bodies, _, _ := hsrvConverse(t, addr,
		"GET /a?x=1&y=%2F HTTP/1.1\r\nHost: h\r\nX-Api-Key: k\r\n\r\n"+
			"POST /b HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\n\r\nhello", 2)
	if resps[0].Status != "200 OK" || resps[0].Header.Get("Date") == "" || resps[0].ContentLength != int64(len(bodies[0])) {
		t.Fatalf("first response: %s %v", resps[0].Status, resps[0].Header)
	}
	if want := "GET|/a?x=1&y=%2F|/a|true|x=1&y=%2F|HTTP/1.1|host=h;x-api-key=k;|"; bodies[0] != want {
		t.Errorf("first = %q, want %q", bodies[0], want)
	}
	if want := "POST|/b|/b|false||HTTP/1.1|host=h;content-length=5;|hello"; bodies[1] != want {
		t.Errorf("second = %q, want %q", bodies[1], want)
	}
	if _, err := time.Parse(http.TimeFormat, resps[0].Header.Get("Date")); err != nil {
		t.Errorf("date %q: %v", resps[0].Header.Get("Date"), err)
	}
}

func TestHTTPServerChunkedRequestBody(t *testing.T) {
	addr := hsrvStart(t)
	_, bodies, _, _ := hsrvConverse(t, addr,
		"POST /c HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2;ext=1\r\nde\r\n0\r\nX-Trailer: t\r\n\r\n"+
			"GET /after HTTP/1.1\r\nHost: h\r\n\r\n", 2)
	if !strings.HasSuffix(bodies[0], "|abcde") {
		t.Errorf("chunked body = %q", bodies[0])
	}
	if !strings.HasPrefix(bodies[1], "GET|/after|") {
		t.Errorf("the request after a chunked body = %q", bodies[1])
	}
}

func TestHTTPServerHTTP10Closes(t *testing.T) {
	addr := hsrvStart(t)
	resps, _, r, _ := hsrvConverse(t, addr, "GET /old HTTP/1.0\r\n\r\n", 1)
	if !resps[0].Close {
		t.Errorf("an HTTP/1.0 response without keep-alive should close: %v", resps[0].Header)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Errorf("connection not closed after HTTP/1.0: %v", err)
	}
}

func TestHTTPServerRefusals(t *testing.T) {
	addr := hsrvStart(t)
	resps, bodies, r, _ := hsrvConverse(t, addr, "POST /big HTTP/1.1\r\nHost: h\r\nContent-Length: 17\r\n\r\n0123456789abcdefg", 1)
	if resps[0].StatusCode != 413 || bodies[0] != "too large" || !resps[0].Close {
		t.Errorf("oversized = %d %q close=%v", resps[0].StatusCode, bodies[0], resps[0].Close)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Errorf("connection not closed after 413: %v", err)
	}
	resps, bodies, _, _ = hsrvConverse(t, addr, "POST /big HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n10\r\n0123456789abcdef\r\n1\r\nx\r\n0\r\n\r\n", 1)
	if resps[0].StatusCode != 413 {
		t.Errorf("oversized chunked = %d %q", resps[0].StatusCode, bodies[0])
	}
	resps, bodies, _, _ = hsrvConverse(t, addr, "NONSENSE\r\n\r\n", 1)
	if resps[0].StatusCode != 400 || !strings.HasPrefix(bodies[0], "bad request: ") {
		t.Errorf("malformed = %d %q", resps[0].StatusCode, bodies[0])
	}
}

func TestHTTPServerWithNetHTTPClient(t *testing.T) {
	addr := hsrvStart(t)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for i := 0; i < 3; i++ {
		resp, err := client.Post("http://"+addr+"/p", "text/plain", strings.NewReader(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.HasPrefix(string(b), "POST|/p|") || !strings.HasSuffix(string(b), fmt.Sprintf("|%d", i)) {
			t.Errorf("request %d = %q", i, b)
		}
	}
	resp, err := client.Get("http://" + addr + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" || len(resp.TransferEncoding) != 1 || string(b) != "data: one\n\ndata: twö\n\n" {
		t.Errorf("stream = %v %v %q", resp.Header, resp.TransferEncoding, b)
	}
}
