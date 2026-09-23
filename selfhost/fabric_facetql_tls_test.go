package selfhost

// https, end to end: the fct FacetQL client (connectTls under
// http_client.fct) against a real FacetQL serving TLS from a PKCS#12
// identity (--tls-identity, via FACETQL_TLS_IDENTITY), against Team 4's fct
// FacetQL server (fqserver.fct's listenTls) doing the same, and — for every
// way TLS can fail — the same GET /stats through the real Rust client
// (reqwest, rustls) so the error classification is compared, not assumed.
//
// The identity is made with openssl in a temp dir, as an operator makes
// one: a throwaway CA, and a server certificate for DNS:localhost signed by
// it. Both clients trust that CA explicitly — reqwest through
// add_root_certificate, the port through its trust file — never by turning
// verification off. Skips when cargo or openssl is absent.

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fqlTlsPKI writes ca.pem and srv.p12 (password "pw", a certificate for
// DNS:localhost only) into dir.
func fqlTlsPKI(t *testing.T, dir string) (caPEM, p12 string) {
	t.Helper()
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(openssl, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "srv.ext"), []byte("subjectAltName=DNS:localhost\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key", "-out", "ca.pem", "-days", "2",
		"-subj", "/CN=fct-test-ca", "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign")
	run("req", "-newkey", "rsa:2048", "-nodes", "-keyout", "srv.key", "-out", "srv.csr", "-subj", "/CN=localhost")
	run("x509", "-req", "-in", "srv.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial", "-out", "srv.pem", "-days", "2", "-extfile", "srv.ext")
	run("pkcs12", "-export", "-inkey", "srv.key", "-in", "srv.pem", "-certfile", "ca.pem", "-out", "srv.p12", "-passout", "pass:pw")
	return filepath.Join(dir, "ca.pem"), filepath.Join(dir, "srv.p12")
}

// fqlTlsWait waits until an https server on port answers a TLS handshake
// verified against caPEM.
func fqlTlsWait(t *testing.T, port int, caPEM string, log func() string) {
	t.Helper()
	pem, err := os.ReadFile(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: pool, ServerName: "localhost"})
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no TLS server on %d: %v\n%s", port, err, log())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// fqlTlsStartRust runs facetql with its TLS identity and returns its port.
func fqlTlsStartRust(t *testing.T, p12, caPEM string) int {
	t.Helper()
	bin := fqlLiveServerBin(t)
	port := fqFreePort(t)
	var log strings.Builder
	cmd := exec.Command(bin, "start")
	cmd.Env = append(os.Environ(),
		"FACETQL_DATA_DIR="+t.TempDir(),
		fmt.Sprintf("FACETQL_PORT=%d", port),
		"FACETQL_TOKENS=fabtok:fabric:admin",
		"FACETQL_MASTER_KEY="+facetqlCheckKey,
		"FACETQL_TLS_IDENTITY="+p12,
		"FACETQL_TLS_IDENTITY_PASSWORD=pw",
		"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off",
	)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	fqlTlsWait(t, port, caPEM, log.String)
	return port
}

// fqlTlsStartFct runs Team 4's fqserver.fct (`facet exec`) with the same
// identity, served by listenTls, and returns its port.
func fqlTlsStartFct(t *testing.T, p12, caPEM string) int {
	t.Helper()
	facet := fqServerFacet(t)
	server, err := filepath.Abs("fqserver.fct")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw, err := os.ReadFile(p12)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "srv.p12"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	port := fqFreePort(t)
	var log strings.Builder
	cmd := exec.Command(facet, "exec", server)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"FACET_DATA_DIR="+dir, "FACETQL_DATA_DIR="+dir,
		fmt.Sprintf("FACETQL_PORT=%d", port),
		"FACETQL_TOKENS=fabtok:fabric:admin",
		"FACETQL_MASTER_KEY="+facetqlCheckKey,
		"FACETQL_TLS_IDENTITY=srv.p12",
		"FACETQL_TLS_IDENTITY_PASSWORD=pw",
		"FACETQL_RATE_READ=off", "FACETQL_RATE_WRITE=off", "FACETQL_RATE_BULK=off", "FACETQL_RATE_ADMIN=off",
	)
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	fqlTlsWait(t, port, caPEM, log.String)
	return port
}

func TestFabricFacetqlOverTLS(t *testing.T) {
	pki := t.TempDir()
	caPEM, p12 := fqlTlsPKI(t, pki)
	rustPort := fqlTlsStartRust(t, p12, caPEM)

	// The port's trust file lives in its data sandbox.
	data := t.TempDir()
	t.Setenv("FACET_DATA_DIR", data)
	raw, err := os.ReadFile(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "ca.pem"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("https://localhost:%d", rustPort)

	t.Run("the_live_flows_over_https", func(t *testing.T) {
		ts := fqltApp(t, "fabric_facetql_placement.fct")
		flow := func(name string, shard, rows int) map[string]string {
			t.Helper()
			return fqlLiveFields(t, fqltCall(t, ts, "fqlLiveOut", "fqlLiveRun", name, base, "fabtok", shard, rows, "ca.pem"))
		}
		expect := func(m, want map[string]string) {
			t.Helper()
			for k, v := range want {
				if m[k] != v {
					t.Errorf("%s = %q, want %q (all: %v)", k, m[k], v, m)
				}
			}
		}
		expect(flow("stats", 0, 0), map[string]string{"first": "ok", "second": "ok", "hasVersion": "true"})
		expect(flow("writeRead", 9101, 0), map[string]string{"create": "ok", "get": "ok found=true", "registry": "ok located=true dbms=db-a", "secondCreate": "Conflict", "cleanup": "ok"})
		expect(flow("stale", 9102, 0), map[string]string{"firstUpdate": "ok v2", "staleUpdate": "PreconditionFailed", "staleRemove": "PreconditionFailed", "remove": "ok", "gone": "ok found=false"})
		expect(flow("claim", 9103, 0), map[string]string{"first": "ok true", "second": "ok false", "missing": "NotFound"})
		expect(flow("surface", 0, 0), map[string]string{"count": "ok 2", "applied": `ok b={"n":6}`, "cleared": "ok count=0"})
		expect(flow("events", 0, 0), map[string]string{"opened": "true 200 text/event-stream", "seen": "true true"})
		expect(flow("cursor", 9200, 600), map[string]string{"createFailures": "0", "ours": "600", "distinct": "600", "cleanupFailures": "0"})

		poller := fqltApp(t, "fabric_facetql_poller.fct")
		m := fqlLiveFields(t, fqltCall(t, poller, "pollerOut", "pollerRunLive", "run", base, "fabtok", "ca.pem"))
		expect(m, map[string]string{"outcome": "Sampled", "health": "healthy"})
	})

	t.Run("the_mover_between_two_https_instances", func(t *testing.T) {
		dst := fmt.Sprintf("https://localhost:%d", fqlTlsStartRust(t, p12, caPEM))
		src := fmt.Sprintf("https://localhost:%d", fqlTlsStartRust(t, p12, caPEM))
		ts := fqltApp(t, "fabric_facetql_mover.fct")
		m := fqlLiveFields(t, fqltCall(t, ts, "moverOut", "moverRunLive", "copy", src, dst, "fabtok", "ca.pem"))
		for k, v := range map[string]string{"subscribe": "true", "rowsCopied": "30", "verified": "verified 30", "report": "30 true 3 3"} {
			if m[k] != v {
				t.Errorf("%s = %q, want %q", k, m[k], v)
			}
		}
	})

	t.Run("against_the_fct_facetql_servers_listenTls", func(t *testing.T) {
		fctBase := fmt.Sprintf("https://localhost:%d", fqlTlsStartFct(t, p12, caPEM))
		ts := fqltApp(t, "fabric_facetql_placement.fct")
		m := fqlLiveFields(t, fqltCall(t, ts, "fqlLiveOut", "fqlLiveRun", "writeRead", fctBase, "fabtok", 9301, 0, "ca.pem"))
		for k, v := range map[string]string{"create": "ok", "get": "ok found=true", "secondCreate": "Conflict", "cleanup": "ok"} {
			if m[k] != v {
				t.Errorf("%s = %q, want %q (all: %v)", k, m[k], v, m)
			}
		}
	})

	t.Run("tls_failures_classify_as_the_rust_clients_do", func(t *testing.T) {
		bin := laCheck(t)
		plainPort := fqlLiveStart(t)
		plain := strings.TrimPrefix(plainPort, "http://")
		ts := fqltApp(t, "fabric_facetql_client.fct")
		cases := []struct {
			name, base string
			trusted    bool
		}{
			{"trusted", base, true},
			{"untrusted chain", base, false},
			{"wrong name", fmt.Sprintf("https://127.0.0.1:%d", rustPort), true},
			{"plain server behind an https url", "https://" + plain, true},
			{"http url to a tls server", fmt.Sprintf("http://localhost:%d", rustPort), true},
			{"nothing listening", fmt.Sprintf("https://localhost:%d", fqFreePort(t)), true},
		}
		for _, c := range cases {
			args := []string{"fql-tls", c.base, "fabtok"}
			trust := ""
			if c.trusted {
				args = append(args, caPEM)
				trust = "ca.pem"
			}
			out, err := exec.Command(bin, args...).Output()
			if err != nil {
				t.Fatalf("%s: fabric_check fql-tls: %v", c.name, err)
			}
			rust := strings.TrimSuffix(string(out), "\n")
			port := fqltCall(t, ts, "fqlClientOut", "fqlStatsProbe", c.base, "fabtok", trust)
			// Transport text is compared up to where each HTTP stack's own
			// words begin; everything before it — variant, health, Display
			// prefix and context — must agree.
			cut := func(s string) string {
				if i := strings.Index(s, "GET /stats on "+c.base+": "); i >= 0 {
					return s[:i+len("GET /stats on "+c.base+": ")]
				}
				return s
			}
			t.Logf("%s\n port: %s\n rust: %s", c.name, port, rust)
			if cut(port) != cut(rust) {
				t.Errorf("%s:\n port: %s\n rust: %s", c.name, port, rust)
			}
			if c.name == "trusted" && !strings.HasPrefix(port, "OK ") {
				t.Errorf("trusted: %s", port)
			}
			if c.name != "trusted" && !strings.HasPrefix(port, "ERR Transport true facetql unreachable: GET /stats on ") {
				t.Errorf("%s: %s", c.name, port)
			}
		}
	})
}
