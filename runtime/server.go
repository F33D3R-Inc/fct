// Package runtime executes a compiled Facet IR. The server is the authority: it
// owns the durable entity store (shared, persisted) and per-session server
// state, runs server-placed actions, and enforces policies. The browser holds
// client state and runs client-placed actions locally. Both interpret the same
// IR, so there is one application projected to two execution domains the
// compiler chose.
package runtime

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"facet/internal/ir"
)

//go:embed assets/facet.js
var clientJS []byte

// Server serves one compiled app.
type Server struct {
	ir          *ir.IR
	byAction    map[string]*ir.Action
	byBind      map[string]*ir.Binding
	byPolicy    map[string]*ir.Policy
	byComponent map[string]*ir.Component
	uploadDir   string // directory uploaded files are written to and served from
	store       Store
	children    map[string][]childRef // entity -> the relations that point at it (for cascade)
	secure      bool                  // mark cookies Secure (TLS / FACET_SECURE_COOKIES=1)

	mu       sync.Mutex
	entities map[string][]any         // durable, shared (the in-memory working set; the Store is the source of truth)
	nextID   map[string]int           // per-entity id counter
	sessions map[string]*sessionState // sid -> session (per-session scalar state + identity + expiry)
	nextSID  int

	limiter *rateLimiter  // per-IP request throttle on state-changing endpoints
	lockout *lockout      // per-username brute-force login lockout
	audit   *auditLog     // append-only record of every server action
	oidc    *oidcProvider // optional OIDC SSO (nil unless configured)

	obs      *obs         // structured logs + metrics + tracing
	cluster  *cluster     // cross-instance pub/sub + shared sessions (nil unless FACET_CLUSTER)
	jobs     *jobQueue    // durable job workers (nil until StartJobs)
	apiCache *apiCache    // optional read cache for entity-list GETs (nil unless FACET_API_CACHE_TTL)
	dev      *devHub      // live-reload hub (nil unless `facet dev`)
	i18n     *i18nCatalog // message catalogs for localization (empty until FACET_I18N_DIR)

	subsMu sync.Mutex           // guards subs
	subs   map[chan []byte]bool // live SSE connections (shared-state fan-out)
}

// sessionState is one session: its per-session server (scalar) state, the signed
// -in identity (actor/role/verified), and a sliding expiry. Client state never
// lives here — the authority cannot see it.
type sessionState struct {
	state    map[string]any
	actor    string // signed-in username, else "guest"
	role     string // actor role: admin | member | guest
	verified bool   // the account's email/contact is verified
	expires  time.Time
	// pendingMFA holds the username that passed a password check and now awaits a
	// TOTP second factor (set by login, consumed by loginMFA).
	pendingMFA string
}

// childRef is one reverse relation: rows of Entity reference a parent through
// Field. When the parent is removed, the database cascades the delete down these
// edges (ON DELETE CASCADE); the runtime mirrors the same cascade in the
// in-memory working set so live clients converge.
type childRef struct {
	Entity string
	Field  string
}

// New builds a server for a compiled IR, opening the Postgres entity store
// (FACET_DATABASE_URL) and loading its rows into the in-memory working set. It
// fails if the database cannot be reached — the app does not run without it.
func New(graph *ir.IR) (*Server, error) {
	s := newServer(graph)
	store, err := openStore(os.Getenv("FACET_DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	if err := s.attachStore(store); err != nil {
		return nil, err
	}
	return s, nil
}

// newServer builds the storeless half of a server: the lookup maps over the IR
// and the reverse-relation graph. attachStore then wires in a backend.
func newServer(graph *ir.IR) *Server {
	// Phase 6: add the reserved tenancy/billing tables for the enabled enterprise
	// features before anything reads the entity set, so they ride the same load and
	// migration path as a declared entity.
	injectEnterpriseEntities(graph)
	s := &Server{
		ir:          graph,
		byAction:    map[string]*ir.Action{},
		byBind:      map[string]*ir.Binding{},
		byPolicy:    map[string]*ir.Policy{},
		byComponent: map[string]*ir.Component{},
		uploadDir:   uploadDirFromEnv(),
		entities:    map[string][]any{},
		nextID:      map[string]int{},
		sessions:    map[string]*sessionState{},
		subs:        map[chan []byte]bool{},
		secure:      os.Getenv("FACET_SECURE_COOKIES") == "1",
		limiter:     newRateLimiter(rateLimitFromEnv()),
		lockout:     newLockout(),
		obs:         newObs(),
		apiCache:    newAPICache(),
		i18n:        newI18n(),
	}
	for i := range graph.Actions {
		s.byAction[graph.Actions[i].Name] = &graph.Actions[i]
	}
	// Index every page's bindings, not just the entry page's — a multi-screen app
	// renders binds that live on whichever screen the request hit, so server-side
	// rendering needs the union across all pages.
	for pi := range graph.Pages {
		for i := range graph.Pages[pi].Bindings {
			s.byBind[graph.Pages[pi].Bindings[i].ID] = &graph.Pages[pi].Bindings[i]
		}
	}
	for i := range graph.Policies {
		s.byPolicy[graph.Policies[i].Name] = &graph.Policies[i]
	}
	for i := range graph.Components {
		s.byComponent[graph.Components[i].Name] = &graph.Components[i]
	}
	// reverse-relation graph: parent entity -> children that reference it.
	s.children = map[string][]childRef{}
	for _, e := range graph.Entities {
		for _, f := range e.Fields {
			if f.IsRelation() {
				s.children[f.Ref] = append(s.children[f.Ref], childRef{Entity: e.Name, Field: f.Name})
			}
		}
	}
	return s
}

// NewInMemory builds a server backed by a volatile in-memory store — no database
// required. It powers the developer-experience tooling (`facet test`, `facet
// console`, `facet seed --dry`), which exercises the real runtime without
// touching Postgres. Production traffic always goes through New (Postgres).
func NewInMemory(graph *ir.IR) (*Server, error) {
	s := newServer(graph)
	if err := s.attachStore(newMemStore()); err != nil {
		return nil, err
	}
	return s, nil
}

// attachStore wires a Store into a half-built server: it initializes the schema,
// loads the working set, seeds the audit feed, and brings up optional OIDC and
// clustering. New and NewInMemory share it so both backends behave identically.
func (s *Server) attachStore(store Store) error {
	graph := s.ir
	s.store = store
	loaded, err := store.Init(graph.Entities)
	if err != nil {
		store.Close()
		return fmt.Errorf("load entity data: %w", err)
	}
	for _, e := range graph.Entities {
		rows := loaded[e.Name]
		if rows == nil {
			rows = []any{}
		}
		s.entities[e.Name] = rows
		max := 0
		for _, r := range rows {
			if m, ok := r.(record); ok {
				if id := toInt(m["id"]); id > max {
					max = id
				}
			}
		}
		s.nextID[e.Name] = max
	}

	// Audit log: write through to the durable table, seeded from its recent history
	// so the in-memory feed survives a restart.
	s.audit = newAuditLog(func(e auditEntry) { go s.store.Audit(e) })
	if seed, err := s.store.RecentAudit(s.audit.size); err == nil {
		s.audit.seed(seed)
	}

	// Optional OIDC single sign-on, configured entirely from the environment.
	s.oidc = newOIDCFromEnv()

	// Horizontal scale (opt-in): join the cross-instance event bus and back
	// sessions with the shared store, so many instances can run behind one LB.
	if clusterEnabled() {
		c, err := startCluster(s)
		if err != nil {
			store.Close()
			return fmt.Errorf("start cluster: %w", err)
		}
		s.cluster = c
	}
	return nil
}

// Handler returns the HTTP routes for the app.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/facet.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(clientJS)
	})
	mux.HandleFunc("/event", s.handleEvent)
	mux.HandleFunc("/live", s.handleLive)
	mux.HandleFunc("/api", s.handleAPISchema)
	mux.HandleFunc("/api/", s.handleAPI)
	mux.HandleFunc("/upload", s.handleUpload)
	mux.HandleFunc("/uploads/", s.handleUploads)
	// Phase 6: the generated admin dashboard and the billing provider webhook.
	mux.HandleFunc("/admin", s.handleAdmin)
	mux.HandleFunc("/admin/", s.handleAdmin)
	mux.HandleFunc("/billing/webhook", s.handleBillingWebhook)
	if s.dev != nil {
		mux.HandleFunc("/dev/reload", s.handleDevReload)
	}
	if s.oidc != nil {
		mux.HandleFunc("/auth/oidc/login", s.handleOIDCLogin)
		mux.HandleFunc("/auth/oidc/callback", s.handleOIDCCallback)
	}
	// Operations endpoints: liveness/readiness probes for orchestrators and the
	// Prometheus scrape target. They sit outside the app's routes.
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/", s.handlePage)
	// Wrap the whole mux in the observability middleware: every request becomes a
	// span, a structured access log line, and a metrics sample.
	return s.obs.observe(mux)
}

// handleLive is the real-time transport: one SSE stream per client. Changes to
// shared/durable state (entities) are pushed here to every connected client, so
// a post in one tab appears in all the others with no refresh. Per-session
// scalar state is NOT broadcast — it belongs to its own session.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 16)
	s.subsMu.Lock()
	s.subs[ch] = true
	s.subsMu.Unlock()
	s.obs.metrics.addSSE(1)
	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
		s.obs.metrics.addSSE(-1)
	}()

	// Send a current snapshot of all entities so a client that connected after a
	// change cannot miss it (closes the page-load → stream-open race).
	s.mu.Lock()
	snap := map[string]any{}
	for ent, rows := range s.entities {
		if isReservedEntity(ent) {
			continue // never stream the credential store or other runtime-managed tables
		}
		snap[ent] = rows
	}
	s.mu.Unlock()
	if data, err := json.Marshal(map[string]any{"deltas": snap}); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// broadcast fans a deltas payload out to this instance's live clients, then —
// when clustering is on — announces the changed entities to peer instances so
// their clients converge too. Any entity write also invalidates the API read
// cache, so a cached list can never outlive the data it was built from.
func (s *Server) broadcast(deltas map[string]any) {
	if len(deltas) == 0 {
		return
	}
	s.apiCache.invalidate()
	s.fanout(deltas)
	if s.cluster != nil {
		names := make([]string, 0, len(deltas))
		for ent := range deltas {
			names = append(names, ent)
		}
		s.cluster.publish(names)
	}
}

// fanout pushes a deltas payload to every live client on this instance only
// (non-blocking; a client whose buffer is full is skipped — it will catch up on
// its next snapshot). The cluster receive path calls this directly to avoid
// re-publishing a peer's change.
func (s *Server) fanout(deltas map[string]any) {
	if len(deltas) == 0 {
		return
	}
	data, err := json.Marshal(map[string]any{"deltas": deltas})
	if err != nil {
		return
	}
	s.subsMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- data:
		default:
		}
	}
	s.subsMu.Unlock()
}

// handlePage routes the request to the view whose path matches, server-renders
// it (first paint), and embeds that page's IR + the initial state the client
// runtime renders from. Following a link re-enters here for the next page.
func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	pg, params := s.pageFor(r.URL.Path)
	if pg == nil {
		http.NotFound(w, r)
		return
	}
	sid := s.session(w, r)
	store := s.fullStore(sid)
	// Route parameters (`/post/:id`) enter the render scope as text values, visible
	// to both the first-paint render and the client (they ride in the state JSON).
	for k, v := range params {
		store[k] = v
	}

	// Route guard: a zero-arg policy the authority enforces before rendering. A
	// failing guard refuses the page — the client also hides links to it, but the
	// server is the enforcement point. For a composed screen, a failure isn't a
	// dead end: the actor is redirected to the first screen they may enter (a guest
	// at "/" lands on the login screen; a member at "/login" lands on home), so the
	// auth state routes between screens without any redirect code in the app.
	if pg.Requires != "" && !s.guardOK(pg.Requires, store) {
		if pg.Screen {
			if to := s.firstEnterableScreen(pg.Path, store); to != "" {
				http.Redirect(w, r, to, http.StatusFound)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, guardedPage, html.EscapeString(s.ir.App))
		return
	}

	var body strings.Builder
	for _, n := range pg.View {
		s.renderNode(&body, n, store)
	}

	// Ship the IR with this page's view/bindings/depgraph (the client runtime
	// reads those three fields), not every page's tree. Routes (lightweight) stay,
	// so the client can match links and hide guarded ones.
	reqIR := *s.ir
	reqIR.View = pg.View
	reqIR.Bindings = pg.Bindings
	reqIR.DepGraph = pg.DepGraph
	reqIR.Pages = nil
	irJSON, _ := json.Marshal(&reqIR)
	stateJSON, _ := json.Marshal(store)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Phase 6: stamp the negotiated locale so a client knows which catalog to load.
	w.Header().Set("Content-Language", s.i18n.localeFor(r))
	dev := ""
	if s.dev != nil {
		dev = devScript
	}
	fmt.Fprintf(w, page, html.EscapeString(s.ir.App), csrfToken(sid), themeCSS(s.ir.Theme), body.String(), irJSON, stateJSON, dev)
}

// guardOK evaluates a zero-arg guard policy against the current store.
func (s *Server) guardOK(policy string, store map[string]any) bool {
	pol := s.byPolicy[policy]
	return pol != nil && truthy(eval(pol.Expr, store))
}

// firstEnterableScreen returns the path of the first composed screen (other than
// exceptPath) whose guard the current actor satisfies, or "" if none. It is how a
// failed screen guard finds where to send the actor instead of a dead end.
func (s *Server) firstEnterableScreen(exceptPath string, store map[string]any) string {
	for i := range s.ir.Pages {
		p := &s.ir.Pages[i]
		if !p.Screen || p.Path == exceptPath {
			continue
		}
		if p.Requires == "" || s.guardOK(p.Requires, store) {
			return p.Path
		}
	}
	return ""
}

// pageFor returns the page whose route matches path, plus the bound `:param`
// values. An exact (static) route wins over a dynamic one.
func (s *Server) pageFor(path string) (*ir.Page, map[string]string) {
	for i := range s.ir.Pages {
		if s.ir.Pages[i].Path == path {
			return &s.ir.Pages[i], nil
		}
	}
	for i := range s.ir.Pages {
		if params, ok := matchRoute(s.ir.Pages[i].Path, path); ok {
			return &s.ir.Pages[i], params
		}
	}
	return nil, nil
}

// matchRoute matches a concrete path against a route pattern, binding each
// `:param` segment to its concrete value. ok is false if they do not match.
func matchRoute(pattern, path string) (map[string]string, bool) {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	cs := strings.Split(strings.Trim(path, "/"), "/")
	if len(ps) != len(cs) {
		return nil, false
	}
	params := map[string]string{}
	for i := range ps {
		if strings.HasPrefix(ps[i], ":") {
			if cs[i] == "" {
				return nil, false
			}
			if dec, err := url.PathUnescape(cs[i]); err == nil {
				params[ps[i][1:]] = dec
			} else {
				params[ps[i][1:]] = cs[i]
			}
			continue
		}
		if ps[i] != cs[i] {
			return nil, false
		}
	}
	return params, true
}

// themeCSS renders the app's theme variables as a `:root` block of CSS custom
// properties (`--fa-<name>`), so the whole UI restyles from one declarative
// source. Empty when no `theme:` block is declared.
func themeCSS(theme map[string]string) string {
	if len(theme) == 0 {
		return ""
	}
	names := make([]string, 0, len(theme))
	for k := range theme {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(":root{")
	for _, k := range names {
		fmt.Fprintf(&b, "--fa-%s:%s;", k, strings.ReplaceAll(theme[k], "}", ""))
	}
	b.WriteString("}")
	return b.String()
}

// guardMutation applies the edge defenses every state-changing request passes
// through: a per-IP rate limit (cheap flood protection) and, on the browser
// channel, CSRF validation. It writes the error response itself and returns
// false when the request must stop.
//
// CSRF is enforced on /event (the cookie-authenticated browser channel) by a
// per-session token a cross-origin page cannot read. /api is the programmatic
// projection and is not token-gated; its cross-site protection is SameSite=Lax
// on the session cookie, which browsers do not send on a cross-site POST.
func (s *Server) guardMutation(w http.ResponseWriter, r *http.Request, requireCSRF bool) bool {
	if !s.limiter.allow(clientIP(r)) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return false
	}
	if requireCSRF && !s.checkCSRF(r) {
		http.Error(w, "missing or invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

// checkCSRF validates the anti-forgery token on a cookie-authenticated request.
// A request with no (or an unsigned) session cookie has no ambient authority to
// abuse, so it is allowed; a cookie-authenticated one must present the matching
// per-session token in the X-Facet-CSRF header.
func (s *Server) checkCSRF(r *http.Request) bool {
	c, err := r.Cookie("fa_sid")
	if err != nil {
		return true
	}
	sid, ok := verifySigned(c.Value)
	if !ok {
		return true // a garbage cookie yields a fresh session anyway
	}
	return csrfValid(sid, r.Header.Get("X-Facet-CSRF"))
}

// handleEvent runs a server-placed action: binds arguments, enforces required
// policies, executes the statements against authoritative state, persists entity
// changes, and returns the state it changed as deltas. The client then refreshes
// only the regions those states feed.
func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.guardMutation(w, r, true) {
		return
	}
	var req struct {
		Action string `json:"action"`
		Args   []any  `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if s.ir.Auth && isAuthAction(req.Action) {
		s.runAuth(w, r, req.Action, req.Args)
		return
	}
	if s.runReserved(w, r, req.Action, req.Args) {
		return
	}
	act := s.byAction[req.Action]
	if act == nil {
		http.Error(w, "unknown action", http.StatusNotFound)
		return
	}
	if act.Placement != ir.Server {
		http.Error(w, "action is client-placed", http.StatusBadRequest)
		return
	}

	sid := s.session(w, r)
	deltas, status, msg := s.runAction(sid, act, req.Args)
	if status != http.StatusOK {
		http.Error(w, msg, status)
		return
	}
	if len(deltas) > 0 {
		s.persistSession(sid) // the action changed per-session state; share it
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"deltas": deltas})
}

// runAction is the one authoritative execution of a server-placed action, shared
// by every projection that can trigger one — the web event channel, the JSON
// API, and scheduled jobs. It binds arguments, enforces required policies,
// applies the statements to authoritative state under the lock, persists and
// fans out entity changes, and returns the per-session scalar deltas (plus an
// HTTP-shaped status so each caller can report failures in its own idiom).
func (s *Server) runAction(sid string, act *ir.Action, args []any) (map[string]any, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scope := s.scope(sid)
	for i, p := range act.Params {
		var v any
		if i < len(args) {
			v = coerce(args[i], p.Type)
		} else {
			v = zero(p.Type)
		}
		scope[p.Name] = v
	}
	actor := toStr(scope["actor"])

	// permission gate — the authority's job. A row-level policy is evaluated with
	// its arguments (from the `requires` call site) bound; a denial is audited.
	for _, req := range act.Requires {
		if !s.policyPasses(req, scope) {
			s.recordAudit(actor, act.Name, false, "denied: "+req.Name)
			s.obs.metrics.observeAction(act.Name, "denied")
			return nil, http.StatusForbidden, "forbidden: " + req.Name
		}
	}

	// Input validation: each `check` is a precondition over the bound arguments and
	// the actor. A failure aborts before any state changes and returns its friendly
	// message, so invalid input never reaches the store.
	for _, chk := range act.Checks {
		if !truthy(eval(chk.Cond, scope)) {
			s.recordAudit(actor, act.Name, false, "check failed: "+chk.Msg)
			s.obs.metrics.observeAction(act.Name, "invalid")
			return nil, http.StatusUnprocessableEntity, chk.Msg
		}
	}

	deltas := map[string]any{}
	entChanged := map[string]bool{}
	ses := s.sessions[sid]
	if ses == nil {
		ses = s.newSession("guest", "guest")
		s.sessions[sid] = ses
	}
	sess := ses.state
	var ops []durOp // durable writes, replayed in one transaction at the end
	for _, st := range act.Body {
		switch st.Op {
		case "assign":
			v := eval(st.Value, scope)
			if !sameValue(sess[st.Target], v) {
				sess[st.Target] = v
				scope[st.Target] = v
				deltas[st.Target] = v
			}
		case "add":
			row := record{}
			for _, fi := range st.Fields {
				row[fi.Name] = eval(fi.Expr, scope)
			}
			s.nextID[st.Entity]++
			row["id"] = s.nextID[st.Entity]
			s.entities[st.Entity] = append(s.entities[st.Entity], row)
			ops = append(ops, durOp{kind: "save", entity: st.Entity, row: row})
			entChanged[st.Entity] = true
		case "set":
			key := eval(st.Key, scope)
			for _, r := range s.entities[st.Entity] {
				if m, ok := r.(record); ok && equal(m["id"], key) {
					m[st.Field] = eval(st.Value, scope)
					ops = append(ops, durOp{kind: "save", entity: st.Entity, row: m})
					entChanged[st.Entity] = true
					break
				}
			}
		case "remove":
			key := eval(st.Key, scope)
			rows := s.entities[st.Entity]
			for i, r := range rows {
				if m, ok := r.(record); ok && equal(m["id"], key) {
					s.entities[st.Entity] = append(rows[:i], rows[i+1:]...)
					ops = append(ops, durOp{kind: "delete", entity: st.Entity, id: key})
					entChanged[st.Entity] = true
					// the database cascades the delete to children; mirror it in memory.
					s.cascadeMem(st.Entity, map[int]bool{toInt(key): true}, entChanged)
					break
				}
			}
		case "clear":
			removed := map[int]bool{}
			for _, r := range s.entities[st.Entity] {
				if m, ok := r.(record); ok {
					removed[toInt(m["id"])] = true
				}
			}
			s.entities[st.Entity] = []any{}
			ops = append(ops, durOp{kind: "clear", entity: st.Entity})
			entChanged[st.Entity] = true
			s.cascadeMem(st.Entity, removed, entChanged)
		}
		// keep entity collections in scope fresh for later statements.
		for ent := range entChanged {
			scope[ent] = s.entities[ent]
		}
	}

	// Persist the whole action atomically: every write rides one transaction, so
	// the database never holds a half-applied action. The in-memory working set is
	// already updated and stays live even if the commit fails — durability is
	// best-effort and surfaced loudly, the app does not stall on it.
	s.commit(ops)

	// Shared (entity) changes fan out to every live client over SSE — including
	// this one — so all tabs converge with no refresh. Per-session scalar deltas
	// stay private and ride back on this response.
	if len(entChanged) > 0 {
		entDeltas := map[string]any{}
		for ent := range entChanged {
			entDeltas[ent] = s.entities[ent]
		}
		s.broadcast(entDeltas)
	}
	s.recordAudit(actor, act.Name, true, "")
	s.obs.metrics.observeAction(act.Name, "ok")
	return deltas, http.StatusOK, ""
}

// policyPasses evaluates one permission check against the action scope. A
// zero-parameter policy is read directly; a row-level policy has its parameters
// bound from the call-site arguments first.
func (s *Server) policyPasses(req ir.Require, scope map[string]any) bool {
	pol := s.byPolicy[req.Name]
	if pol == nil {
		return false
	}
	if len(pol.Params) == 0 {
		return truthy(eval(pol.Expr, scope))
	}
	ps := cloneScope(scope)
	for i, p := range pol.Params {
		var v any
		if i < len(req.Args) {
			v = eval(req.Args[i], scope)
		}
		ps[p.Name] = coerce(v, p.Type)
	}
	return truthy(eval(pol.Expr, ps))
}

// durOp is one pending durable write, captured as an action runs and replayed
// against a transaction when it finishes.
type durOp struct {
	kind   string // save | delete | clear
	entity string
	row    record // save
	id     any    // delete
}

// commit replays an action's durable writes in a single transaction. A failure
// rolls the whole batch back (the database stays consistent) and is logged; the
// in-memory working set — already updated — keeps the app live.
func (s *Server) commit(ops []durOp) {
	if len(ops) == 0 {
		return
	}
	tx, err := s.store.Begin()
	if err != nil {
		s.persist(fmt.Errorf("begin transaction: %w", err))
		return
	}
	for _, op := range ops {
		switch op.kind {
		case "save":
			err = tx.Save(op.entity, op.row)
		case "delete":
			err = tx.Delete(op.entity, op.id)
		case "clear":
			err = tx.Clear(op.entity)
		}
		if err != nil {
			tx.Rollback()
			s.persist(fmt.Errorf("action write rolled back: %w", err))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.persist(fmt.Errorf("commit failed: %w", err))
	}
}

// cascadeMem removes, from the in-memory working set, the rows that reference a
// just-removed set of parent ids — the same cascade the database performs via ON
// DELETE CASCADE — recursing through the reverse-relation graph and marking every
// affected entity changed so the deletions fan out to live clients.
func (s *Server) cascadeMem(parent string, removedIDs map[int]bool, entChanged map[string]bool) {
	for _, ch := range s.children[parent] {
		rows := s.entities[ch.Entity]
		kept := rows[:0:0]
		childRemoved := map[int]bool{}
		for _, r := range rows {
			m, ok := r.(record)
			if ok && removedIDs[toInt(m[ch.Field])] {
				childRemoved[toInt(m["id"])] = true
				continue
			}
			kept = append(kept, r)
		}
		if len(childRemoved) > 0 {
			s.entities[ch.Entity] = kept
			entChanged[ch.Entity] = true
			s.cascadeMem(ch.Entity, childRemoved, entChanged)
		}
	}
}

// persist logs a Store write failure without aborting the request: the in-memory
// working set is already updated, so the app stays live; the durable copy is
// best-effort and surfaced loudly for operators.
func (s *Server) persist(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet: store write failed: %v\n", err)
	}
}

// scope builds the evaluation scope for a session: durable entities + per-session
// server state + the session identity (actor/role/verified). Client states are
// not here (the authority cannot see them; the compiler guarantees server
// actions never read them).
func (s *Server) scope(sid string) map[string]any {
	ses := s.sessions[sid]
	actor, role, verified := "guest", "guest", false
	if ses != nil {
		actor, role, verified = ses.actor, ses.role, ses.verified
	}
	scope := map[string]any{"actor": actor, "role": role, "verified": verified}
	// Phase 6: the session's active tenant and the actor's role within it, exposed
	// to the graph like `actor`/`role` so policies can scope rows by `tenant`.
	tid := activeTenant(ses)
	scope["tenant"] = tid
	scope["tenantRole"] = s.tenantRoleFor(actor, tid)
	for ent, rows := range s.entities {
		if isReservedEntity(ent) {
			continue // runtime-managed tables never enter a render scope or reach a client
		}
		scope[ent] = rows
	}
	if ses != nil {
		for k, v := range ses.state {
			// `__`-prefixed keys are runtime-internal session state (e.g. the active
			// tenant); they persist with the session but never enter the eval scope.
			if strings.HasPrefix(k, "__") {
				continue
			}
			scope[k] = v
		}
	}
	return scope
}

// fullStore is the scope plus client-state defaults — the complete snapshot the
// client renders its first frame from.
func (s *Server) fullStore(sid string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	store := s.scope(sid)
	for _, st := range s.ir.States {
		if st.Placement == ir.Client {
			store[st.Name] = eval(st.Init, map[string]any{})
		}
	}
	return store
}

// ── JSON API projection ───────────────────────────────────────────────────────
//
// The same application graph the web page renders is also a machine-facing API,
// with no extra application code — a second projection of one IR. `GET /api`
// describes the graph; `GET /api/<Entity>` lists durable rows; `POST
// /api/<action>` invokes a server action (policies enforced identically to the
// web channel). A native or mobile client reads exactly this.

// handleAPISchema describes the application: its entities, the invocable server
// actions (with their parameters and required policies), and its derives. This
// is the contract any non-web projection compiles against.
func (s *Server) handleAPISchema(w http.ResponseWriter, r *http.Request) {
	type apiParam struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	type apiAction struct {
		Name     string     `json:"name"`
		Params   []apiParam `json:"params"`
		Requires []string   `json:"requires,omitempty"`
	}
	schema := map[string]any{"app": s.ir.App}
	var ents []ir.Entity
	for _, e := range s.ir.Entities {
		if isReservedEntity(e.Name) {
			continue // never expose the credential store or other runtime-managed tables
		}
		ents = append(ents, e)
	}
	schema["entities"] = ents
	schema["derives"] = s.ir.Derives
	var actions []apiAction
	for _, a := range s.ir.Actions {
		if a.Placement != ir.Server {
			continue // client actions are not callable over the API
		}
		ap := apiAction{Name: a.Name}
		for _, req := range a.Requires {
			ap.Requires = append(ap.Requires, req.Name)
		}
		for _, p := range a.Params {
			ap.Params = append(ap.Params, apiParam{Name: p.Name, Type: p.Type})
		}
		actions = append(actions, ap)
	}
	schema["actions"] = actions
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(schema)
}

// handleAPI serves the two data routes under /api/: a GET on an entity name
// returns its rows; a POST on an action name invokes it.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/")
	switch name {
	case "_audit":
		s.handleAudit(w, r)
		return
	case "_export":
		s.handleGDPRExport(w, r)
		return
	case "_erase":
		s.handleGDPRErase(w, r)
		return
	case "_i18n":
		s.handleI18n(w, r)
		return
	case "_billing":
		s.handleBilling(w, r)
		return
	}
	if name == "" || strings.Contains(name, "/") || isReservedEntity(name) {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// The entity list is a true SQL pushdown: `?by=field&desc=1&limit=20` and
		// `field=value` filters compile to an indexed, paginated SELECT, and the
		// reply carries an opaque `next` cursor for the following page — the table is
		// never loaded whole.
		ent, ok := s.entityByName(name)
		if !ok {
			http.Error(w, "unknown entity", http.StatusNotFound)
			return
		}
		query, err := buildAPIQuery(ent, r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Serve a hot list from the read cache when enabled; it is invalidated the
		// instant any entity changes, so it never returns stale rows.
		cacheKey := name + "?" + r.URL.RawQuery
		if body, ok := s.apiCache.get(cacheKey); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "hit")
			w.Write(body)
			return
		}
		rows, next, err := s.store.Query(query)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		out := map[string]any{"rows": rows}
		if next != "" {
			out["next"] = next
		}
		body, _ := json.Marshal(out)
		s.apiCache.put(cacheKey, body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)

	case http.MethodPost:
		if !s.guardMutation(w, r, false) {
			return
		}
		var req struct {
			Args []any `json:"args"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
		if s.ir.Auth && isAuthAction(name) {
			s.runAuth(w, r, name, req.Args)
			return
		}
		if s.runReserved(w, r, name, req.Args) {
			return
		}
		act := s.byAction[name]
		if act == nil {
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}
		if act.Placement != ir.Server {
			http.Error(w, "action is client-placed and not callable over the API", http.StatusBadRequest)
			return
		}
		sid := s.session(w, r)
		deltas, status, msg := s.runAction(sid, act, req.Args)
		if status != http.StatusOK {
			http.Error(w, msg, status)
			return
		}
		if len(deltas) > 0 {
			s.persistSession(sid)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "deltas": deltas})

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// atoiPositive parses a strictly-positive integer.
func atoiPositive(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("not a positive integer")
	}
	return n, nil
}

// entityByName looks up a declared entity.
func (s *Server) entityByName(name string) (ir.Entity, bool) {
	for _, e := range s.ir.Entities {
		if e.Name == name {
			return e, true
		}
	}
	return ir.Entity{}, false
}

// buildAPIQuery turns an entity-list request's query string into a pushed-down
// Query: `by`/`desc`/`limit`/`after` set the order, direction, page size, and
// cursor; every other parameter that names a field becomes an equality filter,
// AND-ed together. Unknown fields are a 400 rather than a silently ignored
// filter, so a typo can never widen a result set.
func buildAPIQuery(e ir.Entity, vals url.Values) (Query, error) {
	query := Query{Entity: e.Name, ItemVar: "r", After: vals.Get("after")}
	if by := vals.Get("by"); by != "" {
		if _, ok := fieldOf(e, by); !ok {
			return Query{}, fmt.Errorf("unknown order field %q", by)
		}
		query.Order = by
	}
	if d := vals.Get("desc"); d == "1" || d == "true" {
		query.Desc = true
	}
	if l := vals.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			return Query{}, fmt.Errorf("limit must be a positive integer")
		}
		query.Limit = n
	}

	reserved := map[string]bool{"by": true, "desc": true, "limit": true, "after": true}
	var preds []*ir.Expr
	for key, vs := range vals {
		if reserved[key] || len(vs) == 0 {
			continue
		}
		f, ok := fieldOf(e, key)
		if !ok {
			return Query{}, fmt.Errorf("unknown filter field %q", key)
		}
		preds = append(preds, &ir.Expr{Kind: "bin", Op: "==",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "r"}, Field: key},
			R: litFor(f, vs[0])})
	}
	if len(preds) > 0 {
		pred := preds[0]
		for _, p := range preds[1:] {
			pred = &ir.Expr{Kind: "bin", Op: "&&", L: pred, R: p}
		}
		query.Where = pred
	}
	return query, nil
}

// fieldOf resolves a field by name; id is an implicit int field every entity has.
func fieldOf(e ir.Entity, name string) (ir.Field, bool) {
	if name == "id" {
		return ir.Field{Name: "id", Type: "int"}, true
	}
	for _, f := range e.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return ir.Field{}, false
}

// litFor builds a typed literal for a filter value, matching the field's type so
// the pushed-down comparison binds the right SQL parameter type.
func litFor(f ir.Field, s string) *ir.Expr {
	switch {
	case f.IsRelation() || f.Type == "int":
		n, _ := strconv.Atoi(s)
		return &ir.Expr{Kind: "lit", Val: n, VType: "int"}
	case f.Type == "bool":
		return &ir.Expr{Kind: "lit", Val: s == "true" || s == "1", VType: "bool"}
	default:
		return &ir.Expr{Kind: "lit", Val: s, VType: "text"}
	}
}

// ── server-side rendering (first paint) ──────────────────────────────────────

func (s *Server) renderNode(b *strings.Builder, n ir.Node, scope map[string]any) {
	switch n.Kind {
	case "box":
		b.WriteString(`<div class="fa-box">`)
		for _, c := range n.Children {
			s.renderNode(b, c, scope)
		}
		b.WriteString(`</div>`)
	case "row":
		b.WriteString(`<div class="fa-row">`)
		for _, c := range n.Children {
			s.renderNode(b, c, scope)
		}
		b.WriteString(`</div>`)
	case "text":
		b.WriteString(`<span class="fa-text">`)
		for _, seg := range n.Segs {
			switch {
			case seg.Lit != "":
				b.WriteString(html.EscapeString(seg.Lit))
			case seg.Bind != "":
				val := toStr(eval(s.byBind[seg.Bind].Expr, scope))
				fmt.Fprintf(b, `<span data-fa-bind="%s">%s</span>`, seg.Bind, html.EscapeString(val))
			case seg.Expr != nil:
				b.WriteString(html.EscapeString(toStr(eval(seg.Expr, scope))))
			}
		}
		b.WriteString(`</span>`)
	case "button":
		fmt.Fprintf(b, `<button data-fa-action="%s">%s</button>`,
			html.EscapeString(n.Action), html.EscapeString(n.Label))
	case "list":
		if n.ID != "" {
			fmt.Fprintf(b, `<div data-fa-region="%s">`, n.ID)
		} else {
			b.WriteString(`<div>`)
		}
		rows, _ := scope[n.Coll].([]any)
		for _, r := range selectRows(rows, n, scope) {
			child := cloneScope(scope)
			child[n.Var] = r
			for _, c := range n.Children {
				s.renderNode(b, c, child)
			}
		}
		b.WriteString(`</div>`)
	case "if":
		if n.ID != "" {
			fmt.Fprintf(b, `<div data-fa-region="%s">`, n.ID)
		} else {
			b.WriteString(`<div>`)
		}
		if truthy(eval(n.Cond, scope)) {
			for _, c := range n.Children {
				s.renderNode(b, c, scope)
			}
		}
		b.WriteString(`</div>`)
	case "input":
		val := html.EscapeString(toStr(scope[n.Bind]))
		fmt.Fprintf(b, `<input data-fa-input="%s" value="%s" placeholder="%s">`,
			n.Bind, val, html.EscapeString(n.Placeholder))
	case "link":
		fmt.Fprintf(b, `<a class="fa-link" href="%s">%s</a>`,
			html.EscapeString(n.Path), html.EscapeString(n.Label))
	case "select":
		fmt.Fprintf(b, `<select class="fa-select" data-fa-input="%s">`, n.Bind)
		cur := toStr(scope[n.Bind])
		for _, o := range n.Options {
			sel := ""
			if o.Value == cur {
				sel = " selected"
			}
			fmt.Fprintf(b, `<option value="%s"%s>%s</option>`,
				html.EscapeString(o.Value), sel, html.EscapeString(o.Label))
		}
		b.WriteString(`</select>`)
	case "form":
		fmt.Fprintf(b, `<form class="fa-form" data-fa-form="%s">`, html.EscapeString(n.Action))
		for _, c := range n.Children {
			s.renderNode(b, c, scope)
		}
		fmt.Fprintf(b, `<button type="submit">%s</button></form>`, html.EscapeString(n.Label))
	case "upload":
		fmt.Fprintf(b, `<label class="fa-upload">%s<input type="file" data-fa-upload="%s"></label>`,
			html.EscapeString(n.Label), n.Bind)
	case "use":
		comp := s.byComponent[n.Name]
		if comp == nil {
			return
		}
		if n.ID != "" {
			fmt.Fprintf(b, `<div data-fa-region="%s">`, n.ID)
		} else {
			b.WriteString(`<div class="fa-use">`)
		}
		child := cloneScope(scope)
		for i, p := range comp.Params {
			var v any
			if i < len(n.Args) {
				v = eval(n.Args[i], scope)
			}
			child[p.Name] = coerce(v, p.Type)
		}
		for _, c := range comp.View {
			s.renderNode(b, c, child)
		}
		b.WriteString(`</div>`)
	}
}

// ── sessions + persistence ───────────────────────────────────────────────────

// sessionTTL is how long a session stays valid without activity; each request
// slides it forward (a refresh), so an active user is never logged out.
const sessionTTL = 24 * time.Hour

// session resolves (or creates) the caller's session from a signed cookie. A
// tampered or expired cookie is rejected and a fresh guest session is minted.
// Every call slides the expiry forward and re-stamps the cookie (sliding
// refresh), and the cookie carries HttpOnly/SameSite/Secure hardening flags.
func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	now := time.Now()
	if c, err := r.Cookie("fa_sid"); err == nil {
		// The cookie is "sid.signature"; reject anything we did not sign.
		if sid, ok := verifySigned(c.Value); ok {
			s.mu.Lock()
			ses, live := s.sessions[sid]
			// Stateless servers: a request can land on a cold instance, so rehydrate
			// the session from the shared store on a local cache miss.
			if !live && s.cluster != nil {
				if ps, found, err := s.store.LoadSession(sid); err == nil && found {
					ses = sessionFromPersisted(ps)
					s.sessions[sid] = ses
					live = true
				}
			}
			if live && now.Before(ses.expires) {
				ses.expires = now.Add(sessionTTL) // slide the window forward
				s.mu.Unlock()
				s.persistSession(sid)
				s.setSessionCookie(w, sid)
				return sid
			}
			if live {
				delete(s.sessions, sid) // expired — drop it
				s.mu.Unlock()
				s.dropSharedSession(sid)
				s.mu.Lock()
			}
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	s.nextSID++
	sid := fmt.Sprintf("s%d", s.nextSID)
	if s.cluster != nil {
		// A process-local counter is not unique across instances; mint a globally
		// unique id so two instances never collide on a session key.
		sid = "s-" + s.cluster.instanceID + "-" + strconv.Itoa(s.nextSID)
	}
	ses := s.newSession("guest", "guest")
	// Without auth, identity comes from `?as=` (a dev convenience). With auth, the
	// session starts as an anonymous guest until login.
	if !s.ir.Auth {
		if as := r.URL.Query().Get("as"); as != "" {
			ses.actor = as
		}
	}
	s.sessions[sid] = ses
	s.obs.metrics.setSessions(int64(len(s.sessions)))
	s.mu.Unlock()
	s.persistSession(sid)
	s.setSessionCookie(w, sid)
	return sid
}

// persistSession writes a session through to the shared store (no-op unless
// clustering is on), so any instance can serve the user's next request.
func (s *Server) persistSession(sid string) {
	if s.cluster == nil {
		return
	}
	s.mu.Lock()
	ses := s.sessions[sid]
	if ses == nil {
		s.mu.Unlock()
		return
	}
	ps := persistedFromSession(ses)
	s.mu.Unlock()
	if err := s.store.SaveSession(sid, ps); err != nil {
		s.obs.log.Warn("save session", slog_err(err))
	}
}

// dropSharedSession removes a session from the shared store (logout/expiry).
func (s *Server) dropSharedSession(sid string) {
	if s.cluster == nil {
		return
	}
	if err := s.store.DeleteSession(sid); err != nil {
		s.obs.log.Warn("delete session", slog_err(err))
	}
}

// persistedFromSession snapshots a live session for the shared store.
func persistedFromSession(ses *sessionState) *persistedSession {
	state := map[string]any{}
	for k, v := range ses.state {
		state[k] = v
	}
	return &persistedSession{
		Actor: ses.actor, Role: ses.role, Verified: ses.verified,
		PendingMFA: ses.pendingMFA, State: state, Expires: ses.expires,
	}
}

// sessionFromPersisted rebuilds a live session from a shared-store row.
func sessionFromPersisted(ps *persistedSession) *sessionState {
	state := ps.State
	if state == nil {
		state = map[string]any{}
	}
	return &sessionState{
		state: state, actor: ps.Actor, role: ps.Role, verified: ps.Verified,
		pendingMFA: ps.PendingMFA, expires: ps.Expires,
	}
}

// setSessionCookie writes the signed, hardened session cookie.
func (s *Server) setSessionCookie(w http.ResponseWriter, sid string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "fa_sid",
		Value:    signValue(sid),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// newSession is a fresh session with the given identity and the server-placed
// state cells at their declared defaults.
func (s *Server) newSession(actor, role string) *sessionState {
	store := map[string]any{}
	for _, st := range s.ir.States {
		if st.Placement == ir.Server {
			store[st.Name] = eval(st.Init, map[string]any{})
		}
	}
	return &sessionState{state: store, actor: actor, role: role, expires: time.Now().Add(sessionTTL)}
}

// ── value helpers ────────────────────────────────────────────────────────────

// selectRows applies a list node's query to its rows: filter by the `where`
// predicate, order by `by`, then cap by `limit` — the same pipeline, in the same
// order, as the client (assets/facet.js), so server first paint and client
// re-renders agree exactly. scope supplies outer names; the item var is bound per
// row for the filter.
func selectRows(rows []any, n ir.Node, scope map[string]any) []any {
	filtered := rows
	if n.Where != nil {
		filtered = filtered[:0:0]
		for _, r := range rows {
			child := cloneScope(scope)
			child[n.Var] = r
			if truthy(eval(n.Where, child)) {
				filtered = append(filtered, r)
			}
		}
	}
	out := sortRows(filtered, n.Order, n.Desc)
	if n.Limit > 0 && len(out) > n.Limit {
		out = out[:n.Limit]
	}
	return out
}

// sortRows returns a copy of rows ordered by field (ascending, or descending if
// desc). Identical comparator to the client (assets/facet.js) so server and
// client agree on order. order == "" leaves insertion order untouched.
func sortRows(rows []any, order string, desc bool) []any {
	out := append([]any{}, rows...)
	if order == "" {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool {
		a := out[i].(record)[order]
		b := out[j].(record)[order]
		less := lessVal(a, b)
		if desc {
			return lessVal(b, a)
		}
		return less
	})
	return out
}

// lessVal compares two field values: numeric if both are numbers, else string.
func lessVal(a, b any) bool {
	_, aNum := numeric(a)
	_, bNum := numeric(b)
	if aNum && bNum {
		return toInt(a) < toInt(b)
	}
	return toStr(a) < toStr(b)
}

func numeric(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	}
	return 0, false
}

func cloneScope(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func coerce(v any, typ string) any {
	switch typ {
	case "int", "money", "date":
		return toInt(v)
	case "bool":
		return truthy(v)
	case "text":
		return toStr(v)
	default:
		// an enum (already text) or an entity-typed value (a row record) — pass it
		// through untouched rather than stringifying a structured value.
		return v
	}
}

func zero(typ string) any {
	switch typ {
	case "int", "money", "date":
		return 0
	case "bool":
		return false
	default:
		return ""
	}
}

func sameValue(a, b any) bool {
	return toStr(a) == toStr(b) && fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b)
}

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Facet</title>
<meta name="fa-csrf" content="%s">
<style>
  :root {
    --fa-bg: #fff; --fa-fg: #111; --fa-muted: #666; --fa-accent: #1a56db;
    --fa-border: #888; --fa-card-border: #e3e3e3; --fa-radius: 8px;
    --fa-font: 16px/1.5 system-ui, sans-serif; --fa-maxwidth: 34rem;
  }
  body { font: var(--fa-font); margin: 2.5rem auto; max-width: var(--fa-maxwidth);
         color: var(--fa-fg); background: var(--fa-bg); padding: 0 1rem; }
  .fa-box { display: flex; flex-direction: column; gap: .5rem; align-items: stretch; }
  .fa-box .fa-box { border: 1px solid var(--fa-card-border); border-radius: calc(var(--fa-radius) + 2px); padding: .6rem .8rem; }
  /* A row lays its children out horizontally and wraps; on a narrow viewport it
     collapses to a vertical stack, so multi-column layouts reflow with the window. */
  .fa-row { display: flex; flex-direction: row; gap: 1.25rem; align-items: flex-start; flex-wrap: wrap; }
  .fa-row > * { flex: 1 1 0; min-width: 0; }
  /* A box directly inside a row is a structural column (a wireframe region), not a
     card — it stays transparent and borderless so the layers composite into one
     surface rather than reading as stacked panels. */
  .fa-row > .fa-box { border: none; background: transparent; padding: 0; }
  @media (max-width: 720px) { .fa-row { flex-direction: column; } .fa-row > * { width: 100%%; } }
  .fa-text { font-variant-numeric: tabular-nums; }
  .fa-form { display: flex; flex-direction: column; gap: .5rem; align-items: stretch; }
  .fa-use { display: contents; }
  button { font: inherit; padding: .4rem .8rem; border: 1px solid var(--fa-border); border-radius: var(--fa-radius);
           background: var(--fa-bg); color: var(--fa-fg); cursor: pointer; align-self: flex-start; }
  button[type=submit] { background: var(--fa-accent); color: #fff; border-color: var(--fa-accent); }
  button:active { transform: translateY(1px); }
  button:focus-visible, input:focus-visible, select:focus-visible, a:focus-visible {
    outline: 2px solid var(--fa-accent); outline-offset: 2px; }
  input, select { font: inherit; padding: .4rem .6rem; border: 1px solid var(--fa-border);
                  border-radius: var(--fa-radius); background: var(--fa-bg); color: var(--fa-fg); }
  .fa-upload { display: inline-flex; gap: .4rem; align-items: center; cursor: pointer; }
  .fa-link { color: var(--fa-accent); }
  .fa-error { color: #b00020; font-size: .9em; }
  [data-fa-bind] { font-weight: 600; }
  [aria-busy=true] { opacity: .6; }
</style>
<style id="fa-theme">%s</style>
</head>
<body>
<div id="fa-root" data-fa-mount>%s</div>
<script type="application/json" id="fa-ir">%s</script>
<script type="application/json" id="fa-state">%s</script>
<script src="/facet.js"></script>
%s</body>
</html>
`

// guardedPage is the minimal response when a route guard denies the actor.
const guardedPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>%s — Facet</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:4rem auto;max-width:30rem;color:#111;padding:0 1rem}</style>
</head><body><h1>Not available</h1><p>You don't have access to this page.</p><p><a href="/">Home</a></p></body></html>
`
