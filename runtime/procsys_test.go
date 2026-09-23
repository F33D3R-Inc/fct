package runtime

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// procSysRun runs testdata/procsys.fct's main with args, answering what it
// wrote and the status exitProcess asked for (-1 when it did not).
func procSysRun(t *testing.T, dataDir string, args ...string) (string, int) {
	t.Helper()
	g, err := compile.File("testdata/procsys.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	if dataDir != "" {
		srv.SetDataDir(dataDir)
	}
	var out stdioSyncBuffer
	srv.SetStdio(strings.NewReader(""), &out, io.Discard)
	status := -1
	srv.SetExit(func(code int) { status = code })
	if _, err := srv.RunMain(args); err != nil {
		t.Fatalf("main %v: %v\n%s", args, err, out.String())
	}
	return out.String(), status
}

// TestChannelCloseAndAwaitAny: awaitAny reports which channel has a value
// (without taking it), times out with -1, waits indefinitely on a negative
// wait; closeChannel releases a blocked sender (its send answers false),
// makes later sends false and reads as ready to awaitAny, and answers
// false the second time.
func TestChannelCloseAndAwaitAny(t *testing.T) {
	out, _ := procSysRun(t, "", "channels")
	want := "-1 1 x -1:true 0 late:true true false false 1 true,false\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

// TestExitProcessStatus: exitProcess hands its status to the process's exit.
func TestExitProcessStatus(t *testing.T) {
	out, status := procSysRun(t, "", "exit")
	if status != 7 || !strings.HasPrefix(out, "bye\n") {
		t.Fatalf("status %d, stdout %q", status, out)
	}
}

// TestProcessStats: the process section, every field facetql reports, with
// the values this process really has.
func TestProcessStats(t *testing.T) {
	out, _ := procSysRun(t, "", "stats")
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	keys := []string{"cpu_seconds_total", "cpu_cores", "resident_bytes", "memory_limit_bytes", "memory_limit_source", "memory_utilization"}
	for _, k := range keys {
		if _, ok := st[k]; !ok {
			t.Errorf("no %s in %s", k, out)
		}
	}
	if len(st) != len(keys) {
		t.Errorf("unexpected fields in %s", out)
	}
	if cores, _ := st["cpu_cores"].(float64); cores < 1 {
		t.Errorf("cpu_cores %v", st["cpu_cores"])
	}
	if rss, _ := st["resident_bytes"].(float64); rss < 1<<20 {
		t.Errorf("resident_bytes %v", st["resident_bytes"])
	}
	if cpu, _ := st["cpu_seconds_total"].(float64); cpu <= 0 {
		t.Errorf("cpu_seconds_total %v", st["cpu_seconds_total"])
	}
}

// procSysIdentity writes a self-signed certificate and key as a PKCS#12
// identity with openssl, as an operator makes one; legacy selects the
// pre-OpenSSL-3 encryption (RC2/3DES).
func procSysIdentity(t *testing.T, dir, name, password string, legacy bool) string {
	t.Helper()
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl is not installed")
	}
	key, cert, p12 := filepath.Join(dir, name+".key"), filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".p12")
	run := func(args ...string) error {
		out, err := exec.Command(openssl, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("openssl %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := run("req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", key, "-out", cert, "-days", "2", "-subj", "/CN=localhost"); err != nil {
		t.Fatal(err)
	}
	args := []string{"pkcs12", "-export", "-inkey", key, "-in", cert, "-out", p12, "-passout", "pass:" + password}
	if legacy {
		args = append(args, "-legacy")
	}
	if err := run(args...); err != nil {
		if legacy {
			t.Skipf("this openssl cannot write a legacy PKCS#12: %v", err)
		}
		t.Fatal(err)
	}
	return p12
}

// TestListenTls: a PKCS#12 identity — OpenSSL 3's default PBES2/AES form
// and the legacy RC2/3DES one — serves TLS; a client's bytes arrive as
// plaintext and the answer goes back encrypted, under the identity's
// certificate. A wrong password is refused when the listener is made.
func TestListenTls(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			dir := t.TempDir()
			p12 := procSysIdentity(t, dir, "id", "s3cret", legacy)
			port := detachSharedFreePort(t)
			done := make(chan string, 1)
			go func() {
				out, _ := procSysRun(t, dir, "tls", fmt.Sprint(port), p12, "s3cret")
				done <- out
			}()
			var c *tls.Conn
			deadline := time.Now().Add(5 * time.Second)
			for {
				var err error
				c, err = tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{InsecureSkipVerify: true})
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no TLS listener: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			defer c.Close()
			if cn := c.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "localhost" {
				t.Fatalf("certificate %q", cn)
			}
			if _, err := c.Write([]byte("hello")); err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(c)
			if string(got) != "echo:hello" {
				t.Fatalf("answer %q", got)
			}
			if out := <-done; !strings.Contains(out, "peer 127.0.0.1:") {
				t.Fatalf("connPeer: %q", out)
			}
		})
	}
	dir := t.TempDir()
	p12 := procSysIdentity(t, dir, "id", "right", false)
	raw, err := os.ReadFile(p12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePKCS12(raw, "wrong"); err == nil || !strings.Contains(err.Error(), "MAC verification failed") {
		t.Fatalf("wrong password: %v", err)
	}
}

// TestPositionalFileIOIsAtomic: a readFileAt racing writeFileAt over the
// same 64 KiB block sees one write or the other, never a mix — what lets a
// paged file be read by snapshot readers while its one writer rewrites it.
func TestPositionalFileIOIsAtomic(t *testing.T) {
	g, err := compile.File("testdata/procsys.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(t.TempDir())
	block := func(b int) []any {
		out := make([]any, 65536)
		for i := range out {
			out[i] = b
		}
		return out
	}
	if _, err := srv.ioWriteFileAt("paged", 0, block(1)); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := srv.ioWriteFileAt("paged", 0, block(1+i%2)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for n := 0; n < 300; n++ {
		v, err := srv.ioReadFileAt("paged", 0, 65536)
		if err != nil {
			t.Fatal(err)
		}
		arr := v.([]any)
		first := toInt(arr[0])
		for i, x := range arr {
			if toInt(x) != first {
				close(stop)
				<-done
				t.Fatalf("read %d saw a torn block: byte 0 is %d, byte %d is %d", n, first, i, toInt(x))
			}
		}
	}
	close(stop)
	<-done
}

// TestRunDaemonsReportsAFailedDaemon: a service (`facet exec` of a program
// with daemons) whose daemon ends in an error ends in failure itself, so
// its exit status says so.
func TestRunDaemonsReportsAFailedDaemon(t *testing.T) {
	g, err := compile.String(`app FailingService:
    daemon Boom uses io.file:
        let s = readFile("no-such-file")
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetDataDir(t.TempDir())
	if err := srv.RunDaemons(); err == nil || !strings.Contains(err.Error(), "daemon Boom") {
		t.Fatalf("RunDaemons = %v, want the daemon's failure", err)
	}
}
