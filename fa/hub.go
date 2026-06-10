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
	maxConnsPerIP     = 16 // SSE connection cap per client IP (audit H2)
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

	mu      sync.RWMutex
	clients map[string]*sseClient      // connID → client (LOCAL to this instance)
	rooms   map[string]map[string]bool // channel → set of connID
	ipConns map[string]int             // client IP → live connection count
}

// fanoutMsg is the wire form of an emit, routed through the Broker to every
// instance. Event is signed before publish so all instances emit identical,
// client-verifiable frames (requires a shared signing key — see fa.New).
type fanoutMsg struct {
	Scope  string `json:"s"` // conn | identity | channel | broadcast
	Target string `json:"t,omitempty"`
	Event  Event  `json:"e"`
}

// sseClient is one active connection. Only the ServeSSE goroutine writes to the
// socket; everything else delivers through the buffered send channel.
type sseClient struct {
	id       string          // unguessable connection id
	identity string          // app-resolved identity ("" = anonymous)
	ip       string          // client IP (for the per-IP connection cap)
	channels map[string]bool // subscribed channels
	send     chan []byte
}

func newHub(key []byte, broker Broker) *Hub {
	if broker == nil {
		broker = newLocalBroker()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		ctx: ctx, cancel: cancel, key: key, broker: broker, metrics: &Metrics{},
		clients: make(map[string]*sseClient),
		rooms:   make(map[string]map[string]bool),
		ipConns: make(map[string]int),
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
	}
	if !h.register(c) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer h.unregister(c)

	hello, _ := json.Marshal(map[string]string{"op": "_conn", "conn": c.id})
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
	if c.ip != "" && h.ipConns[c.ip] >= maxConnsPerIP {
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
// matching local connections.
func (h *Hub) emit(scope, target string, events []Event) {
	for _, e := range events {
		sign(h.key, &e) // e is a copy (range value) — caller's slice not mutated
		msg, err := json.Marshal(fanoutMsg{Scope: scope, Target: target, Event: e})
		if err != nil {
			slog.Error("fa: marshal fanout", "err", err)
			continue
		}
		if err := h.broker.Publish(msg); err != nil {
			slog.Error("fa: broker publish", "err", err)
		}
		h.metrics.EventsOut.Add(1)
	}
}

// EmitConn delivers events to a single connection — the actor of an action.
// This is the default scope for a handler's returned events: no fan-out, no leak.
func (h *Hub) EmitConn(id string, events ...Event) { h.emit("conn", id, events) }

// EmitTo delivers events to every connection of an identity (a user across tabs/
// devices), wherever those connections live.
func (h *Hub) EmitTo(identity string, events ...Event) { h.emit("identity", identity, events) }

// EmitChannel delivers events to the subscribers of a channel (topic fan-out).
func (h *Hub) EmitChannel(channel string, events ...Event) { h.emit("channel", channel, events) }

// Broadcast delivers events to ALL connections. Public content only — anything
// user-specific must use EmitConn / EmitTo / EmitChannel.
func (h *Hub) Broadcast(events ...Event) { h.emit("broadcast", "", events) }

// deliverLocal applies a fanout message (from this or another instance) to this
// instance's matching connections.
func (h *Hub) deliverLocal(msg []byte) {
	var m fanoutMsg
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	line := sseLine(m.Event)
	if line == nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	switch m.Scope {
	case "conn":
		if c := h.clients[m.Target]; c != nil {
			h.sendLine(c, line)
		}
	case "identity":
		for _, c := range h.clients {
			if c.identity == m.Target {
				h.sendLine(c, line)
			}
		}
	case "channel":
		for id := range h.rooms[m.Target] {
			if c := h.clients[id]; c != nil {
				h.sendLine(c, line)
			}
		}
	case "broadcast":
		for _, c := range h.clients {
			h.sendLine(c, line)
		}
	}
}
