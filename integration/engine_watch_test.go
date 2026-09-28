package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// engineWatchEnv turns on engineWatch for every engine a stack test starts:
// FACETQL_ENGINE_WATCH=1. It is off by default — a watched engine runs with
// proc labels on, which costs a little per call — and is how an engine that
// stops answering in the middle of a scenario is caught doing it.
const engineWatchEnv = "FACETQL_ENGINE_WATCH"

// engineWatchedEnv is what a watched engine is started with: proc labels, so
// its goroutine dumps name the fct task each goroutine is running.
func engineWatchedEnv() []string {
	if os.Getenv(engineWatchEnv) != "1" {
		return nil
	}
	return []string{"FACET_PROC_LABELS=1"}
}

// engineWatch probes an engine's GET /stats every 100 ms, with its own
// client, for as long as the test runs — the daemon's telemetry poll made
// from outside the daemon. A probe slower than 1 s (or one that fails) is
// written into the engine's log with its time and latency, and the engine
// is sent SIGUSR1 (`facet exec`'s goroutine dump, at most once a second),
// so what the engine was doing while it did not answer lands in the same
// log, beside its own output. Rust engines are probed and logged, not
// signalled.
func engineWatch(t *testing.T, cmd *exec.Cmd, base string, log io.Writer, fct bool) {
	if os.Getenv(engineWatchEnv) != "1" {
		return
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		client := &http.Client{Timeout: 5 * time.Second}
		var lastDump time.Time
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			t0 := time.Now()
			slowSignalled := false
			done := make(chan string, 1)
			go func() {
				req, _ := http.NewRequest("GET", base+"/stats", nil)
				req.Header.Set("x-api-key", afEngineToken)
				resp, err := client.Do(req)
				if err != nil {
					done <- "error: " + err.Error()
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				done <- fmt.Sprintf("status %d", resp.StatusCode)
			}()
			var outcome string
			for outcome == "" {
				select {
				case outcome = <-done:
				case <-time.After(time.Second):
					// still waiting a second in: dump while it is stuck
					if fct && !slowSignalled && time.Since(lastDump) > time.Second {
						slowSignalled = true
						lastDump = time.Now()
						fmt.Fprintf(log, "=== engine watch %s: GET /stats unanswered after 1s; SIGUSR1 ===\n", t0.Format("15:04:05.000"))
						_ = cmd.Process.Signal(syscall.SIGUSR1)
					}
				case <-stop:
					return
				}
			}
			if d := time.Since(t0); d > time.Second || outcome[:6] != "status" {
				fmt.Fprintf(log, "=== engine watch %s: GET /stats took %v: %s ===\n", t0.Format("15:04:05.000"), d.Round(time.Millisecond), outcome)
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
}
