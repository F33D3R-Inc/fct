package runtime

import (
	"bufio"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"
)

// Shutdown ends the program a server hosts: the daemon's accept loop, the
// connection it detached, and a sleeping ticker all return, and the port
// is released — nothing keeps serving after its host shut it down.
func TestShutdownHaltsTheProgram(t *testing.T) {
	srv, ts := detachSharedServer(t)
	port := detachSharedFreePort(t)
	postJSON(t, ts, "setup", fmt.Sprintf(`{"args":[%d,"hello"]}`, port))
	before := runtime.NumGoroutine()
	srv.StartJobs()
	c := detachSharedDial(t, port)
	if got := detachSharedAsk(t, c, bufio.NewReader(c), "one"); got != "hello one" {
		t.Fatalf("the daemon answered %q", got)
	}
	srv.Shutdown()
	// The open connection is closed from the server's side.
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
		t.Fatal("the connection stayed open after Shutdown")
	}
	// Nothing listens any more.
	deadline := time.Now().Add(3 * time.Second)
	for {
		d, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			break
		}
		d.Close()
		if time.Now().After(deadline) {
			t.Fatal("the daemon still accepts after Shutdown")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The daemon's and the handler's goroutines are gone.
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+2 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Fatalf("%d goroutines still running after Shutdown (%d before the program started)", n, before)
	}
	// A sleep in a halted program ends at once.
	t0 := time.Now()
	if _, err := srv.ioSleepMs(5000); err == nil || time.Since(t0) > time.Second {
		t.Fatalf("sleepMs after Shutdown = %v after %v", err, time.Since(t0))
	}
}
