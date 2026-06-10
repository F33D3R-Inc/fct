package fa

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis is a tiny in-process Redis pub/sub server speaking just enough RESP
// for the broker: AUTH, SUBSCRIBE, PUBLISH. It lets us prove the wire protocol
// and cross-instance delivery without a real Redis.
type fakeRedis struct {
	ln   net.Listener
	mu   sync.Mutex
	subs map[string][]net.Conn // channel → subscriber conns
}

func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln, subs: map[string][]net.Conn{}}
	go f.accept()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) accept() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(conn)
	}
}

func (f *fakeRedis) serve(conn net.Conn) {
	br := bufio.NewReader(conn)
	for {
		reply, err := readReply(br)
		if err != nil {
			return
		}
		arr, ok := reply.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		cmd := strings.ToUpper(string(arr[0].([]byte)))
		switch cmd {
		case "AUTH":
			conn.Write([]byte("+OK\r\n"))
		case "SUBSCRIBE":
			ch := string(arr[1].([]byte))
			f.mu.Lock()
			f.subs[ch] = append(f.subs[ch], conn)
			f.mu.Unlock()
			// subscribe confirmation: *3 $9 subscribe $len ch :1
			conn.Write([]byte("*3\r\n$9\r\nsubscribe\r\n$" + itoa(len(ch)) + "\r\n" + ch + "\r\n:1\r\n"))
		case "PUBLISH":
			ch := string(arr[1].([]byte))
			payload := arr[2].([]byte)
			f.mu.Lock()
			targets := append([]net.Conn(nil), f.subs[ch]...)
			f.mu.Unlock()
			frame := []byte("*3\r\n$7\r\nmessage\r\n$" + itoa(len(ch)) + "\r\n" + ch + "\r\n$" + itoa(len(payload)) + "\r\n")
			frame = append(frame, payload...)
			frame = append(frame, '\r', '\n')
			f.mu.Lock()
			for _, c := range targets {
				c.Write(frame)
			}
			f.mu.Unlock()
			conn.Write([]byte(":" + itoa(len(targets)) + "\r\n"))
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Two RedisBrokers on one fake Redis model two app instances: a message published
// by instance A is delivered to instance B (and back to A) — cross-instance
// fan-out over the real RESP wire path.
func TestRedisBrokerCrossInstance(t *testing.T) {
	srv := startFakeRedis(t)

	a, err := NewRedisBroker(srv.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewRedisBroker(srv.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	gotB := make(chan []byte, 1)
	b.Subscribe(func(msg []byte) { gotB <- msg })
	// give B's SUBSCRIBE time to register on the server
	time.Sleep(100 * time.Millisecond)

	want := []byte(`{"op":"replace","fragment":"<b>hi\r\nthere</b>"}`) // payload with CRLF
	if err := a.Publish(want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-gotB:
		if string(got) != string(want) {
			t.Errorf("instance B got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("instance B never received the cross-instance message")
	}
}

// Concurrent publishes from many goroutines must not race or drop (run with -race).
func TestRedisBrokerConcurrent(t *testing.T) {
	srv := startFakeRedis(t)
	pub, err := NewRedisBroker(srv.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	sub, err := NewRedisBroker(srv.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	var count int64
	var mu sync.Mutex
	received := map[string]bool{}
	sub.Subscribe(func(msg []byte) {
		mu.Lock()
		received[string(msg)] = true
		count++
		mu.Unlock()
	})
	time.Sleep(100 * time.Millisecond)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = pub.Publish([]byte("msg-" + itoa(i)))
		}(i)
	}
	wg.Wait()

	// allow delivery to drain
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := len(received)
		mu.Unlock()
		if c == n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != n {
		t.Errorf("received %d distinct messages, want %d", len(received), n)
	}
}
