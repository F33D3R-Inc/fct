package fa

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"github.com/F33D3R-Inc/fct/runtime"
)

// Ctx is passed to an event Handler. It carries the decoded client action plus
// the actor's resolved identity and connection. A handler's RETURNED events are
// delivered only to this actor (Ctx.ConnID); for broader fan-out a handler calls
// Ctx.EmitChannel / Ctx.EmitTo / Ctx.Broadcast explicitly.
type Ctx struct {
	Type     string            // the data-action / event type
	Payload  map[string]string // remaining data-* attributes from the element
	Identity string            // app-resolved identity of the actor ("" = anon)
	ConnID   string            // the actor's SSE connection
	R        *http.Request
	app      *App
}

// EmitChannel pushes events to every subscriber of a channel (topic fan-out).
func (c Ctx) EmitChannel(channel string, events ...Event) { c.app.hub.EmitChannel(channel, events...) }

// EmitTo pushes events to all connections of an identity (a user across tabs).
func (c Ctx) EmitTo(identity string, events ...Event) { c.app.hub.EmitTo(identity, events...) }

// Broadcast pushes events to every connection. Public content only.
func (c Ctx) Broadcast(events ...Event) { c.app.hub.Broadcast(events...) }

// Handler processes one client event and returns the DOM mutations to push to
// the ACTOR. This is where application logic lives — on the server, where the
// real state is. Returning an error responds 500 and pushes nothing.
type Handler func(Ctx) ([]Event, error)

// App ties a compiled manifest, an event router, and the SSE hub together and
// mounts them on an http.ServeMux. An FA application is: New → On(...) → Mount.
type App struct {
	hub      *Hub
	manifest []byte

	metrics  *Metrics
	draining atomic.Bool // set during graceful shutdown so /readyz fails

	mu          sync.RWMutex
	handlers    map[string]Handler
	guards      map[string]func(Ctx) bool // per-event authorization (audit C2)
	key         []byte
	identify    func(*http.Request) string          // resolves a connection's identity
	channelAuth func(identity, channel string) bool // channel_auth (default: deny)
	limiter     *rateLimiter                        // per-IP /events throttle (audit H2)

	routes    []*route     // URL routes (see router.go)
	shellOpts ShellOptions // base Playground chrome for routed pages
	notFound  PageFunc     // optional custom 404 content
}

// Option configures an App at construction (see WithSigningKey, WithBroker).
type Option func(*appConfig)

type appConfig struct {
	key    []byte
	broker Broker
}

// WithSigningKey sets a STABLE event-signing key. Use this (or the FA_SIGNING_KEY
// env var) in production: a key that is shared across instances and stable across
// restarts is REQUIRED for pushed-event signatures to verify after a redeploy or
// when load-balanced across instances.
func WithSigningKey(key []byte) Option { return func(c *appConfig) { c.key = key } }

// WithBroker sets the cross-instance event Broker (default: in-process). A
// Redis/NATS-backed Broker is what makes a multi-instance deployment deliver
// events correctly. See the Broker docs.
func WithBroker(b Broker) Option { return func(c *appConfig) { c.broker = b } }

var ephemeralKeyOnce sync.Once

// New creates an App for a compiled manifest. The signing key resolves in order:
// WithSigningKey option → FA_SIGNING_KEY env (hex, ≥16 bytes) → a random
// ephemeral key (dev only — logged once, because ephemeral keys break event
// verification across restarts and instances). Embed Key() in the page shell as
// <meta name="fa-key"> so the client can verify pushed events.
func New(manifest []byte, opts ...Option) *App {
	var cfg appConfig
	for _, o := range opts {
		o(&cfg)
	}

	key := cfg.key
	if key == nil {
		if hx := os.Getenv("FA_SIGNING_KEY"); hx != "" {
			if k, err := hex.DecodeString(hx); err == nil && len(k) >= 16 {
				key = k
			} else {
				slog.Warn("fa: FA_SIGNING_KEY is invalid (need hex, ≥16 bytes); ignoring")
			}
		}
	}
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("fa: cannot read random key: " + err.Error())
		}
		ephemeralKeyOnce.Do(func() {
			slog.Warn("fa: using an EPHEMERAL signing key — pushed-event signatures will not survive a restart or work across instances; set FA_SIGNING_KEY (hex) or fa.WithSigningKey for production")
		})
	}

	hub := newHub(key, cfg.broker)
	return &App{
		hub:      hub,
		manifest: manifest,
		metrics:  hub.metrics,
		handlers: make(map[string]Handler),
		guards:   make(map[string]func(Ctx) bool),
		limiter:  newRateLimiter(20, 40), // ~20 events/sec/IP, burst 40
		key:      key,
	}
}

// Shutdown gracefully drains the app: it marks /readyz unhealthy (so a load
// balancer stops sending traffic and lets existing SSE connections finish), then
// closes the SSE connections. Call this on SIGTERM.
func (a *App) Shutdown() {
	a.draining.Store(true)
	a.hub.Shutdown()
}

// On registers a handler for a client event type. Chainable.
func (a *App) On(eventType string, h Handler) *App {
	a.mu.Lock()
	a.handlers[eventType] = h
	a.mu.Unlock()
	return a
}

// Identify sets the resolver that maps an SSE/events request to a stable
// identity (e.g. a user or session id). "" means anonymous (the connection is
// then addressed only by its unguessable id). Chainable.
func (a *App) Identify(f func(*http.Request) string) *App {
	a.identify = f
	return a
}

// ChannelAuth sets the authorizer deciding whether identity may subscribe to a
// channel. Default is DENY (fail closed): with no authorizer set, no channel
// subscription is allowed. Chainable.
func (a *App) ChannelAuth(f func(identity, channel string) bool) *App {
	a.channelAuth = f
	return a
}

func (a *App) identityOf(r *http.Request) string {
	if a.identify == nil {
		return ""
	}
	return a.identify(r)
}

func (a *App) authorizeChannel(identity, channel string) bool {
	if a.channelAuth == nil {
		return false // secure default: deny
	}
	return a.channelAuth(identity, channel)
}

// Guard registers an authorization check for an event type. Before the handler
// runs, the guard receives the action Ctx (identity, payload, connection);
// returning false responds 403 and the handler never executes. This makes
// per-event authorization structural rather than advisory (audit C2). Chainable.
func (a *App) Guard(eventType string, fn func(Ctx) bool) *App {
	a.mu.Lock()
	a.guards[eventType] = fn
	a.mu.Unlock()
	return a
}

// Dispatch runs the registered guard and handler for an event and returns the
// events the handler produced — WITHOUT a live SSE connection or pushing. It is
// the seam for unit-testing handlers (see the fatest package). Returns
// ErrForbidden if a guard denies, or an error if no handler is registered.
// Events a handler sends via Ctx.Emit* go through the hub/broker as usual.
func (a *App) Dispatch(eventType string, payload map[string]string, identity string) ([]Event, error) {
	a.mu.RLock()
	h := a.handlers[eventType]
	g := a.guards[eventType]
	a.mu.RUnlock()
	if h == nil {
		return nil, fmt.Errorf("fa: no handler for event %q", eventType)
	}
	ctx := Ctx{Type: eventType, Payload: payload, Identity: identity, app: a}
	if g != nil && !g(ctx) {
		return nil, ErrForbidden
	}
	return h(ctx)
}

// Hub exposes the SSE hub for server-initiated pushes (events not triggered by a
// client action).
func (a *App) Hub() *Hub { return a.hub }

// Key returns the hex signing key to embed in the shell's <meta name="fa-key">.
func (a *App) Key() string { return hex.EncodeToString(a.key) }

// Page wraps content in the Playground document (the base canvas), embedding
// this app's signing key so the client can verify pushed events.
func (a *App) Page(content template.HTML, opts ShellOptions) template.HTML {
	return renderShell(a.Key(), content, opts)
}

// HandlePage registers GET / to serve the Playground with content(r) inside the
// root mount. This is the out-of-the-box page: no hand-written HTML shell.
func (a *App) HandlePage(mux *http.ServeMux, opts ShellOptions, content func(*http.Request) template.HTML) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		secureHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(a.Page(content(r), opts)))
	})
}

// secureHeaders applies a secure-by-default header set to an HTML page (audit
// M1). script-src is locked to 'self' — no inline scripts, no eval — so any
// injected <script> or on*="" handler that ever slips the escaper cannot run;
// CSP is the backstop behind html/template's auto-escaping. Styles allow inline
// for ergonomics (style injection is far lower risk than script). object-src
// 'none' and base-uri 'none' close plugin/base-tag tricks; frame-ancestors
// 'none' stops clickjacking. Apps may override by setting headers after Mount.
func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data: https:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
}

// Mount registers the FA endpoints on mux: SSE, the single events sink, the
// manifest, and the client runtime.
func (a *App) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /sse", func(w http.ResponseWriter, r *http.Request) {
		a.hub.ServeSSE(w, r, a.identityOf(r))
	})
	mux.HandleFunc("POST /events", a.handleEvents)
	a.mountObservability(mux)
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(a.manifest)
	})
	mux.HandleFunc("GET /fa-runtime.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", runtime.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(runtime.Script)
	})
}

// handleEvents is the single server-side sink for all client actions. The client
// POSTs {type, payload, conn} here; the server routes by type. A handler's
// returned events are delivered ONLY to the acting connection (conn) — never
// broadcast — so nothing leaks to other clients (audit C1).
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	a.metrics.EventsIn.Add(1)
	if !sameOrigin(r) { // defense-in-depth CSRF (H1)
		a.metrics.Forbidden.Add(1)
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if !a.limiter.allow(clientIP(r)) { // per-IP throttle (H2)
		a.metrics.RateLimited.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Type    string            `json:"type"`
		Payload map[string]string `json:"payload"`
		Conn    string            `json:"conn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	identity := a.identityOf(r)

	// Reserved control events: channel subscription (authorized) / unsubscription.
	switch req.Type {
	case "fa.subscribe":
		channel := req.Payload["channel"]
		if channel == "" || !a.hub.connBelongs(req.Conn, identity) {
			http.Error(w, "bad subscription", http.StatusForbidden)
			return
		}
		if !a.authorizeChannel(identity, channel) {
			http.Error(w, "channel not authorized", http.StatusForbidden)
			return
		}
		a.hub.subscribe(req.Conn, channel)
		w.WriteHeader(http.StatusNoContent)
		return
	case "fa.unsubscribe":
		a.hub.unsubscribe(req.Conn, req.Payload["channel"])
		w.WriteHeader(http.StatusNoContent)
		return
	}

	a.mu.RLock()
	h := a.handlers[req.Type]
	a.mu.RUnlock()
	if h == nil {
		http.Error(w, "no handler for event "+req.Type, http.StatusNotFound)
		return
	}
	// The acting connection must exist and belong to the requester (anti-spoof).
	if !a.hub.connBelongs(req.Conn, identity) {
		a.metrics.Forbidden.Add(1)
		http.Error(w, "unknown or mismatched connection", http.StatusForbidden)
		return
	}
	ctx := Ctx{Type: req.Type, Payload: req.Payload, Identity: identity, ConnID: req.Conn, R: r, app: a}
	// Authorization guard (structural, enforced before any handler logic).
	a.mu.RLock()
	guard := a.guards[req.Type]
	a.mu.RUnlock()
	if guard != nil && !guard(ctx) {
		a.metrics.Forbidden.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	events, err := h(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.hub.EmitConn(req.Conn, events...) // actor only — no fan-out by default
	w.WriteHeader(http.StatusNoContent)
}
