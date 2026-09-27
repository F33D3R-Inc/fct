package selfhost

// The engines' cost, side by side: `go test ./selfhost -run xxx -bench
// FqServer -benchtime 200x` times one HTTP round trip of each kind against
// `facetql start` and against `facet exec fqserver.fct`, on keep-alive
// connections, so a regression in either engine — or in the proc
// evaluator everything the fct engine does runs on — shows as a ratio.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type fqBenchClient struct {
	b    *testing.B
	port int
	c    *http.Client
}

func (bc *fqBenchClient) do(method, path, body string) int {
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", bc.port, path), bytes.NewBufferString(body))
	req.Header.Set("x-api-key", "tok")
	req.Header.Set("content-type", "application/json")
	r, err := bc.c.Do(req)
	if err != nil {
		bc.b.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return r.StatusCode
}

func BenchmarkFqServer(b *testing.B) {
	for _, which := range []string{"rust", "fct"} {
		b.Run(which, func(b *testing.B) {
			dir := b.TempDir()
			env := append(append([]string{}, fqContractEnv...), "FACETQL_DATA_DIR="+dir, "FACETQL_PORT=0")
			p, port := fqStartProcB(b, which, dir, env...)
			defer func() { _ = p.cmd.Process.Kill(); <-p.done }()
			bc := &fqBenchClient{b: b, port: port, c: &http.Client{}}
			seed := 0
			b.Run("write", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					seed++
					if s := bc.do("POST", "/node", fmt.Sprintf(`{"address":"w:%d","kind":"W","x":0,"y":0,"z":0,"q":0,"data":"{\"i\":%d,\"t\":\"note %d\"}","public":true}`, seed, seed, seed)); s != 201 {
						b.Fatalf("write: %d", s)
					}
				}
			})
			b.Run("read", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					bc.do("GET", fmt.Sprintf("/node/w:%d", 1+i%seed), "")
				}
			})
			b.Run("query-ordered-page", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					bc.do("POST", "/nodes/query", `{"kind":"W","order":"i","desc":true,"limit":20}`)
				}
			})
			b.Run("transaction-of-20", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					var ops string
					for k := 0; k < 20; k++ {
						if k > 0 {
							ops += ","
						}
						ops += fmt.Sprintf(`{"type":"insert_node","address":"t:%d:%d","kind":"T","x":0,"y":0,"z":0,"q":0,"data":"{}"}`, i, k)
					}
					bc.do("POST", "/transaction", `{"operations":[`+ops+`]}`)
				}
			})
		})
	}
}

// fqStartProcB: fqStartProc and fqWaitListening for a benchmark — the
// server started on port 0, and the port its banner names.
func fqStartProcB(b *testing.B, which, dir string, env ...string) (*fqProc, int) {
	var cmd *exec.Cmd
	if which == "rust" {
		bin, err := filepath.Abs("../../facetql/target/release/facetql")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := os.Stat(bin); err != nil {
			b.Skip("no facetql release binary")
		}
		cmd = exec.Command(bin, "start")
	} else {
		server, _ := filepath.Abs("fqserver.fct")
		facet, err := filepath.Abs("../../" + os.Getenv("FCT_FACET_BIN"))
		if os.Getenv("FCT_FACET_BIN") == "" || err != nil {
			dirb, err := os.MkdirTemp("", "fqbench-facet-")
			if err != nil {
				b.Fatal(err)
			}
			facet = filepath.Join(dirb, "facet")
			build := exec.Command("go", "build", "-o", facet, "facet/cmd/facet")
			build.Dir = ".."
			if out, err := build.CombinedOutput(); err != nil {
				b.Fatalf("building facet: %v\n%s", err, out)
			}
		}
		cmd = exec.Command(facet, "exec", server)
		env = append(env, "FACET_DATA_DIR="+dir)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	p := &fqProc{cmd: cmd, stdout: &fqLockedBuf{}, stderr: &fqLockedBuf{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		b.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(p.done) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if port := fqBannerPort(p.stdout.String()); port != 0 {
			if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
				c.Close()
				return p, port
			}
		}
		select {
		case <-p.done:
			b.Fatalf("exited: %q %q", p.stdout.String(), p.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	b.Fatal("never listened")
	return nil, 0
}
