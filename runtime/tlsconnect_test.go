package runtime

import (
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
)

// TestConnectTls: a verified exchange when the server's CA is in the trust
// file, and each refusal — untrusted chain, wrong name, a peer that does
// not speak TLS — reported through connError without aborting; an
// unreadable trust file is a configuration error that does abort.
func TestConnectTls(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello over tls")
	}))
	defer upstream.Close()
	port := upstream.Listener.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	t.Setenv("FACET_DATA_DIR", dir)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := netConnectListen(t, func(c net.Conn) {
		defer c.Close()
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
	})

	g, err := compile.File("testdata/tlsconnect.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	get := func(port int, name, trust string) string {
		t.Helper()
		d := postJSON(t, ts, "get", fmt.Sprintf(`{"args":["127.0.0.1",%d,%q,%q]}`, port, name, trust))
		return fmt.Sprint(d["result"])
	}

	if got := get(port, "", "ca.pem"); got != "HTTP/1.1 200|true|" {
		t.Errorf("trusted: %q", got)
	}
	// httptest's certificate also names example.com.
	if got := get(port, "example.com", "ca.pem"); got != "HTTP/1.1 200|true|" {
		t.Errorf("trusted by DNS name: %q", got)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for _, c := range []struct{ name, trust, want string }{
		{"", "", "|false|connectTls " + addr + ": tls: failed to verify certificate: x509: certificate signed by unknown authority"},
		{"wrong.example", "ca.pem", "|false|connectTls " + addr + ": tls: failed to verify certificate: x509: certificate is valid for example.com, *.example.com, not wrong.example"},
	} {
		if got := get(port, c.name, c.trust); got != c.want {
			t.Errorf("name %q trust %q:\n got %q\nwant %q", c.name, c.trust, got, c.want)
		}
	}
	if got := get(plain, "", "ca.pem"); !strings.HasPrefix(got, fmt.Sprintf("|false|connectTls 127.0.0.1:%d: ", plain)) {
		t.Errorf("plain peer: %q", got)
	}

	resp, err := postRaw(t, ts, "get", fmt.Sprintf(`{"args":["127.0.0.1",%d,"","junk.pem"]}`, port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := readBody(t, resp)
	if resp.StatusCode < 400 || !strings.Contains(b, "trust file junk.pem holds no PEM certificate") {
		t.Errorf("junk trust file: %d %s", resp.StatusCode, b)
	}
}
