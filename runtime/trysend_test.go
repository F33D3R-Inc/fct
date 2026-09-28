package runtime

import (
	"testing"

	"facet/internal/compile"
)

// trySend never waits: it queues when there is room, and answers false when
// the buffer is full or the channel is closed.
func TestTrySendNeverWaits(t *testing.T) {
	r := newChannelRegistry()
	ch := r.create()
	if ok, err := r.trySend(ch, "a"); err != nil || ok != true {
		t.Fatalf("trySend into an empty channel = %v, %v", ok, err)
	}
	if ok, err := r.trySend(ch, "b"); err != nil || ok != false {
		t.Fatalf("trySend into a full channel = %v, %v; want false", ok, err)
	}
	if v, err := r.recv(ch); err != nil || v != "a" {
		t.Fatalf("recv = %v, %v", v, err)
	}
	if ok, _ := r.trySend(ch, "c"); ok != true {
		t.Fatal("trySend after the reader drained the buffer = false")
	}
	r.close(ch)
	if ok, err := r.trySend(ch, "d"); err != nil || ok != false {
		t.Fatalf("trySend into a closed channel = %v, %v; want false", ok, err)
	}
	if _, err := r.trySend(9999, "x"); err == nil {
		t.Fatal("trySend to a handle that never existed did not fail")
	}
}

// In a program: a proc with a full channel moves on.
func TestTrySendInAProc(t *testing.T) {
	g, err := compile.String(`app T:
    proc run() -> text:
        let ch = channel()
        let a = trySend(ch, "one")
        let b = trySend(ch, "two")
        let got = recv(ch)
        return "" + a + " " + b + " " + got
    state out: text = ""
    action go():
        let r = do run()
        out = r
    view V at "/":
        text "{out}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	d, err := srv.Run("ada", "member", true, "go", nil)
	if err != nil || d["out"] != "true false one" {
		t.Fatalf("run = %v, %v", d, err)
	}
	if _, err := compile.String("app U:\n    state n: int = 0\n    action a():\n        n = 1\n    view V at \"/\":\n        text \"{trySend(1, \\\"x\\\")}\"\n"); err == nil {
		t.Fatal("trySend outside a proc compiled")
	}
}

// channel(n) buffers n values; trySend fills it and then answers false.
func TestSizedChannel(t *testing.T) {
	r := newChannelRegistry()
	ch, err := r.createSized(3)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, true, false} {
		if ok, _ := r.trySend(ch, "v"); ok != want {
			t.Fatalf("trySend %d = %v, want %v", i, ok, want)
		}
	}
	for _, bad := range []int{0, -1, maxChannelBuffer + 1} {
		if _, err := r.createSized(bad); err == nil {
			t.Fatalf("channel(%d) was accepted", bad)
		}
	}
	g, err := compile.String(`app T:
    proc run() -> text:
        let ch = channel(2)
        let a = trySend(ch, "one")
        let b = trySend(ch, "two")
        let c = trySend(ch, "three")
        return "" + a + b + c + recv(ch) + recv(ch)
    state out: text = ""
    action go():
        let r = do run()
        out = r
    view V at "/":
        text "{out}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	d, err := srv.Run("ada", "member", true, "go", nil)
	if err != nil || d["out"] != "truetruefalseonetwo" {
		t.Fatalf("run = %v, %v", d, err)
	}
}
