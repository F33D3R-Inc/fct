package fa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	heartbeatInterval = 25 * time.Second
	clientSendBuffer  = 64 // events buffered per client before dropping
	maxConnsPerIP     = 16 // default SSE connection cap per client IP (audit H2; fa.WithConnCap overrides)
)

// Hub is the SSE connection manager — one per process. It signs every event and
// delivers it to a SCOPED set of recipients:
//
//	EmitConn    → one connection (the actor of an action; the default scope)
//	EmitTo      → all connections of one identity (a user across tabs)
//	EmitChannel → subscribers of a channel (topic fan-out, subscription-authorized)
//	Broadcast   → every connection (PUBLIC content only)
//
// There is intentionally no "send to everyone" default: a handler's output goes
// only to the actor unless the app explicitly fans out. This closes the SSE
// data-leak (audit finding C1).
type Hub struct {
	ctx     context.Context
	cancel  context.CancelFunc
	key     []byte
	broker  Broker // cross-instance event fan-out
	metrics *Metrics
	tracer  Tracer // optional dispatch→render→emit trace hooks (see Tracer)
	connCap int    // per-IP SSE connection cap (fa.WithConnCap)

	rules map[string]facetMeta // per-primitive runtime rules (throttle enforcement)

	mu      sync.RWMutex
	clients map[string]*sseClient      // connID → client (LOCAL to this instance)
	rooms   map[string]map[string]bool // channel → set of connID
	ipConns map[string]int             // client IP → live connection count

	gateMu sync.Mutex
	gates  map[string]*throttleGate // scope\0target\0facetID → throttle state
}

// throttleGate is the per-(scope,target,facet-instance) state for a stream/pipe
// `throttle:`. Trailing-edge coalescing: the first event in a quiet period goes
// out immediately; events arriving inside the interval replace each other and
// the LATEST is flushed when the interval elapses. Intermediates are dropped —
// for a stream, delivering the final state is the contract; every intermediate
// frame is not.
type throttleGate struct {
	last    time.Time
	pending *fanoutMsg // latest deferred emit; nil when no flush is scheduled
}

// fanoutMsg is the wire form of an emit, routed through the Broker to every
// instance. Event is signed before publish so all instances emit identical,
// client-verifiable frames (requires a shared signing key — see fa.New).
type fanoutMsg struct {
	Scope  string `json:"s"` // conn | identity | channel | broadcast
	Target string `json:"t,omitempty"`
	Event  Event  `json:"e"`
	// Trace is the emitter's serialized trace context (Tracer.Inject), restored
	// by the delivering instance so a request is traceable across the broker.
	Trace string `json:"tr,omitempty"`
}

// sseClient is one active connection. Only the ServeSSE goroutine writes to the
// socket; everything else delivers through the buffered send channel.
type sseClient struct {
	id       string          // unguessable connection id
	identity string          // app-resolved identity ("" = anonymous)
	ip       string          // client IP (for the per-IP connection cap)
	channels map[string]bool // subscribed channels
	native   bool            // a native client (FA-Native) — gets styled trees, not HTML
	send     chan []byte
}

func newHub(key []byte, broker Broker, rules map[string]facetMeta, tracer Tracer) *Hub {
	if broker == nil {
		broker = newLocalBroker()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		ctx: ctx, cancel: cancel, key: key, broker: broker, metrics: &Metrics{}, tracer: tracer,
		connCap: maxConnsPerIP,
		rules:   rules,
		clients: make(map[string]*sseClient),
		rooms:   make(map[string]map[string]bool),
		ipConns: make(map[string]int),
		gates:   make(map[string]*throttleGate),
	}
	broker.Subscribe(h.deliverLocal) // apply events from this and other instances
	return h
}

// Shutdown ends all active SSE goroutines.
func (h *Hub) Shutdown() { h.cancel() }

func newConnID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ServeSSE handles GET /sse for a client with the given resolved identity
// ("" = anonymous). It assigns an unguessable connection id and sends it as the
// first frame (op "_conn") so the client can address its own connection when it
// POSTs to /events.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, identity string) {
	if !checkWireVersion(w, r) { // fail loud at connect, not weird at render
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	c := &sseClient{
		id: newConnID(), identity: identity, ip: clientIP(r),
		channels: make(map[string]bool), send: make(chan []byte, clientSendBuffer),
		native: r.Header.Get("FA-Native") == "1",
	}
	if !h.register(c) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer h.unregister(c)

	// The hello frame carries the connection id, the signing key (already public
	// — it ships in every web page's <meta name="fa-key">) so a native client can
	// verify pushed events before any arrive, and the server's wire version so
	// every client re-verifies compatibility on each (re)connect.
	hello, _ := json.Marshal(map[string]string{"op": "_conn", "conn": c.id, "key": hex.EncodeToString(h.key), "v": WireVersion})
	c.send <- []byte(fmt.Sprintf("data: %s\n\n", hello))

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-r.Context().Done():
			return
		case msg := <-c.send:
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// register adds a connection, enforcing the per-IP cap. Returns false (and adds
// nothing) when the client's IP already has maxConnsPerIP live connections.
func (h *Hub) register(c *sseClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ip != "" && h.ipConns[c.ip] >= h.connCap {
		return false
	}
	h.clients[c.id] = c
	if c.ip != "" {
		h.ipConns[c.ip]++
	}
	h.metrics.ConnsActive.Add(1)
	h.metrics.ConnsTotal.Add(1)
	return true
}

func (h *Hub) unregister(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c.id)
	h.metrics.ConnsActive.Add(-1)
	if c.ip != "" {
		if h.ipConns[c.ip]--; h.ipConns[c.ip] <= 0 {
			delete(h.ipConns, c.ip)
		}
	}
	for ch := range c.channels {
		if set := h.rooms[ch]; set != nil {
			delete(set, c.id)
			if len(set) == 0 {
				delete(h.rooms, ch)
			}
		}
	}
	h.mu.Unlock()
}

// connBelongs reports whether connID exists and may be acted on by identity.
// A connection bound to a non-empty identity can only be driven by that same
// identity (anti-spoof); anonymous connections are addressed by their
// unguessable id alone.
func (h *Hub) connBelongs(id, identity string) bool {
	if id == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[id]
	if !ok {
		return false
	}
	return c.identity == "" || c.identity == identity
}

func (h *Hub) subscribe(id, channel string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clients[id]
	if !ok {
		return
	}
	c.channels[channel] = true
	if h.rooms[channel] == nil {
		h.rooms[channel] = make(map[string]bool)
	}
	h.rooms[channel][id] = true
}

func (h *Hub) unsubscribe(id, channel string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.clients[id]; ok {
		delete(c.channels, channel)
	}
	if set := h.rooms[channel]; set != nil {
		delete(set, id)
		if len(set) == 0 {
			delete(h.rooms, channel)
		}
	}
}

// sseLine serialises an already-signed event into an SSE frame.
func sseLine(e Event) []byte {
	data, err := json.Marshal(e)
	if err != nil {
		slog.Error("fa: marshal event", "err", err)
		return nil
	}
	return []byte(fmt.Sprintf("data: %s\n\n", data))
}

func (h *Hub) sendLine(c *sseClient, line []byte) {
	if line == nil {
		return
	}
	select {
	case c.send <- line:
	default:
		slog.Warn("fa: client send buffer full, dropping event", "conn", c.id)
	}
}

// emit signs each event and publishes a routing message to the broker, which
// fans it out to every instance (including this one); deliverLocal applies it to
// matching local connections. Events targeting a stream/pipe with a declared
// `throttle:` go through a per-instance gate first (trailing-edge coalescing);
// throttling happens at the EMITTING instance, before the broker.
func (h *Hub) emit(ctx context.Context, scope, target string, events []Event) {
	if len(events) == 0 {
		return
	}
	ctx, end := h.span(ctx, "fa.emit", map[string]string{
		"fa.scope": scope, "fa.target": target, "fa.events": fmt.Sprint(len(events)),
	})
	defer end(nil)
	var trace string
	if h.tracer != nil {
		trace = h.tracer.Inject(ctx)
	}
	for _, e := range events {
		sign(h.key, &e) // e is a copy (range value) — caller's slice not mutated
		m := fanoutMsg{Scope: scope, Target: target, Event: e, Trace: trace}
		if d := h.throttleFor(e.FacetID); d > 0 {
			h.throttle(m, d)
			continue
		}
		h.publish(m)
	}
}

// publish sends one signed fanout message to the broker.
func (h *Hub) publish(m fanoutMsg) {
	msg, err := json.Marshal(m)
	if err != nil {
		slog.Error("fa: marshal fanout", "err", err)
		return
	}
	if err := h.broker.Publish(msg); err != nil {
		slog.Error("fa: broker publish", "err", err)
	}
	h.metrics.EventsOut.Add(1)
}

// throttleFor returns the declared throttle interval for the primitive a
// facet-id instance belongs to (0 = unthrottled).
func (h *Hub) throttleFor(facetID string) time.Duration {
	if facetID == "" || len(h.rules) == 0 {
		return 0
	}
	return h.rules[facetName(facetID)].throttle
}

// throttle applies trailing-edge coalescing to one emit. The gate key includes
// scope and target so e.g. two channels of the same stream throttle
// independently (one room's chatter cannot starve another room's update).
func (h *Hub) throttle(m fanoutMsg, d time.Duration) {
	key := m.Scope + "\x00" + m.Target + "\x00" + m.Event.FacetID
	now := time.Now()

	h.gateMu.Lock()
	g := h.gates[key]
	if g == nil {
		g = &throttleGate{}
		h.gates[key] = g
	}
	if g.pending != nil {
		g.pending = &m // a flush is already scheduled — latest wins
		h.gateMu.Unlock()
		return
	}
	since := now.Sub(g.last)
	if since >= d {
		g.last = now
		h.gateMu.Unlock()
		h.publish(m)
		return
	}
	g.pending = &m
	h.gateMu.Unlock()
	time.AfterFunc(d-since, func() {
		if h.ctx.Err() != nil {
			return // hub shut down — drop the pending frame
		}
		h.gateMu.Lock()
		pending := g.pending
		g.pending = nil
		g.last = time.Now()
		h.gateMu.Unlock()
		if pending != nil {
			h.publish(*pending)
		}
	})
}

// EmitConn delivers events to a single connection — the actor of an action.
// This is the default scope for a handler's returned events: no fan-out, no leak.
func (h *Hub) EmitConn(id string, events ...Event) { h.emit(context.Background(), "conn", id, events) }

// EmitTo delivers events to every connection of an identity (a user across tabs/
// devices), wherever those connections live.
func (h *Hub) EmitTo(identity string, events ...Event) {
	h.emit(context.Background(), "identity", identity, events)
}

// EmitChannel delivers events to the subscribers of a channel (topic fan-out).
func (h *Hub) EmitChannel(channel string, events ...Event) {
	h.emit(context.Background(), "channel", channel, events)
}

// Broadcast delivers events to ALL connections. Public content only — anything
// user-specific must use EmitConn / EmitTo / EmitChannel.
func (h *Hub) Broadcast(events ...Event) { h.emit(context.Background(), "broadcast", "", events) }

// deliverLocal applies a fanout message (from this or another instance) to this
// instance's matching connections.
func (h *Hub) deliverLocal(msg []byte) {
	var m fanoutMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	start := time.Now()
	defer func() { h.metrics.FanoutSeconds.Observe(time.Since(start)) }()
	if h.tracer != nil {
		ctx := h.tracer.Extract(h.ctx, m.Trace) // child of the emitting instance's span
		_, end := h.tracer.StartSpan(ctx, "fa.deliver", map[string]string{
			"fa.scope": m.Scope, "fa.op": m.Event.Op, "fa.facet_id": m.Event.FacetID,
		})
		defer end(nil)
	}
	// Web connections get the HTML fragment; native connections get the same event
	// with the server-resolved, styled neutral tree (built once, lazily). The
	// signature is unchanged (it covers op/facet_id/fragment), so the web frame is
	// untouched and native clients need no style table of their own.
	var webLine, nativeLine []byte
	web := func() []byte {
		if webLine == nil {
			webLine = sseLine(m.Event)
		}
		return webLine
	}
	nat := func() []byte {
		if nativeLine == nil {
			nativeLine = sseLine(h.nativeEvent(m.Event))
		}
		return nativeLine
	}
	lineFor := func(c *sseClient) []byte {
		if c.native {
			return nat()
		}
		return web()
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	switch m.Scope {
	case "conn":
		if c := h.clients[m.Target]; c != nil {
			h.sendLine(c, lineFor(c))
		}
	case "identity":
		for _, c := range h.clients {
			if c.identity == m.Target {
				h.sendLine(c, lineFor(c))
			}
		}
	case "channel":
		for id := range h.rooms[m.Target] {
			if c := h.clients[id]; c != nil {
				h.sendLine(c, lineFor(c))
			}
		}
	case "broadcast":
		for _, c := range h.clients {
			h.sendLine(c, lineFor(c))
		}
	}
}

// nativeEvent transforms a web event (HTML fragment) into the native form: the
// fragment becomes the styled neutral tree as a JSON string, and the event is
// RE-SIGNED over those bytes. This keeps verification symmetric — a native client
// hashes the fragment it received (the tree JSON) exactly as a web client hashes
// its HTML — so the tree the device renders is authenticated, and the style table
// stays solely on the server.
func (h *Hub) nativeEvent(e Event) Event {
	if e.Op == "signal" {
		return e // a signal's fragment is already a JSON payload, not HTML
	}
	if e.Fragment != "" {
		if tree, err := ParseView(e.Fragment); err == nil {
			if js, err := json.Marshal(tree); err == nil {
				e.Fragment = string(js)
				e.HMAC = ""
				sign(h.key, &e)
			}
		}
	}
	return e
}
