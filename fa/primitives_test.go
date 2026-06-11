package fa

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureBroker records published fanout messages without delivering them.
type captureBroker struct {
	mu   sync.Mutex
	msgs [][]byte
}

func (b *captureBroker) Publish(msg []byte) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, msg)
	b.mu.Unlock()
	return nil
}
func (b *captureBroker) Subscribe(fn func([]byte)) {}

func (b *captureBroker) events(t *testing.T) []fanoutMsg {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]fanoutMsg, 0, len(b.msgs))
	for _, m := range b.msgs {
		var fm fanoutMsg
		if err := json.Unmarshal(m, &fm); err != nil {
			t.Fatalf("bad fanout msg: %v", err)
		}
		out = append(out, fm)
	}
	return out
}

// ── feed: SortFeed ──────────────────────────────────────────────────────────

const feedSrc = `feed Timeline:
    what:
        items: PostList
    order: score
    looks:
        <ul>{items}</ul>
`

type feedPost struct {
	Score int
	Title string
}

func TestSortFeedDescendingByDefault(t *testing.T) {
	c, err := Compile(feedSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	posts := []feedPost{{Score: 1, Title: "low"}, {Score: 9, Title: "high"}, {Score: 5, Title: "mid"}}
	if err := c.SortFeed("Timeline", posts); err != nil {
		t.Fatalf("SortFeed: %v", err)
	}
	if posts[0].Title != "high" || posts[1].Title != "mid" || posts[2].Title != "low" {
		t.Errorf("feed not ranked descending: %+v", posts)
	}
}

func TestSortFeedAscending(t *testing.T) {
	src := strings.Replace(feedSrc, "order: score", "order: title asc", 1)
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	posts := []*feedPost{{Title: "b"}, {Title: "c"}, {Title: "a"}}
	if err := c.SortFeed("Timeline", posts); err != nil {
		t.Fatalf("SortFeed: %v", err)
	}
	if posts[0].Title != "a" || posts[2].Title != "c" {
		t.Errorf("feed not ascending: %v %v %v", posts[0].Title, posts[1].Title, posts[2].Title)
	}
}

func TestSortFeedMapsAndSnakeCase(t *testing.T) {
	src := strings.Replace(feedSrc, "order: score", "order: created_at", 1)
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	t0 := time.Now()
	items := []map[string]any{
		{"created_at": t0.Add(-time.Hour), "n": "old"},
		{"created_at": t0, "n": "new"},
	}
	if err := c.SortFeed("Timeline", items); err != nil {
		t.Fatalf("SortFeed: %v", err)
	}
	if items[0]["n"] != "new" {
		t.Errorf("newest-first expected, got %v first", items[0]["n"])
	}

	// The same snake_case field resolves Go-named struct fields (CreatedAt).
	type row struct{ CreatedAt time.Time }
	rows := []row{{t0.Add(-time.Hour)}, {t0}}
	if err := c.SortFeed("Timeline", rows); err != nil {
		t.Fatalf("SortFeed structs: %v", err)
	}
	if !rows[0].CreatedAt.Equal(t0) {
		t.Errorf("struct rows not newest-first")
	}
}

// Fail closed: a missing order field is an error and the slice is untouched.
func TestSortFeedUnknownFieldFailsClosed(t *testing.T) {
	c, err := Compile(feedSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	posts := []feedPost{{Title: "b"}, {Title: "a"}}
	type other struct{ X int }
	if err := c.SortFeed("Timeline", []other{{2}, {1}}); err == nil {
		t.Error("want error for missing order field")
	}
	_ = posts
}

func TestSortFeedRejectsNonFeed(t *testing.T) {
	c, err := Compile("facet Card:\n    what:\n        title: str\n    looks:\n        <div>{title}</div>\n")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := c.SortFeed("Card", []feedPost{}); err == nil {
		t.Error("want error: Card is not a feed")
	}
}

// ── lifecycle: state machine ────────────────────────────────────────────────

func TestLifecycleTransitions(t *testing.T) {
	c, err := Compile("lifecycle Order:\n    what:\n        id: int\n    states: placed, paid, shipped\n    looks:\n        <div>{id}</div>\n")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	lc, err := c.Lifecycle("Order")
	if err != nil {
		t.Fatalf("Lifecycle: %v", err)
	}
	if lc.Initial() != "placed" {
		t.Errorf("initial = %q", lc.Initial())
	}
	if next, err := lc.Next("placed"); err != nil || next != "paid" {
		t.Errorf("Next(placed) = %q, %v", next, err)
	}
	if _, err := lc.Next("shipped"); err == nil {
		t.Error("terminal state must not have a Next")
	}
	if _, err := lc.Next("refunded"); err == nil {
		t.Error("unknown state must error")
	}
	if !lc.CanTransition("paid", "shipped") {
		t.Error("paid → shipped should be legal")
	}
	for _, bad := range [][2]string{{"placed", "shipped"}, {"paid", "placed"}, {"placed", "placed"}} {
		if lc.CanTransition(bad[0], bad[1]) {
			t.Errorf("%s → %s should be rejected", bad[0], bad[1])
		}
	}
	if lc.Valid("nope") || !lc.Valid("paid") {
		t.Error("Valid membership wrong")
	}
}

func TestLifecycleRejectsOtherKinds(t *testing.T) {
	c, err := Compile(feedSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := c.Lifecycle("Timeline"); err == nil {
		t.Error("want error: Timeline is not a lifecycle")
	}
}

// ── signal: ephemeral relay ─────────────────────────────────────────────────

const signalSrc = `signal Typing:
    what:
        who: str
    ttl: 5s
`

func TestSignalRelaysToChannel(t *testing.T) {
	c, err := Compile(signalSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	broker := &captureBroker{}
	a := New(c.Manifest, WithBroker(broker), WithSigningKey([]byte("k-k-k-k-k-k-k-k-k-k-k-k-k-k-k-k!")))
	if err := a.Signal("room:1", "Typing", map[string]string{"who": "ada"}); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	evs := broker.events(t)
	if len(evs) != 1 {
		t.Fatalf("published %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Scope != "channel" || e.Target != "room:1" {
		t.Errorf("scope/target = %s/%s", e.Scope, e.Target)
	}
	if e.Event.Op != "signal" || e.Event.FacetID != "Typing" {
		t.Errorf("op/facet = %s/%s", e.Event.Op, e.Event.FacetID)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(e.Event.Fragment), &payload); err != nil || payload["who"] != "ada" {
		t.Errorf("payload = %q (%v)", e.Event.Fragment, err)
	}
	if e.Event.HMAC == "" {
		t.Error("signal event must be signed")
	}
}

// Fail closed: only a declared `signal` primitive may relay.
func TestSignalRejectsNonSignal(t *testing.T) {
	c, err := Compile(feedSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	broker := &captureBroker{}
	a := New(c.Manifest, WithBroker(broker))
	if err := a.Signal("room:1", "Timeline", nil); err == nil {
		t.Error("want error: Timeline is not a signal")
	}
	if err := a.Signal("room:1", "Ghost", nil); err == nil {
		t.Error("want error: Ghost is not in the manifest")
	}
	if len(broker.events(t)) != 0 {
		t.Error("nothing may be published on a rejected signal")
	}
}

// A signal's fragment is a JSON payload, not HTML — the native transform must
// pass it through untouched.
func TestNativeEventLeavesSignalPayload(t *testing.T) {
	h := newHub([]byte("k-k-k-k-k-k-k-k-k-k-k-k-k-k-k-k!"), nil, nil)
	in := Event{Op: "signal", FacetID: "Typing", Fragment: `{"who":"ada"}`}
	sign(h.key, &in)
	out := h.nativeEvent(in)
	if out.Fragment != in.Fragment || out.HMAC != in.HMAC {
		t.Errorf("signal event was transformed: %+v", out)
	}
}

// ── stream/pipe: throttle ───────────────────────────────────────────────────

const streamSrc = `stream LiveChat:
    what:
        msg: str
    throttle: 60ms
    window: 100
    looks:
        <div>{msg}</div>
`

func TestStreamThrottleCoalesces(t *testing.T) {
	c, err := Compile(streamSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	broker := &captureBroker{}
	a := New(c.Manifest, WithBroker(broker), WithSigningKey([]byte("k-k-k-k-k-k-k-k-k-k-k-k-k-k-k-k!")))

	// A burst of 3 inside one interval: the first goes out immediately, the
	// intermediate is dropped, the LATEST flushes when the interval elapses.
	for _, frag := range []string{"one", "two", "three"} {
		a.Hub().EmitChannel("room:1", Event{Op: "append", FacetID: "LiveChat", Fragment: frag})
	}
	if got := broker.events(t); len(got) != 1 || got[0].Event.Fragment != "one" {
		t.Fatalf("immediate emit wrong: %+v", got)
	}
	time.Sleep(120 * time.Millisecond)
	got := broker.events(t)
	if len(got) != 2 {
		t.Fatalf("after interval: %d events, want 2 (first + coalesced latest)", len(got))
	}
	if got[1].Event.Fragment != "three" {
		t.Errorf("coalesced flush must carry the LATEST frame, got %q", got[1].Event.Fragment)
	}
}

func TestUnthrottledFacetPassesThrough(t *testing.T) {
	c, err := Compile(streamSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	broker := &captureBroker{}
	a := New(c.Manifest, WithBroker(broker))
	for i := 0; i < 3; i++ {
		a.Hub().Broadcast(Event{Op: "replace", FacetID: "Other", Fragment: "x"})
	}
	if got := broker.events(t); len(got) != 3 {
		t.Errorf("unthrottled facet coalesced: %d events, want 3", len(got))
	}
}

// Two channels of the same stream throttle independently.
func TestThrottleGatesAreScopeScoped(t *testing.T) {
	c, err := Compile(streamSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	broker := &captureBroker{}
	a := New(c.Manifest, WithBroker(broker))
	a.Hub().EmitChannel("room:1", Event{Op: "append", FacetID: "LiveChat", Fragment: "r1"})
	a.Hub().EmitChannel("room:2", Event{Op: "append", FacetID: "LiveChat", Fragment: "r2"})
	if got := broker.events(t); len(got) != 2 {
		t.Errorf("independent channels were cross-throttled: %d events, want 2", len(got))
	}
}

// ── manifest meta: fail closed on malformed declarations ───────────────────

func TestCompileRejectsBadDurations(t *testing.T) {
	if _, err := Compile("stream X:\n    what:\n        m: str\n    throttle: fast\n    looks:\n        <div>{m}</div>\n"); err == nil {
		t.Error("want compile error for throttle: fast")
	}
	if _, err := Compile("signal Y:\n    what:\n        w: str\n    ttl: soon\n"); err == nil {
		t.Error("want compile error for ttl: soon")
	}
	if _, err := Compile("stream Z:\n    what:\n        m: str\n    window: many\n    looks:\n        <div>{m}</div>\n"); err == nil {
		t.Error("want compile error for window: many")
	}
}

func TestParseFacetMeta(t *testing.T) {
	c, err := Compile(streamSrc + "\n" + signalSrc)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rules, err := parseFacetMeta(c.Manifest)
	if err != nil {
		t.Fatalf("parseFacetMeta: %v", err)
	}
	if m := rules["LiveChat"]; m.kind != "stream" || m.throttle != 60*time.Millisecond || m.window != 100 {
		t.Errorf("LiveChat meta wrong: %+v", m)
	}
	if m := rules["Typing"]; m.kind != "signal" || m.ttl != 5*time.Second {
		t.Errorf("Typing meta wrong: %+v", m)
	}
}

func TestFacetName(t *testing.T) {
	for in, want := range map[string]string{
		"LikeButton:post:42": "LikeButton",
		"LiveChat":           "LiveChat",
		"fa:root":            "fa",
	} {
		if got := facetName(in); got != want {
			t.Errorf("facetName(%q) = %q, want %q", in, got, want)
		}
	}
}
