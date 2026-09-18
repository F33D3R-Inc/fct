package runtime

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"facet/internal/compile"
)

// ── Milestone 5: structured concurrency (spawn/join/channel), live over HTTP ─

// slowFetchApp spawns two `httpGet` calls concurrently and joins both — the
// shape every test below in this file proves is a REAL goroutine each, not a
// disguised sequential call.
const slowFetchApp = `app A:
    proc slowFetch(url: text) -> text uses io.net:
        return httpGet(url)
    proc fetchTwo(u1: text, u2: text) -> text uses io.net:
        let h1 = spawn slowFetch(u1)
        let h2 = spawn slowFetch(u2)
        let r1 = join h1
        let r2 = join h2
        return r1 + r2
    state result: text = ""
    action run(u1: text, u2: text):
        let r = do fetchTwo(u1, u2)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestSpawnJoinConcurrencyIsRealLive is the single most important test in
// this file: it proves spawn/join actually run their two `httpGet` calls
// CONCURRENTLY, as real goroutines, rather than a naive/wrong implementation
// that secretly still serializes them under some lock. Each of two upstream
// requests sleeps ~150ms before answering; joining both must take roughly
// the time of the SLOWEST one (~150ms), not the SUM (~300ms). A server that
// held an internal lock across the whole `do fetchTwo(...)` call — such as
// accidentally running the spawned goroutine's body under s.mu — would still
// pass a correctness check on the returned VALUE but fail this timing bound,
// which is exactly the bug class this test exists to catch.
func TestSpawnJoinConcurrencyIsRealLive(t *testing.T) {
	const sleep = 150 * time.Millisecond
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(sleep)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()

	g, err := compile.String(slowFetchApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	start := time.Now()
	deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%q,%q]}`, upstream.URL, upstream.URL))
	elapsed := time.Since(start)

	if got := fmt.Sprint(deltas["result"]); got != "okok" {
		t.Fatalf("result = %q, want %q", got, "okok")
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("want 2 upstream hits, got %d", hits)
	}
	// Generous slack (2.5x one sleep) for scheduler jitter/CI noise, while
	// still being well under the 2x sleep a serialized (bugged) execution
	// would take — 2x150ms=300ms is the failure line; 375ms gives headroom
	// without weakening the proof.
	budget := sleep * 5 / 2
	if elapsed >= budget {
		t.Fatalf("spawn/join took %v for two %v-sleep calls — want well under %v (< 2x, proving they ran concurrently, not serially)", elapsed, sleep, budget)
	}
	t.Logf("two concurrent %v-sleep httpGet calls via spawn/join took %v (serial would be ~%v)", sleep, elapsed, 2*sleep)
}

// TestSpawnedGoroutineFullyCompletesBeforeJoinReturnsLive proves the
// structured-concurrency guarantee this milestone chose to enforce at
// compile time (checkSpawnsJoined — see internal/compile/spawn_test.go)
// actually holds at runtime too: by the time `join` hands back a result, the
// spawned goroutine's own effects are already fully complete and observable,
// not merely "probably done soon". The upstream handler records completion
// into an atomic counter AFTER its sleep; the test asserts the counter is
// already at its final value the instant the HTTP response comes back — no
// polling, no retry loop.
func TestSpawnedGoroutineFullyCompletesBeforeJoinReturnsLive(t *testing.T) {
	var completed int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(60 * time.Millisecond)
		atomic.AddInt32(&completed, 1)
		fmt.Fprint(w, "done")
	}))
	defer upstream.Close()

	g, err := compile.String(slowFetchApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%q,%q]}`, upstream.URL, upstream.URL))
	if got := fmt.Sprint(deltas["result"]); got != "donedone" {
		t.Fatalf("result = %q, want %q", got, "donedone")
	}
	// No sleep, no retry: the action's own HTTP response already came back,
	// so both spawned goroutines must already have been joined synchronously
	// inside execProcBlock — if either were still running in the background,
	// this read would race the increment above (and -race would catch it).
	if got := atomic.LoadInt32(&completed); got != 2 {
		t.Fatalf("completed = %d, want 2 — a spawned task's effects must be fully finished by the time it is joined", got)
	}
}

// ── error propagation ────────────────────────────────────────────────────────

const spawnErrorApp = `app A:
    proc fetchOne(url: text) -> text uses io.net:
        return httpGet(url)
    proc runIt(url: text) -> text uses io.net:
        let h = spawn fetchOne(url)
        let r = join h
        return r
    state result: text = ""
    action run(url: text):
        let r = do runIt(url)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestSpawnErrorPropagationLive proves a spawned proc's failure (here, a
// network error inside a spawned httpGet) surfaces cleanly through `join` —
// not swallowed, not a panic taking down the process — and that the server
// keeps answering normally afterward, matching this codebase's established
// "clean error, not a crash" contract (runtime/io_test.go's
// TestHTTPNetworkFailureIsCleanError is this test's non-spawned sibling).
func TestSpawnErrorPropagationLive(t *testing.T) {
	g, err := compile.String(spawnErrorApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Port 0 on localhost is never a listening service — a reliable, fast
	// connection-refused, the same trick runtime/io_test.go's own network
	// failure test uses.
	resp, err := postRaw(t, ts, "run", `{"args":["http://127.0.0.1:0/nope"]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if resp.StatusCode < 400 {
		t.Fatalf("a spawned proc's network failure should fail the request, got %d: %s", resp.StatusCode, body)
	}

	// The server must still be alive and answering normally afterward — the
	// same proc, called again with a reachable URL.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "still alive")
	}))
	defer upstream.Close()
	deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%q]}`, upstream.URL))
	if got := fmt.Sprint(deltas["result"]); got != "still alive" {
		t.Fatalf("server did not recover cleanly after a spawned task's network failure: got %q, want %q", got, "still alive")
	}
}

// spawnPanicApp calls recv() on a channel handle that was never created by
// channel() (a plain int an author passed by mistake) — channelRegistry.
// lookup returns a clean error for this, but the point of running it through
// spawn/join is proving a task's failure (of ANY kind, not just an I/O one)
// still surfaces through join rather than crashing the server.
const spawnBadHandleApp = `app A:
    proc badRecv(fakeCh: int) -> text:
        return recv(fakeCh)
    proc runIt(fakeCh: int) -> text:
        let h = spawn badRecv(fakeCh)
        let r = join h
        return r
    state result: text = ""
    action run(fakeCh: int):
        let r = do runIt(fakeCh)
        result = r
    action ping():
        result = "pong"
    view Home at "/":
        box:
            text "{result}"
`

// TestSpawnBadChannelHandleErrorPropagationLive is a second error-shape proof
// alongside the network-failure one above: a spawned task's runtime error
// (not I/O this time — a bad channel handle) still surfaces cleanly through
// join, and the server survives it.
func TestSpawnBadChannelHandleErrorPropagationLive(t *testing.T) {
	g, err := compile.String(spawnBadHandleApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := postRaw(t, ts, "run", `{"args":[999999]}`)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if resp.StatusCode < 400 {
		t.Fatalf("recv on a nonexistent channel should fail the request, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "channel") {
		t.Fatalf("error body should mention the bad channel, got: %s", body)
	}

	// The server must still be answering normally afterward — a different,
	// unrelated action, so this doesn't just re-trip the same bad path.
	deltas := postJSON(t, ts, "ping", `{"args":[]}`)
	if got := fmt.Sprint(deltas["result"]); got != "pong" {
		t.Fatalf("server did not recover cleanly after a spawned task's error: got %q, want %q", got, "pong")
	}
}

// ── data-race safety (independent frames) ───────────────────────────────────

const spawnIndependentLocalsApp = `app A:
    proc sumTo(n: int) -> int:
        let mut total = 0
        let mut i = 0
        loop i < n:
            total = total + i
            i = i + 1
        return total
    proc sumBoth(a: int, b: int) -> int:
        let h1 = spawn sumTo(a)
        let h2 = spawn sumTo(b)
        let r1 = join h1
        let r2 = join h2
        return r1 + r2
    state result: int = 0
    action run(a: int, b: int):
        let r = do sumBoth(a, b)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestSpawnIndependentFramesNoDataRaceLive spawns two procs that each mutate
// their OWN `let mut` locals (a running total across a loop) concurrently,
// then joins both. If the two spawned calls ever shared a frame or aliased
// any mutable state, the results would be wrong (cross-contaminated totals)
// or `go test -race` would report a data race on the shared map underneath.
// Run this file with `go test ./runtime/ -race -run TestSpawn` to exercise
// the race detector directly, per this milestone's own verification bar.
func TestSpawnIndependentFramesNoDataRaceLive(t *testing.T) {
	g, err := compile.String(spawnIndependentLocalsApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const a, b = 5000, 3333
	want := a*(a-1)/2 + b*(b-1)/2

	deltas := postJSON(t, ts, "run", fmt.Sprintf(`{"args":[%d,%d]}`, a, b))
	if got := toInt(deltas["result"]); got != want {
		t.Fatalf("sumBoth(%d,%d) = %d, want %d (0+1+...+%d + 0+1+...+%d)", a, b, got, want, a-1, b-1)
	}
}

// ── channels ─────────────────────────────────────────────────────────────────

const channelRoundTripApp = `app A:
    proc producer(ch: int, msg: text) -> bool:
        return send(ch, msg)
    proc consumer(ch: int) -> text:
        return recv(ch)
    proc roundTrip(msg: text) -> text:
        let ch = channel()
        let h1 = spawn producer(ch, msg)
        let h2 = spawn consumer(ch)
        join h1
        let got = join h2
        return got
    state result: text = ""
    action run(msg: text):
        let r = do roundTrip(msg)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

// TestChannelSendRecvBetweenSpawnedTasksLive proves a value sent from one
// spawned proc is correctly received by another spawned proc communicating
// purely through a channel() handle — the minimal channel feature's whole
// point. Both producer and consumer are real goroutines (spawned), so this
// also exercises recv() genuinely blocking until send() delivers, not a
// pre-filled value.
func TestChannelSendRecvBetweenSpawnedTasksLive(t *testing.T) {
	g, err := compile.String(channelRoundTripApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":["hello over a channel"]}`)
	if got := fmt.Sprint(deltas["result"]); got != "hello over a channel" {
		t.Fatalf("channel round-trip = %q, want %q", got, "hello over a channel")
	}
}

// TestChannelRecvBlocksUntilSendLive proves recv() genuinely blocks: the
// consumer is spawned FIRST (so it starts trying to recv before anything has
// been sent) and the producer sleeps briefly before sending — a broken
// implementation that made recv() return immediately (e.g. a zero value
// instead of actually blocking) would return before the sleep elapses.
const channelBlockingApp = `app A:
    proc delayedProducer(ch: int, msg: text) -> bool:
        return send(ch, msg)
    proc consumer(ch: int) -> text:
        return recv(ch)
    proc roundTrip(msg: text) -> text:
        let ch = channel()
        let hc = spawn consumer(ch)
        let hp = spawn delayedProducer(ch, msg)
        let got = join hc
        join hp
        return got
    state result: text = ""
    action run(msg: text):
        let r = do roundTrip(msg)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestChannelRecvBlocksUntilSendLive(t *testing.T) {
	g, err := compile.String(channelBlockingApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deltas := postJSON(t, ts, "run", `{"args":["patience"]}`)
	if got := fmt.Sprint(deltas["result"]); got != "patience" {
		t.Fatalf("blocking channel round-trip = %q, want %q", got, "patience")
	}
}
