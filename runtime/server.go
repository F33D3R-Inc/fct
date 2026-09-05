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
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"facet/internal/ir"
)

//go:embed assets/facet.js
var clientJS []byte

// Server serves one compiled app.
type Server struct {
	ir          *ir.IR
	byAction    map[string]*ir.Action
	byPolicy    map[string]*ir.Policy
	byComponent map[string]*ir.Component
	byService   map[string]*ir.Service
	byRecord    map[string]*ir.Record   // record name -> its field schema, for decoding a structured service reply
	triggers    map[string][]ir.Trigger // source action name -> reactions to run on its success
	gated       map[string][]gatedField // entity -> @requires-gated fields (API per-actor, never SSE)
	apiRead     map[string]entityRead   // entity -> its JSON-API read rule; ABSENT MEANS REFUSED (see apiread.go)
	privateNm   map[string]bool         // @private state names — never shipped to a client
	uploadDir   string                  // directory uploaded files are written to and served from

	uploadMu       sync.Mutex                // guards uploadSessions
	uploadSessions map[string]*uploadSession // in-flight resumable uploads, keyed by session id

	idemMu sync.Mutex             // guards idem
	idem   map[string]*idemRecord // webhook idempotency: dedup key -> the once-processed outcome (or an in-flight marker)

	fieldRE map[string]*regexp.Regexp // compiled @matches patterns, keyed by entity.field
	softDel map[string]bool           // entity names that soft-delete (archive) on remove

	store    Store
	children map[string][]ir.Reference // entity -> the relations that point at it (for cascade)
	secure   bool                      // mark cookies Secure (TLS / FACET_SECURE_COOKIES=1)

	mu       sync.Mutex
	entities map[string][]any         // durable, shared (the in-memory working set; the Store is the source of truth)
	nextID   map[string]int           // per-entity id counter
	sessions map[string]*sessionState // sid -> session (per-session scalar state + identity + expiry)

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

	// streamEnts is the set of entities the live stream may carry as rows — a
	// pure function of the IR (see streamEntities), computed once.
	streamOnce sync.Once
	streamEnts map[string]*collRead

	// aggIdx numbers each page's client-visible aggregates so a render can ship
	// their values (see runtime/region.go). A pure function of the page, so it is
	// computed on first render of that route and kept.
	aggMu  sync.Mutex
	aggIdx map[string]map[*ir.Expr]int

	// writeSeq counts entity changes. A page carries the value it was rendered at
	// and the live stream carries the current one, so a client can tell that the
	// authority has written since its page was built — the race the old
	// whole-database SSE snapshot used to paper over.
	writeSeq atomic.Int64
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
		byPolicy:    map[string]*ir.Policy{},
		byComponent: map[string]*ir.Component{},
		byService:   map[string]*ir.Service{},
		byRecord:    map[string]*ir.Record{},
		triggers:    map[string][]ir.Trigger{},
		gated:       map[string][]gatedField{},
		privateNm:   map[string]bool{},
		uploadDir:   uploadDirFromEnv(),

		uploadSessions: map[string]*uploadSession{},
		idem:           map[string]*idemRecord{},
		fieldRE:        map[string]*regexp.Regexp{},
		softDel:        map[string]bool{},
		entities:       map[string][]any{},
		nextID:         map[string]int{},
		sessions:       map[string]*sessionState{},
		subs:           map[chan []byte]bool{},
		secure:         os.Getenv("FACET_SECURE_COOKIES") == "1",
		limiter:        newRateLimiter(rateLimitFromEnv()),
		lockout:        newLockout(),
		obs:            newObs(),
		apiCache:       newAPICache(),
		i18n:           newI18n(),
	}
	for i := range graph.Actions {
		s.byAction[graph.Actions[i].Name] = &graph.Actions[i]
	}
	for _, st := range graph.States {
		if st.Private {
			s.privateNm[st.Name] = true // server-only: stripped from every client payload
		}
	}
	for i := range graph.Policies {
		s.byPolicy[graph.Policies[i].Name] = &graph.Policies[i]
	}
	for i := range graph.Services {
		s.byService[graph.Services[i].Name] = &graph.Services[i]
	}
	for i := range graph.Records {
		s.byRecord[graph.Records[i].Name] = &graph.Records[i]
	}
	// Precompile @matches patterns (full-match anchored) so add/set validation is a
	// cheap lookup. An unparsable pattern is skipped (the field just isn't pattern-
	// constrained) rather than crashing the server.
	for _, ent := range graph.Entities {
		if ent.SoftDelete {
			s.softDel[ent.Name] = true
		}
		for _, f := range ent.Fields {
			if f.Matches == "" {
				continue
			}
			if re, err := regexp.Compile("^(?:" + f.Matches + ")$"); err == nil {
				s.fieldRE[ent.Name+"."+f.Name] = re
			}
		}
	}
	// Index event triggers by their source action, so a successful action can fan
	// out to its reactions in O(1). The compiler proved this graph acyclic.
	for _, tr := range graph.Triggers {
		s.triggers[tr.On] = append(s.triggers[tr.On], tr)
	}
	s.indexGatedFields() // entity fields gated by @requires, for projection-time stripping
	// Which entities the JSON API may list, and to whom. The map is the whole
	// permission: an entity missing from it is refused, so an app that declares
	// nothing publishes nothing (runtime/apiread.go).
	rules, problems := apiReadFromEnv(apiReadEnvValue(), graph.Entities, s.byPolicy)
	s.apiRead = rules
	for _, p := range problems {
		s.obs.log.Warn(p)
	}
	for i := range graph.Components {
		s.byComponent[graph.Components[i].Name] = &graph.Components[i]
	}
	// The reverse-relation graph: parent entity -> the relations that point at it.
	// It is ir.References read backwards — the one derivation of that graph, which
	// is also what the store declares to its engine as the cascade rule, so the
	// working set cannot come to disagree with the database about which rows a
	// delete takes with it.
	s.children = map[string][]ir.Reference{}
	for _, r := range ir.References(graph.Entities) {
		s.children[r.Parent] = append(s.children[r.Parent], r)
	}
	// A view whose route a built-in endpoint claims is dead code the author cannot
	// see: it compiles, `facet routes` prints it, and the server serves something
	// else there. Say so now, while someone is watching the log.
	s.warnShadowedRoutes()
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
		// nextID tracks the high-water id across ALL rows (archived included) so a new
		// row never reuses a soft-deleted row's id; the live set excludes archived rows.
		max := 0
		for _, r := range rows {
			if m, ok := r.(record); ok {
				if id := toInt(m["id"]); id > max {
					max = id
				}
			}
		}
		s.nextID[e.Name] = max
		s.entities[e.Name] = liveRows(rows, e.SoftDelete)
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

// ── the built-in endpoints, as data ─────────────────────────────────────────
//
// Every one of these is registered ahead of the app's view router ("/"), so a
// request whose path one of them claims NEVER reaches a view. An app declaring
// `view Admin at "/admin"` therefore got the generated console instead of its own
// page — `facet routes` printed the view, the server served the built-in, and
// nothing anywhere said the two were the same path.
//
// The list is a table rather than a run of mux.HandleFunc calls for one reason:
// it is the only way "which paths are built in" can have a single answer. Handler
// builds the mux out of it, ShadowedRoutes reports against it, and `facet routes`
// / `facet doctor` read it through BuiltinRoutes() — so a built-in added later is
// added once and every consumer sees it. A second hand-written list somewhere in
// cmd/ would drift the first time an endpoint moved, and the drift would show up
// as this same bug wearing a different path.
//
// A pattern ending in "/" is a subtree (Go's ServeMux), so it claims every path
// beneath it; any other pattern claims exactly itself.
type builtinRoute struct {
	BuiltinRoute
	// handler builds this endpoint's handler for a server, or returns nil when the
	// endpoint is not enabled in this configuration (dev reload, OIDC). The
	// condition lives here so there is nowhere else to state it.
	handler func(*Server) http.HandlerFunc
}

// BuiltinRoute describes one endpoint the runtime serves itself. It is exported
// so `facet routes` and `facet doctor` can report against the same list the
// server actually registers.
type BuiltinRoute struct {
	// Pattern is the ServeMux pattern. A trailing "/" makes it a subtree.
	Pattern string `json:"pattern"`
	// What names the endpoint in a diagnostic ("the generated admin console").
	What string `json:"what"`
	// Always is false for an endpoint only some configurations register — it is
	// still reported, because a route that is shadowed only in development or only
	// when OIDC is on is a route that breaks when someone turns that on.
	Always bool `json:"always"`
}

var builtins = []builtinRoute{
	{BuiltinRoute{"/facet.js", "the client runtime script", true}, func(s *Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			w.Write(clientJS)
		}
	}},
	{BuiltinRoute{"/event", "the action endpoint", true}, func(s *Server) http.HandlerFunc { return s.handleEvent }},
	{BuiltinRoute{"/live", "the live change stream", true}, func(s *Server) http.HandlerFunc { return s.handleLive }},
	{BuiltinRoute{"/region", "the region data endpoint", true}, func(s *Server) http.HandlerFunc { return s.handleRegion }},
	{BuiltinRoute{"/api", "the JSON API schema", true}, func(s *Server) http.HandlerFunc { return s.handleAPISchema }},
	{BuiltinRoute{"/api/", "the JSON API", true}, func(s *Server) http.HandlerFunc { return s.handleAPI }},
	{BuiltinRoute{"/upload", "the file upload endpoint", true}, func(s *Server) http.HandlerFunc { return s.handleUpload }},
	// resumable: init/chunk/finish/abort
	{BuiltinRoute{"/upload/", "the resumable upload endpoints", true}, func(s *Server) http.HandlerFunc { return s.handleUploadChunked }},
	{BuiltinRoute{"/uploads/", "stored media", true}, func(s *Server) http.HandlerFunc { return s.handleUploads }},
	// Phase 6: the generated admin dashboard and the billing provider webhook.
	{BuiltinRoute{"/admin", "the generated admin console", true}, func(s *Server) http.HandlerFunc { return s.handleAdmin }},
	{BuiltinRoute{"/admin/", "the generated admin console", true}, func(s *Server) http.HandlerFunc { return s.handleAdmin }},
	{BuiltinRoute{"/billing/webhook", "the billing provider webhook", true}, func(s *Server) http.HandlerFunc { return s.handleBillingWebhook }},
	{BuiltinRoute{"/dev/reload", "the dev-server reload endpoint", false}, func(s *Server) http.HandlerFunc {
		if s.dev == nil {
			return nil
		}
		return s.handleDevReload
	}},
	{BuiltinRoute{"/auth/oidc/login", "OIDC single sign-on", false}, func(s *Server) http.HandlerFunc {
		if s.oidc == nil {
			return nil
		}
		return s.handleOIDCLogin
	}},
	{BuiltinRoute{"/auth/oidc/callback", "OIDC single sign-on", false}, func(s *Server) http.HandlerFunc {
		if s.oidc == nil {
			return nil
		}
		return s.handleOIDCCallback
	}},
	// Operations endpoints: liveness/readiness probes for orchestrators and the
	// Prometheus scrape target. They sit outside the app's routes.
	{BuiltinRoute{"/healthz", "the liveness probe", true}, func(s *Server) http.HandlerFunc { return s.handleHealthz }},
	{BuiltinRoute{"/readyz", "the readiness probe", true}, func(s *Server) http.HandlerFunc { return s.handleReadyz }},
	{BuiltinRoute{"/metrics", "the Prometheus scrape target", true}, func(s *Server) http.HandlerFunc { return s.handleMetrics }},
}

// BuiltinRoutes is the list of endpoints the runtime registers ahead of the app's
// view router. It is a copy, so a caller cannot edit the runtime's routing table
// by editing what it was handed.
func BuiltinRoutes() []BuiltinRoute {
	out := make([]BuiltinRoute, len(builtins))
	for i, b := range builtins {
		out[i] = b.BuiltinRoute
	}
	return out
}

// Handler returns the HTTP routes for the app.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, b := range builtins {
		if h := b.handler(s); h != nil {
			mux.HandleFunc(b.Pattern, h)
		}
	}
	// User-declared inbound webhooks: each authenticates with its own HMAC secret
	// and runs the named action with system authority. The compiler guarantees the
	// paths are unique and never collide with a route registered above.
	for i := range s.ir.Webhooks {
		mux.HandleFunc(s.ir.Webhooks[i].Path, s.webhookHandler(s.ir.Webhooks[i]))
	}
	// The app's view router, last and least specific: it receives every path no
	// built-in above claimed. shadowedRoutes reports the ones it will never see.
	mux.HandleFunc("/", s.handlePage)
	// Wrap the whole mux in the observability middleware: every request becomes a
	// span, a structured access log line, and a metrics sample.
	return s.obs.observe(mux)
}

// RouteShadow is one of an app's routes together with the built-in endpoint that
// claims it. The route is declared, printed by `facet routes`, and dead.
type RouteShadow struct {
	Route   string       `json:"route"`          // the route as the app declares it
	View    string       `json:"view,omitempty"` // the view at that route, when the IR names one
	Builtin BuiltinRoute `json:"builtin"`
}

// Message is the one sentence that describes this shadow, so the boot notice,
// `facet routes` and `facet doctor` all say the same thing about it.
func (rs RouteShadow) Message() string {
	what := "route " + rs.Route
	if rs.View != "" {
		what = "view " + rs.View + " at " + rs.Route
	}
	sometimes := ""
	if !rs.Builtin.Always {
		sometimes = " whenever that is enabled,"
	}
	return fmt.Sprintf("%s is never reached: the runtime serves %s (%s) at that path,%s "+
		"and built-in endpoints are registered ahead of the app's view router. Move the view to a "+
		"path no built-in claims (`facet routes` lists them).",
		what, rs.Builtin.What, rs.Builtin.Pattern, sometimes)
}

// ShadowedRoutes is every route of an app that a built-in endpoint claims, so the
// author is told about a page that cannot be reached rather than discovering it
// by opening it. `cmd/` calls this for `facet routes` and `facet doctor`; the
// runtime calls it at boot (see newServer).
//
// A route with a `:param` segment is matched by the literal prefix in front of
// that segment, because a subtree pattern claims everything beneath it however
// the tail is spelled. A route that is dynamic from its first segment ("/:slug")
// has a literal prefix of "/" alone, which is shorter than every built-in pattern
// and so matches none — correctly: such a route is reachable, just not for the
// handful of paths the built-ins hold.
func ShadowedRoutes(graph *ir.IR) []RouteShadow {
	viewAt := map[string]string{}
	for i := range graph.Pages {
		viewAt[graph.Pages[i].Path] = graph.Pages[i].Name
	}
	var out []RouteShadow
	for _, r := range graph.Routes {
		if b, ok := builtinClaiming(r.Path); ok {
			out = append(out, RouteShadow{Route: r.Path, View: viewAt[r.Path], Builtin: b})
		}
	}
	return out
}

// builtinClaiming reports which built-in endpoint, if any, receives requests for
// a declared route. It is the routing rule ServeMux applies, read against the
// table Handler registers from — never a re-listing of the paths.
func builtinClaiming(route string) (BuiltinRoute, bool) {
	lit := route
	if i := strings.Index(lit, ":"); i >= 0 {
		lit = lit[:i] // ":param" and everything after it is not literal
	}
	var claim BuiltinRoute
	found := false
	for _, b := range builtins {
		hit := false
		if strings.HasSuffix(b.Pattern, "/") {
			hit = strings.HasPrefix(lit, b.Pattern)
		} else {
			hit = route == b.Pattern // an exact pattern claims exactly one path
		}
		// ServeMux gives the request to the longest matching pattern, so the report
		// must name the same one the server will actually run.
		if hit && (!found || len(b.Pattern) > len(claim.Pattern)) {
			claim, found = b.BuiltinRoute, true
		}
	}
	return claim, found
}

// warnShadowedRoutes reports, at boot, every declared route a built-in will
// swallow.
//
// IT WARNS, IT DOES NOT REFUSE, and the line between the two is the same one Init
// draws against `facet migrate`: booting is not a moment anyone asked a question.
// Refusing here would take down an app that has been serving for months the first
// time it restarts — and worse, a built-in added in a later release would turn a
// framework upgrade into an outage for any app that happened to have named that
// path. The failure this fixes is invisibility, not the route itself; the author
// needs to be told, in the place they will see it, on every boot until they move
// it.
//
// The hard failure belongs where an operator did ask: `facet routes` and `facet
// doctor` read the same ShadowedRoutes and can refuse, because nothing is serving
// traffic while they run.
func (s *Server) warnShadowedRoutes() {
	for _, rs := range ShadowedRoutes(s.ir) {
		s.obs.log.Warn(rs.Message())
	}
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

	// Send a current snapshot so a client that connected after a change cannot
	// miss it (closes the page-load → stream-open race). Only the entities a
	// client's own evaluator still reads as collections travel — everything else
	// reaches a client as a region's result set, which the stream cannot know the
	// shape of because it has no page and no actor.
	s.mu.Lock()
	snap := map[string]any{}
	for ent, rows := range s.entities {
		c := s.streamEntities()[ent]
		if isReservedEntity(ent) || c == nil {
			continue // never stream the credential store, runtime tables, or a table nothing reads whole
		}
		snap[ent] = projectRows(rows, c.keepFields(), nil)
	}
	s.mu.Unlock()
	// The hello carries the current write sequence. A client whose page was
	// rendered before a write it did not see compares the two and re-asks; that is
	// what the old snapshot-the-whole-database opening frame was really for.
	safe := s.sseSafe(snap)
	if data, err := json.Marshal(map[string]any{
		"deltas": safe, "media": mediaGrants(safe), "seq": s.writeSeq.Load()}); err == nil {
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
	s.writeSeq.Add(1)
	s.fanout(deltas)
	if s.cluster != nil {
		names := make([]string, 0, len(deltas))
		for ent := range deltas {
			names = append(names, ent)
		}
		s.cluster.publish(names)
	}
}

// fanout pushes an entity change to every live client on this instance only
// (non-blocking; a client whose buffer is full is skipped — it will catch up on
// its next snapshot). The cluster receive path calls this directly to avoid
// re-publishing a peer's change, which is also why the gate is applied *here*
// rather than at each caller: this is the one door onto the shared stream.
//
// Two different things travel through it. An entity the client's own evaluator
// reads as a collection (an aggregate's source) must arrive as rows, because
// nothing else supplies them. Every other entity travels as its *name*: the
// client re-asks the authority for the regions that read it and gets one page
// of rows back, which is why a fifty-thousand-row table no longer crosses this
// wire on every write.
func (s *Server) fanout(deltas map[string]any) {
	if len(deltas) == 0 {
		return
	}
	stream := s.streamEntities()
	rows := map[string]any{}
	changed := entityKeys(deltas)
	sort.Strings(changed) // a stable payload, so an identical change is an identical frame
	for _, ent := range changed {
		if c := stream[ent]; c != nil {
			rows[ent] = projectRows(deltas[ent], c.keepFields(), nil)
		}
	}
	// The stream reaches every subscriber with no actor to authorize against, so
	// gated fields are stripped unconditionally (sseSafe); clients receive them
	// only over the per-actor API.
	safe := s.sseSafe(rows)
	data, err := json.Marshal(map[string]any{
		"deltas": safe, "media": mediaGrants(safe), "changed": changed, "seq": s.writeSeq.Load()})
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
	// `route` is the path being rendered. It is bound here, once, and travels to
	// the client in the same state JSON the route parameters do — so a nav inside
	// a shared layout can compare a destination against it and mark itself active,
	// and the client cannot disagree with the server about which page this is,
	// because it never computes the answer for itself.
	store["route"] = r.URL.Path
	// Active theme for first paint: the client persists the chosen palette in the
	// `fa_theme` cookie, so the server can stamp `data-theme` on <html> and seed the
	// `theme` state before any JS runs — no flash of the base palette on reload.
	themeAttr := ""
	if _, switchable := store["theme"]; switchable {
		if c, err := r.Cookie("fa_theme"); err == nil && validThemeName(c.Value) {
			store["theme"] = c.Value
			if c.Value != "" {
				themeAttr = ` data-theme="` + c.Value + `"`
			}
		}
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

	// One pass builds the first paint and collects, per region, exactly the rows
	// that paint used — so the bootstrap the client hydrates from is the render's
	// own output rather than a second, wider answer to the same question.
	rd := s.newRenderer(pg, store)
	var body strings.Builder
	for i, n := range pg.View {
		rd.node(&body, n, store, childPath("", i))
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
	// The server-render store (above) holds @private values and whole entity
	// collections for server-side logic; the client bootstrap carries neither.
	stateJSON, _ := json.Marshal(s.clientState(pg, store, rd))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Phase 6: stamp the negotiated locale so a client knows which catalog to load.
	w.Header().Set("Content-Language", s.i18n.localeFor(r))
	dev := ""
	if s.dev != nil {
		dev = devScript
	}
	fmt.Fprintf(w, page, themeAttr, s.headMeta(pg, store), csrfToken(sid), themeCSS(s.ir.Theme, s.ir.ThemeDark, s.ir.Themes), safeStyleBody(s.ir.CSS), body.String(), irJSON, stateJSON, dev)
}

// headMeta renders a page's <head> metadata: the <title> (the page's `meta title`
// if set, else the app name), plus <meta description> and OpenGraph tags when a
// `meta description` is given. Both are interpolated against the route scope, so a
// dynamic route (`/post/:id`) gets a per-record title for SEO and link previews.
func (s *Server) headMeta(pg *ir.Page, store map[string]any) string {
	title := s.ir.App + " — Facet"
	if len(pg.Title) > 0 {
		title = s.segsToString(pg.Title, store)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<title>%s</title>", html.EscapeString(title))
	fmt.Fprintf(&b, "\n<meta property=\"og:title\" content=\"%s\">", html.EscapeString(title))
	if len(pg.Desc) > 0 {
		desc := s.segsToString(pg.Desc, store)
		fmt.Fprintf(&b, "\n<meta name=\"description\" content=\"%s\">", html.EscapeString(desc))
		fmt.Fprintf(&b, "\n<meta property=\"og:description\" content=\"%s\">", html.EscapeString(desc))
		b.WriteString("\n<meta property=\"og:type\" content=\"website\">")
	}
	return b.String()
}

// callService posts a service operation's arguments as JSON to an external brain,
// fire-and-forget: a `call` is a side effect, so it never blocks the action's
// response. Failures are logged, not surfaced — the authority did its part. The
// only egress is to the URLs declared in `service` blocks.
func (s *Server) callService(baseURL, op string, body map[string]any) {
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/" + op
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(payload)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "facet: service call %s failed: %v\n", endpoint, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			fmt.Fprintf(os.Stderr, "facet: service call %s returned %d\n", endpoint, resp.StatusCode)
		}
	}()
}

// callServiceSync is the request→response form behind `let x = call …`: it posts
// the arguments and waits for the brain's typed answer, decoding the JSON reply.
// The reply may be a bare JSON value (`[1,2,3]`, `42`) or an object wrapping it in
// a `result` field (`{"result": …}`); both are accepted. A non-2xx or transport
// failure is an error, which aborts the action. (The action holds the store lock
// for the round-trip, so a bound brain should answer fast — it is the authority's
// egress, on localhost in the mesh.)
func (s *Server) callServiceSync(baseURL, op string, body map[string]any) (any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/" + op
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(endpoint, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
	}
	var decoded any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	// Unwrap a {"result": …} envelope if present; otherwise use the value as-is.
	if obj, ok := decoded.(map[string]any); ok {
		if v, has := obj["result"]; has {
			return v, nil
		}
	}
	return decoded, nil
}

// coerceRet coerces a decoded service result to its declared return type — a
// scalar via coerce, or each element of a list.
func (s *Server) coerceRet(v any, ret string, list bool) any {
	if !list {
		return s.coerceOne(v, ret)
	}
	items, ok := v.([]any)
	if !ok {
		if v == nil {
			return []any{}
		}
		items = []any{v} // a lone value where a list was declared — wrap it
	}
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = s.coerceOne(it, ret)
	}
	return out
}

// coerceOne coerces one returned value to its declared type. A record return is
// decoded field-by-field against the record's schema — each field coerced to its
// own declared type (and lists of fields handled element-wise) — so `v.score` is
// an int and `v.reasons` a list of text, regardless of how loosely the brain typed
// its JSON. An unknown type, or a non-record, falls back to scalar coercion.
func (s *Server) coerceOne(v any, typ string) any {
	rec := s.byRecord[typ]
	if rec == nil {
		return coerce(v, typ)
	}
	obj, _ := v.(map[string]any)
	out := record{}
	for _, f := range rec.Fields {
		fv := obj[f.Name] // nil when the reply omitted it
		if f.List {
			out[f.Name] = coerceRetScalarList(fv, f.Type)
		} else {
			out[f.Name] = coerce(fv, f.Type)
		}
	}
	return out
}

// coerceRetScalarList coerces a decoded value to a list of a scalar type — each
// element coerced, a lone value wrapped, a nil treated as the empty list.
func coerceRetScalarList(v any, typ string) any {
	items, ok := v.([]any)
	if !ok {
		if v == nil {
			return []any{}
		}
		items = []any{v}
	}
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = coerce(it, typ)
	}
	return out
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
func themeCSS(theme, dark map[string]string, named map[string]map[string]string) string {
	if len(theme) == 0 && len(dark) == 0 && len(named) == 0 {
		return ""
	}
	var b strings.Builder
	if len(theme) > 0 {
		b.WriteString(":root{")
		writeThemeVars(&b, theme)
		b.WriteString("}")
	}
	// The dark palette overrides the same tokens when the OS asks for dark mode, so
	// one declarative `theme dark:` block restyles the whole UI for both schemes.
	// It is *also* emitted under `[data-theme="dark"]` so a runtime switch can force
	// dark independent of the OS (and `[data-theme="light"]` can force the base).
	if len(dark) > 0 {
		b.WriteString("@media(prefers-color-scheme:dark){:root{")
		writeThemeVars(&b, dark)
		b.WriteString("}}")
		b.WriteString(`[data-theme="dark"]{`)
		writeThemeVars(&b, dark)
		b.WriteString("}")
	}
	// Each named palette becomes a `[data-theme="<name>"]` block, selected when the
	// `theme` state holds its name. Stable name order keeps the output deterministic.
	if len(named) > 0 {
		names := make([]string, 0, len(named))
		for n := range named {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, `[data-theme="%s"]{`, n)
			writeThemeVars(&b, named[n])
			b.WriteString("}")
		}
	}
	return b.String()
}

// validThemeName guards a theme name read from the (untrusted) `fa_theme` cookie
// before it is interpolated into an HTML attribute and a CSS selector. It admits
// only the same shape the parser accepts for a palette name — lowercase letters,
// digits, and interior hyphens — plus the empty string (the base palette). Any
// stray quote, angle bracket, or space is rejected, so the cookie cannot break out.
func validThemeName(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// safeStyleBody emits author CSS into a <style> element verbatim, neutralising only
// the one sequence that could break out of the element — a literal `</style` — by
// escaping the slash (valid inside a CSS string, the only place it could legitimately
// appear). The author owns this CSS, so nothing else is sanitised.
func safeStyleBody(css string) string {
	if css == "" {
		return ""
	}
	r := strings.NewReplacer("</style", `<\/style`, "</STYLE", `<\/STYLE`)
	return r.Replace(css)
}

// nodeAttrs renders an element's class/style/id attributes for the author's
// trailing node modifiers. The author's `class "..."` is appended after the
// built-in `fa-*` class so both apply, `style "..."` becomes an inline style
// attribute, and `anchor "..."` becomes the element's id. All are escaped.
// Returns "" when there is no base class and the author supplied none of them.
//
// An interpolated class has already been resolved into n.Class by renderer.node,
// which is the one place with a scope to resolve it against.
//
// This is the one place a node's hardcoded base class and its author-supplied
// modifiers are merged, and every node kind that emits an element must call it —
// the parser strips the modifiers off any node's line
// (parser.stripNodeMods, called once for every kind in parseNodes), so the IR
// never tells this renderer which kinds "have" the escape hatch. A kind that
// builds its own tag without going through here silently has none, which is
// exactly the bug this function exists to make impossible: it used to be called
// from every leaf node but not from tabs/form/upload's hardcoded classes, nor
// from list/if/match/overlay/use's hardcoded-region wrapper div — the client's
// `render()` applies `node.class`/`node.style`/`node.anchor` to every element it produces with
// no such split, so those seven kinds rendered one page on first paint and a
// different one the instant the client hydrated.
func nodeAttrs(base string, n ir.Node) string {
	cls := base
	if n.Class != "" {
		if cls != "" {
			cls += " "
		}
		cls += n.Class
	}
	var b strings.Builder
	if cls != "" {
		fmt.Fprintf(&b, ` class="%s"`, html.EscapeString(cls))
	}
	if n.Style != "" {
		fmt.Fprintf(&b, ` style="%s"`, html.EscapeString(n.Style))
	}
	// An author's `anchor "install"` is the element's `id` — the thing a `#install`
	// link scrolls to. It is written here, at the one attribute choke point every
	// element node goes through, for the reason class/style ended up here: the
	// alternative is one `id=` Fprintf per node kind and a future node kind that
	// forgets it. It is NOT n.ID; that is the runtime's region address and is
	// written as data-fa-region by regionAttrs, so a node can carry both without
	// either name standing in for the other.
	if n.Anchor != "" {
		fmt.Fprintf(&b, ` id="%s"`, html.EscapeString(n.Anchor))
	}
	return b.String()
}

// regionAttrs is nodeAttrs plus the `data-fa-region` id that list/if/match/
// overlay/use nodes carry when they are a top-level region (addressable by the
// client without walking the tree). Kept as one function, not a class/style call
// plus a separate literal `data-fa-region` Fprintf per node, so the id can never
// be the half of the attribute set a future container node forgets.
func regionAttrs(base, id string, n ir.Node) string {
	attrs := nodeAttrs(base, n)
	if id == "" {
		return attrs
	}
	return fmt.Sprintf(` data-fa-region="%s"%s`, id, attrs)
}

// writeThemeVars emits `--fa-<name>:<value>;` for each token, in a stable order.
func writeThemeVars(b *strings.Builder, theme map[string]string) {
	names := make([]string, 0, len(theme))
	for k := range theme {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(b, "--fa-%s:%s;", k, strings.ReplaceAll(theme[k], "}", ""))
	}
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

// maxTriggerDepth bounds reaction chains as a defense in depth: the compiler
// already proves the trigger graph acyclic, so a well-formed app never approaches
// this — it only stops a runaway if that proof is ever bypassed.
const maxTriggerDepth = 64

// runAction is the public entry point every projection calls to run a server
// action — the web event channel, the JSON API, scheduled jobs, and webhooks. It
// runs the action, then, on success, fires any event triggers the action's
// completion registers (`on <action> -> <reaction>`). The trigger fan-out lives
// outside the action lock (it re-enters runAction for each reaction), so the lock
// is never held re-entrantly.
func (s *Server) runAction(sid string, act *ir.Action, args []any) (map[string]any, int, string) {
	return s.runActionDepth(sid, act, args, 0)
}

func (s *Server) runActionDepth(sid string, act *ir.Action, args []any, depth int) (map[string]any, int, string) {
	deltas, status, msg := s.runActionLocked(sid, act, args)
	if status == http.StatusOK {
		s.fireTriggers(act.Name, depth)
	}
	return deltas, status, msg
}

// fireTriggers runs the reactions registered for a just-completed action, each as
// its own server action under system authority, synchronously. Reactions may chain
// (a reaction's success fires its own triggers); the depth cap is a backstop to the
// compiler's acyclicity proof.
func (s *Server) fireTriggers(actName string, depth int) {
	if depth >= maxTriggerDepth {
		s.obs.log.Warn("trigger depth cap hit", slog.String("action", actName))
		return
	}
	for _, tr := range s.triggers[actName] {
		if react := s.byAction[tr.Action]; react != nil {
			s.runActionDepth(systemSID, react, nil, depth+1)
		}
	}
}

// runActionLocked is the one authoritative execution of a server-placed action. It
// binds arguments, enforces required policies, applies the statements to
// authoritative state under the lock, persists and fans out entity changes, and
// returns the per-session scalar deltas (plus an HTTP-shaped status so each caller
// can report failures in its own idiom). Trigger fan-out is the caller's job.
// ensureSession guarantees a session exists for sid before an action reads scope.
// The system session (jobs, triggers, webhooks) is a verified admin so its actions
// pass policies as a trusted internal caller; any other missing session starts as
// a guest. Callers hold s.mu.
func (s *Server) ensureSession(sid string) *sessionState {
	if ses := s.sessions[sid]; ses != nil {
		return ses
	}
	var ses *sessionState
	if sid == systemSID {
		ses = s.newSession("system", roleAdmin)
		ses.verified = true
	} else {
		ses = s.newSession("guest", "guest")
	}
	s.sessions[sid] = ses
	return ses
}

func (s *Server) runActionLocked(sid string, act *ir.Action, args []any) (map[string]any, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureSession(sid) // so scope() and policies see the right actor (system → admin)
	scope := s.scope(sid)
	for i, p := range act.Params {
		var v any
		if i < len(args) {
			var ok bool
			if v, ok = coerceParam(args[i], p.Type); !ok {
				return nil, http.StatusBadRequest, fmt.Sprintf(
					"%s: parameter %q expects %s, got %v", act.Name, p.Name, p.Type, args[i])
			}
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

	deltas := map[string]any{}
	entChanged := map[string]bool{}
	ses := s.sessions[sid] // ensureSession guaranteed it exists, with the right actor
	sess := ses.state
	var ops []durOp // durable writes, replayed in one transaction at the end
	// What this action changes in the working set, recorded as it changes it, so a
	// refused commit can be undone rather than merely reported (runtime/undo.go).
	undo := newUndoLog(ses)
	for _, st := range act.Body {
		switch st.Op {
		case "check":
			// Validation in body order — runs after any earlier `let` bind so it can
			// validate a brain result. The compiler guarantees checks precede every
			// mutation, so a failure here has applied nothing to roll back.
			if !truthy(eval(st.Value, scope)) {
				s.recordAudit(actor, act.Name, false, "check failed: "+st.Msg)
				s.obs.metrics.observeAction(act.Name, "invalid")
				return nil, http.StatusUnprocessableEntity, st.Msg
			}
		case "assign":
			v := eval(st.Value, scope)
			if !sameValue(sess[st.Target], v) {
				undo.state(sess, st.Target)
				sess[st.Target] = v
				scope[st.Target] = v
				if !s.privateNm[st.Target] {
					deltas[st.Target] = v // a @private cell is server-only — never shipped
				}
			}
		case "establish":
			// Adopt a custom session identity (PIAL): set the session actor (and
			// optionally role) in place — effective for every later request — and
			// echo it as a delta so a reactive {actor} updates and clustering syncs.
			na := toStr(eval(st.Value, scope))
			ses.actor = na
			scope["actor"] = na
			deltas["actor"] = na
			if st.Role != nil {
				nr := toStr(eval(st.Role, scope))
				ses.role = nr
				scope["role"] = nr
				deltas["role"] = nr
			}
		case "add":
			row := record{}
			for _, fi := range st.Fields {
				row[fi.Name] = eval(fi.Expr, scope)
			}
			// Declarative constraints (@required/@unique/@min/@max/@matches) are
			// enforced before the row lands, so invalid data never reaches the store.
			if msg := s.constraintError(st.Entity, row, "", nil); msg != "" {
				s.recordAudit(actor, act.Name, false, "constraint: "+msg)
				s.obs.metrics.observeAction(act.Name, "invalid")
				return nil, http.StatusUnprocessableEntity, msg
			}
			undo.entity(s, st.Entity)
			s.nextID[st.Entity]++
			row["id"] = s.nextID[st.Entity]
			s.entities[st.Entity] = append(s.entities[st.Entity], row)
			ops = append(ops, durOp{kind: "save", entity: st.Entity, row: row})
			entChanged[st.Entity] = true
		case "set":
			key := eval(st.Key, scope)
			for _, r := range s.entities[st.Entity] {
				if m, ok := r.(record); ok && equal(m["id"], key) {
					nv := eval(st.Value, scope)
					// Validate the proposed value (a @unique scan ignores this row).
					cand := record{}
					for k, v := range m {
						cand[k] = v
					}
					cand[st.Field] = nv
					if msg := s.constraintError(st.Entity, cand, st.Field, m["id"]); msg != "" {
						s.recordAudit(actor, act.Name, false, "constraint: "+msg)
						s.obs.metrics.observeAction(act.Name, "invalid")
						return nil, http.StatusUnprocessableEntity, msg
					}
					undo.field(m, st.Field)
					m[st.Field] = nv
					ops = append(ops, durOp{kind: "save", entity: st.Entity, row: m})
					entChanged[st.Entity] = true
					break
				}
			}
		case "remove":
			soft := s.softDel[st.Entity]
			if st.Where != nil {
				// Filtered delete: remove every row the predicate accepts, with the
				// item variable bound to each row (mirrors the filtered-agg fold). For a
				// @softdelete entity a match is archived (flagged + persisted) rather than
				// dropped, and is hidden from the live set either way.
				rows := s.entities[st.Entity]
				prev, had := scope[st.Var]
				kept := make([]any, 0, len(rows))
				removed := map[int]bool{}
				for _, r := range rows {
					m, ok := r.(record)
					if !ok {
						kept = append(kept, r)
						continue
					}
					scope[st.Var] = m
					if truthy(eval(st.Where, scope)) {
						id := m["id"]
						if soft {
							undo.field(m, "archived")
							m["archived"] = true
							ops = append(ops, durOp{kind: "save", entity: st.Entity, row: m})
						} else {
							ops = append(ops, durOp{kind: "delete", entity: st.Entity, id: id})
						}
						removed[toInt(id)] = true
					} else {
						kept = append(kept, r)
					}
				}
				if had {
					scope[st.Var] = prev
				} else {
					delete(scope, st.Var)
				}
				if len(removed) > 0 {
					undo.entity(s, st.Entity)
					s.entities[st.Entity] = kept
					entChanged[st.Entity] = true
					// A hard delete cascades to children; an archive leaves the row (and so
					// its children's foreign keys) intact, so it does not.
					if !soft {
						s.cascadeMem(st.Entity, removed, entChanged, undo)
					}
				}
				break
			}
			key := eval(st.Key, scope)
			rows := s.entities[st.Entity]
			for i, r := range rows {
				if m, ok := r.(record); ok && equal(m["id"], key) {
					undo.entity(s, st.Entity)
					s.entities[st.Entity] = append(rows[:i:i], rows[i+1:]...)
					if soft {
						undo.field(m, "archived")
						m["archived"] = true
						ops = append(ops, durOp{kind: "save", entity: st.Entity, row: m})
					} else {
						ops = append(ops, durOp{kind: "delete", entity: st.Entity, id: key})
						// the database cascades the delete to children; mirror it in memory.
						s.cascadeMem(st.Entity, map[int]bool{toInt(key): true}, entChanged, undo)
					}
					entChanged[st.Entity] = true
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
			undo.entity(s, st.Entity)
			s.entities[st.Entity] = []any{}
			ops = append(ops, durOp{kind: "clear", entity: st.Entity})
			entChanged[st.Entity] = true
			s.cascadeMem(st.Entity, removed, entChanged, undo)
		case "call":
			// An external-service effect. The compiler proved this action is
			// server-placed, so we are on the authority — post the named arguments
			// to the brain.
			if sv := s.byService[st.Service]; sv != nil {
				var params []string
				for _, op := range sv.Ops {
					if op.Name == st.Field {
						params = op.Params
						break
					}
				}
				body := map[string]any{}
				for i, arg := range st.Args {
					key := fmt.Sprintf("arg%d", i)
					if i < len(params) {
						key = params[i]
					}
					body[key] = eval(arg, scope)
				}
				if st.Bind != "" {
					// Request→response: wait for the brain's typed answer and bind it
					// into scope so the rest of the body can use it. A failed call
					// aborts the action (surfaces via failed(<action>)).
					res, err := s.callServiceSync(sv.URL, st.Field, body)
					if err != nil {
						s.obs.metrics.observeAction(act.Name, "service_error")
						return nil, http.StatusBadGateway, fmt.Sprintf("%s.%s unavailable", st.Service, st.Field)
					}
					scope[st.Bind] = s.coerceRet(res, st.Ret, st.RetList)
				} else {
					s.callService(sv.URL, st.Field, body) // fire-and-forget
				}
			}
		}
		// keep entity collections in scope fresh for later statements.
		for ent := range entChanged {
			scope[ent] = s.entities[ent]
		}
	}

	// Persist the whole action atomically: every write rides one transaction, so
	// the database never holds a half-applied action.
	//
	// A refused commit FAILS THE REQUEST. It used to be logged and swallowed —
	// the action answered 200 {"ok":true} for a write the database had rejected,
	// which is the one answer a framework must never give — and the working set
	// went on serving values nothing had stored. Both are undone here: the log
	// records what the body changed, the rollback puts it back, and the caller is
	// told. After this the action either happened in the store and in memory, or
	// in neither.
	if err := s.commit(ops); err != nil {
		undo.rollback(s, ses, sess)
		s.recordAudit(actor, act.Name, false, "store write failed: "+err.Error())
		s.obs.metrics.observeAction(act.Name, "error")
		s.obs.log.Error("action rolled back: the store refused the write",
			slog.String("action", act.Name), slog_err(err))
		return nil, http.StatusInternalServerError, "the write could not be stored and was rolled back"
	}

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

// commit replays an action's durable writes in a single transaction and RETURNS
// whether they landed. A failure rolls the whole batch back, so the database
// stays consistent, and the error travels to the caller, who is the only one who
// can decide what a lost write means for the request in hand.
//
// It used to return nothing and log instead. That is why a rejected batch could
// answer {"ok":true}: the one place that knew the write had failed had no way to
// say so, and every caller assumed success because success was the only thing it
// was ever told. A store write is not best-effort — it is the write.
func (s *Server) commit(ops []durOp) error {
	if len(ops) == 0 {
		return nil
	}
	tx, err := s.store.Begin()
	if err != nil {
		return s.persist(fmt.Errorf("begin transaction: %w", err))
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
			return s.persist(fmt.Errorf("action write rolled back: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return s.persist(fmt.Errorf("commit failed: %w", err))
	}
	return nil
}

// cascadeMem prunes the in-memory working set after a parent row is removed: it
// drops the rows that referenced it, recursing through the reverse-relation
// graph, and marks every affected entity changed so the deletions fan out to live
// clients.
//
// It does NOT delete anything from the database and must not start: the cascade
// is one rule, declared once (ir.References) and enforced in one place — the
// store. pgStore states it as ON DELETE CASCADE, fqStore declares it to FacetQL
// as a reference the engine expands into the same frame as the delete, memStore
// applies it to its own rows. Issuing the child deletes from here would be a
// second implementation of that rule, running in a second transaction.
//
// What this is, then, is cache invalidation: s.entities is a copy of the store
// that the action paths, the projections and the SSE deltas all read, so it has
// to learn what the store just did. It goes away when that copy does — which is
// the open task — and not before, because a working set still holding rows the
// database has deleted is a row that renders, validates a uniqueness check, and
// comes back as an answer, until the next restart says otherwise.
func (s *Server) cascadeMem(parent string, removedIDs map[int]bool, entChanged map[string]bool, undo *undoLog) {
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
			undo.entity(s, ch.Entity)
			s.entities[ch.Entity] = kept
			entChanged[ch.Entity] = true
			s.cascadeMem(ch.Entity, childRemoved, entChanged, undo)
		}
	}
}

// persist reports a Store write failure to the operator and hands it back, so a
// caller can act on it. Returning the error is the point: the callers that can
// fail the request now do, and the ones that genuinely cannot (a session touch,
// an audit line) at least stay loud rather than silent.
func (s *Server) persist(err error) error {
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet: store write failed: %v\n", err)
	}
	return err
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

// clientState is the bootstrap the client renders its first frame from. It is a
// projection of the render scope, not a copy of it, and three rules decide what
// survives:
//
//   - state cells cross, minus every @private one — a server-only value (a PIAL
//     UUID, a secret) must not ride the bootstrap even though the compiler
//     already bars rendering it;
//   - entity collections do NOT cross. The rows a `for` renders travel as that
//     region's result set under "@regions", and the value of every aggregate the
//     render evaluated travels under "@aggs"; shipping the collection instead is
//     what made a twenty-row page cost the whole table and handed every viewer
//     rows they had no business seeing. What still crosses whole is `clientColls`
//     — the reads the render does not perform and so cannot answer: a button's
//     arguments, a typeahead's completion list, a route policy's aggregate;
//   - whatever does cross is narrowed to the fields the client's own evaluator
//     reads AND gated by `gateForActor`, the same gate the JSON API applies, so
//     a @requires field is withheld here exactly as it is there. That gate used
//     to live in one place — the API — while this path, a second producer of the
//     same data, handed the gated field over in plaintext.
//
// "@regions" cannot collide with a state cell: `@` cannot start an identifier,
// so no app can declare one.
func (s *Server) clientState(pg *ir.Page, store map[string]any, rd *renderer) map[string]any {
	keep := s.clientColls(pg)
	out := make(map[string]any, len(store)+2)
	for k, v := range store {
		if s.privateNm[k] || strings.HasPrefix(k, "@") {
			continue // @private cells, and the render's own machinery (@m)
		}
		if _, isEntity := s.entityByName(k); isEntity {
			c := keep[k]
			if c == nil {
				continue
			}
			out[k] = projectRows(v, c.keepFields(), s.gateForActor(k, store))
			continue
		}
		out[k] = v
	}
	regions := rd.regions
	if regions == nil {
		regions = map[string][]any{}
	}
	out["@regions"] = regions
	out["@aggs"] = rd.mat.out
	out["@seq"] = s.writeSeq.Load()
	// The client renders an `image` from a durable reference it cannot sign, so
	// every media value in this payload travels with the signature to render it
	// by. Minted last, over the payload as it will actually be sent, and empty
	// (absent) whenever media signing is off.
	if g := mediaGrants(out); len(g) > 0 {
		out["@media"] = g
	}
	return out
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
	// Which collections this caller may actually GET. An entity is described here
	// (a client compiles against its shape to POST actions about it) but its rows
	// are served only when it is published — see runtime/apiread.go — so the list
	// is stated rather than left to be discovered one 403 at a time.
	schema["readable"] = s.apiPublished()
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
		// Who is asking, resolved from the same signed session cookie the web
		// channel reads. It comes FIRST: the read rule is checked before the query
		// is built, before the cache is consulted and before the store is touched,
		// because a caller who may not read this collection must not be able to
		// learn anything from how it answers.
		scope := s.apiScope(r)
		if allowed, why := s.apiMayRead(name, scope); !allowed {
			http.Error(w, why, http.StatusForbidden)
			return
		}
		query, err := buildAPIQuery(ent, r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// An entity with @requires-gated fields serves an actor-dependent response, so
		// it bypasses the shared read cache (which is keyed only by query, not actor).
		gatedEntity := len(s.gated[name]) > 0
		cacheKey := name + "?" + r.URL.RawQuery
		if !gatedEntity {
			// Serve a hot list from the read cache when enabled; it is invalidated the
			// instant any entity changes, so it never returns stale rows. Reached only
			// by a caller the read rule already admitted.
			if body, ok := s.apiCache.get(cacheKey); ok {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "hit")
				w.Write(body)
				return
			}
		}
		rows, next, err := s.store.Query(query)
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		// One answer to "what may this actor receive of these rows", shared with the
		// page bootstrap, the region endpoint and the live stream.
		out := map[string]any{"rows": s.visibleRows(name, rows, scope)}
		if next != "" {
			out["next"] = next
		}
		body, _ := json.Marshal(out)
		if !gatedEntity {
			s.apiCache.put(cacheKey, body)
		}
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

// renderSegs writes interpolated segments as HTML — literals escaped, top-level
// binds wrapped in a data-fa-bind span the client live-updates, in-region exprs
// evaluated inline. Shared by text leaves and button labels.
func (s *Server) renderSegs(b *strings.Builder, segs []ir.Seg, scope map[string]any) {
	for _, seg := range segs {
		switch {
		case seg.Lit != "":
			b.WriteString(html.EscapeString(seg.Lit))
		case seg.E2E:
			// A sealed (@e2e) value: the authority only holds ciphertext and cannot
			// open it. Emit the ciphertext in a data attribute behind a lock
			// placeholder; the client opens it after hydration (see facet.js openE2E).
			// A bound seg keeps its data-fa-bind so a live ciphertext update still
			// lands here and is re-opened.
			var ciph string
			if seg.Bind != "" {
				ciph = toStr(eval(bindingExpr(scope, seg.Bind), scope))
			} else {
				ciph = toStr(eval(seg.Expr, scope))
			}
			bindAttr := ""
			if seg.Bind != "" {
				bindAttr = fmt.Sprintf(` data-fa-bind="%s"`, seg.Bind)
			}
			fmt.Fprintf(b, `<span class="fa-e2e" data-fa-e2e="%s"%s>%s</span>`,
				html.EscapeString(ciph), bindAttr, e2ePlaceholder)
		case seg.Bind != "":
			val := toStr(eval(bindingExpr(scope, seg.Bind), scope))
			fmt.Fprintf(b, `<span data-fa-bind="%s">%s</span>`, seg.Bind, html.EscapeString(val))
		case seg.Expr != nil:
			b.WriteString(html.EscapeString(toStr(eval(seg.Expr, scope))))
		}
	}
}

// e2ePlaceholder is what the server renders in place of a sealed value — a lock
// glyph the client replaces with the opened plaintext. The server never has the
// plaintext, so this is all it can ever show.
const e2ePlaceholder = "🔒"

// attrText is the one way an interpolated value reaches HTML that is not body
// text: an element attribute (`placeholder`, `src`, `data-fa-icon`) or the plain
// text content of a control (a submit button, an option, a tab, a link). It
// flattens the segments to one plain string and escapes it.
//
// Both contexts are served by one escaper because html.EscapeString escapes the
// union of what they need — `"` and `'` (so a value cannot close the attribute
// this renderer always quotes, and so it cannot open one of its own, which is how
// `" onmouseover="alert(1)` would become an event handler) as well as `<`, `>`
// and `&` (so it cannot open a tag in body text). Writing a second, narrower
// escaper for attributes would be two implementations of one rule, which is the
// failure this repo has already found three times.
//
// The client's mirror is `segsToStr` assigned through a DOM property
// (`i.placeholder = …`, `el.textContent = …`) — never concatenated into markup,
// which is what makes the two agree with no escaper on that side at all. The
// property assignment IS the escape, and it yields exactly the characters this
// function's output decodes to. runtime/attrtext_test.go pins that.
func (s *Server) attrText(segs []ir.Seg, scope map[string]any) string {
	return html.EscapeString(s.segsToString(segs, scope))
}

// segsToString flattens interpolated segments to a plain string — the value an
// attribute or a control's label is built from, not displayed markup. Callers
// that write into HTML go through attrText; the raw form exists for the two
// places that must not be escaped here (a route expression, which is resolved
// against the app's routes and escaped by its own caller, and richtext, which is
// handed to the markdown renderer that escapes it).
func (s *Server) segsToString(segs []ir.Seg, scope map[string]any) string {
	var sb strings.Builder
	for _, seg := range segs {
		switch {
		case seg.Lit != "":
			sb.WriteString(seg.Lit)
		case seg.Bind != "":
			sb.WriteString(toStr(eval(bindingExpr(scope, seg.Bind), scope)))
		case seg.Expr != nil:
			sb.WriteString(toStr(eval(seg.Expr, scope)))
		}
	}
	return sb.String()
}

// activeTab returns the value of the selected tab: the bound cell's value when it
// matches a tab, otherwise the first tab (so a fresh/unmatched cell shows tab one).
func activeTab(n ir.Node, scope map[string]any) string {
	cur := toStr(scope[n.Bind])
	for _, tb := range n.Children {
		if tb.Value == cur {
			return cur
		}
	}
	if len(n.Children) > 0 {
		return n.Children[0].Value
	}
	return cur
}

// children renders a node's children, giving each its own render path so a
// region nested under it can be addressed by the client (see childPath).
func (rd *renderer) children(b *strings.Builder, nodes []ir.Node, scope map[string]any, base string) {
	for i, c := range nodes {
		rd.node(b, c, scope, childPath(base, i))
	}
}

// match renders the body of the `case` matching the subject value, or the
// `else` case if none matches.
func (rd *renderer) match(b *strings.Builder, n ir.Node, scope map[string]any, path string) {
	val := toStr(eval(n.Cond, scope))
	for i, cs := range n.Children {
		if cs.Kind == "case" && cs.Value == val {
			rd.children(b, cs.Children, scope, childPath(path, i))
			return
		}
	}
	for i, cs := range n.Children {
		if cs.Kind == "else" {
			rd.children(b, cs.Children, scope, childPath(path, i))
			return
		}
	}
}

// overlay writes a modal layer: a backdrop (tagged with the bound cell so a
// click closes it) wrapping a centered panel of the overlay's children.
func (rd *renderer) overlay(b *strings.Builder, n ir.Node, scope map[string]any, path string) {
	fmt.Fprintf(b, `<div class="fa-overlay-backdrop" data-fa-close="%s"><div class="fa-overlay-panel">`,
		html.EscapeString(n.Bind))
	rd.children(b, n.Children, scope, path)
	b.WriteString(`</div></div>`)
}

// distinctFieldValues collects the unique, non-empty string values of one field
// across an entity's rows — the suggestion set a typeahead offers.
func distinctFieldValues(rows any, field string) []string {
	list, ok := rows.([]any)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range list {
		rec, ok := r.(record)
		if !ok {
			continue
		}
		if v := toStr(rec[field]); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// rows resolves the rows a repeating node renders, gates them for this actor, and
// records the entity ones at its render path — the addressed result set the
// client renders from instead of a collection it would otherwise have to hold and
// filter itself.
//
// Two nodes repeat: a `for` region, and the `for` inside a select's or a radio
// group's choice list. They are the same query and have to resolve the same way —
// one push-down, one gate, one recorded answer, one address — so they ask here
// rather than each carrying its own copy of it.
func (rd *renderer) rows(n ir.Node, scope map[string]any, path string) []any {
	s := rd.s
	// The list is a query, not a scan: listRows pushes the node's own
	// where/by/limit down to the store. What comes back is gated once, here, and
	// that same gated result set is what the HTML is built from and what the
	// client receives — so the first paint and the hydrated render can never
	// disagree about which fields this actor may see.
	rows := s.visibleRowList(n.Coll, s.listRows(n, scope), scope)
	if rows == nil {
		rows = []any{}
	}
	if _, isEntity := s.entityByName(n.Coll); isEntity {
		// A repeat over a `[T]` state cell is not recorded: that cell already
		// crosses in the state bootstrap, and the client filters it locally exactly
		// as this render did. Only a table's result set is an answer the client
		// cannot compute for itself.
		rd.regions[path] = rows
	}
	return rows
}

// optionRegionID is the region address of a control whose choices come from data,
// and "" for one whose choices are fixed.
//
// A control is a region only when it has something to re-render. A fixed list
// never changes; a list drawn from a collection changes whenever that collection
// does, and the element holding those options IS the control — so it re-fills
// under the id it already has for the cell it writes. Mirrored by optionsRegionId
// in assets/facet.js.
func optionRegionID(n ir.Node) string {
	if len(n.Children) == 0 {
		return ""
	}
	return n.ID
}

// eachOption walks a control's choices in source order, yielding each one's
// stored value and its displayed label.
//
// It is the whole of what a select and a radio group share, and the whole of what
// "choices from data" means on this side: Options (every choice fixed) and the
// `option`/`options` children (the list holding something computed) are one list
// said two ways, and each row of a repeating entry supplies one choice. Both
// controls walk it, so a dropdown and a radio group over the same collection
// cannot come to offer different choices.
//
// assets/facet.js's eachOption is this function; runtime/control_test.go pins the
// two together.
func (rd *renderer) eachOption(n ir.Node, scope map[string]any, path string, yield func(value, label string)) {
	s := rd.s
	for _, o := range n.Options {
		yield(o.Value, s.segsToString(o.Label, scope))
	}
	for i, c := range n.Children {
		cpath := childPath(path, i)
		if c.Kind != "options" {
			yield(rd.optionValue(c, scope), s.segsToString(c.Label, scope))
			continue
		}
		rows := rd.rows(c, scope, cpath)
		fanout := rd.mat.fanout
		rd.mat.fanout = len(rows)
		for j, r := range rows {
			child := cloneScope(scope)
			child[c.Var] = r
			// A row's expressions are addressed exactly the way the client addresses
			// them (listRowPath), so an aggregate inside a label resolves to the same
			// value on both sides.
			rd.mat.path = s.listRowPath(c, cpath, j, r)
			yield(rd.optionValue(c, child), s.segsToString(c.Label, child))
		}
		rd.mat.fanout = fanout
		rd.mat.path = path // the rows moved it; the control's own scope resumes here
	}
}

// optionValue is one choice's stored identity: the literal the author wrote, or
// the value this render computed for this row. It is Node.Val standing beside
// Node.Value, resolved.
func (rd *renderer) optionValue(n ir.Node, scope map[string]any) string {
	if n.Val != nil {
		return toStr(eval(n.Val, scope))
	}
	return n.Value
}

// headingLevel turns a heading's evaluated level into the element it renders as.
//
// It clamps rather than refusing, because this is a rendering path and rendering
// paths in this runtime are total: toStr, toInt and truthy all have nowhere to
// put a diagnostic, and neither does this. The compiler is where a bad level is
// reported, and it refuses every one it can prove — a literal outside 1..6, a
// level whose type is not a number, a level that reads state (internal/ir/heading.go).
// What reaches here is a value that arrived at render, and for those the choice
// is between an element that does not exist and the nearest one that does.
//
// assets/facet.js contains this function character for character; a heading that
// clamped differently on the two sides would change shape at hydration, which is
// exactly the class of bug the pinning tests in this package exist for.
func headingLevel(v any) int {
	n := toInt(v)
	switch {
	case n < 1:
		return 1
	case n > 6:
		return 6
	}
	return n
}

// node renders one IR node to HTML and, for a list, records the rows it
// resolved to under this node's render path — the addressed result set the
// client will render from instead of a collection it filters itself.
func (rd *renderer) node(b *strings.Builder, n ir.Node, scope map[string]any, path string) {
	s := rd.s
	// Every aggregate this node evaluates is addressed from here. A node
	// evaluates its own expressions before it recurses, and each child sets its
	// own path on entry, so one field is enough to keep the addresses right.
	rd.mat.path = path
	// An interpolated class is resolved once, here, into the flat field the rest
	// of the renderer already reads — `n` is this call's own copy, and every
	// nodeAttrs/regionAttrs call the switch below makes is on it. Doing it here
	// rather than in nodeAttrs is what keeps the class rule one rule: nodeAttrs
	// has no scope to resolve against, and threading one through it would mean a
	// second merge path for whichever node kinds got updated to pass it.
	if len(n.ClassSegs) > 0 {
		n.Class = classText(n.ClassSegs, func(segs []ir.Seg) string {
			return s.segsToString(segs, scope)
		})
	}
	switch n.Kind {
	case "box":
		fmt.Fprintf(b, `<div%s>`, nodeAttrs("fa-box", n))
		rd.children(b, n.Children, scope, path)
		b.WriteString(`</div>`)
	case "row":
		fmt.Fprintf(b, `<div%s>`, nodeAttrs("fa-row", n))
		rd.children(b, n.Children, scope, path)
		b.WriteString(`</div>`)
	case "text":
		fmt.Fprintf(b, `<span%s>`, nodeAttrs("fa-text", n))
		s.renderSegs(b, n.Segs, scope)
		b.WriteString(`</span>`)
	case "heading":
		// A heading is a text leaf that lands in an <h1>…<h6> instead of a span:
		// same segments, same escaping, same bindings. The level is the only new
		// thing, and it is an expression because the depth a header renders at
		// belongs to whoever used it (see ast.Heading).
		lvl := headingLevel(eval(n.Level, scope))
		fmt.Fprintf(b, `<h%d%s>`, lvl, nodeAttrs("fa-heading", n))
		s.renderSegs(b, n.Segs, scope)
		fmt.Fprintf(b, `</h%d>`, lvl)
	case "image":
		// The src is minted here, at render, out of whatever durable reference the
		// row holds — never read back as a finished URL. mediaSrc is the whole
		// rule (media.go); html.EscapeString is what s.attrText would have applied,
		// and it matters more now, since a signed URL carries `&` between exp and
		// sig.
		// `alt` is what the author said the picture says. Absent, it is the empty
		// string — correct markup for a decorative image, and the reason a missing
		// description produces no visible symptom and has to be reported instead
		// (ast.Advise).
		fmt.Fprintf(b, `<img%s src="%s" alt="%s">`, nodeAttrs("fa-image", n),
			html.EscapeString(mediaSrc(s.segsToString(n.Segs, scope))), s.attrText(n.Alt, scope))
	case "icon":
		fmt.Fprintf(b, `<span%s data-fa-icon="%s" aria-hidden="true"></span>`, nodeAttrs("fa-icon", n), s.attrText(n.Segs, scope))
	case "video":
		// Same rule, same reason: a `video` src is a media value stored in a column
		// exactly as an `image` src is, and an expiring one would rot identically.
		// A <video> has no `alt`; its accessible name is `aria-label`. Written only
		// when there is one — aria-label="" names nothing and is worse than the
		// absent attribute, because it tells a reader a name exists and hands it an
		// empty one. (An image's empty alt means the opposite: skip me.)
		aria := ""
		if name := s.attrText(n.Alt, scope); name != "" {
			aria = fmt.Sprintf(` aria-label="%s"`, name)
		}
		fmt.Fprintf(b, `<video%s controls src="%s"%s></video>`, nodeAttrs("fa-video", n),
			html.EscapeString(mediaSrc(s.segsToString(n.Segs, scope))), aria)
	case "richtext":
		// markdownHTML escapes its input and emits only a fixed safe tag set.
		fmt.Fprintf(b, `<div%s>%s</div>`, nodeAttrs("fa-richtext", n), markdownHTML(s.segsToString(n.Segs, scope)))
	case "badge":
		fmt.Fprintf(b, `<span%s>`, nodeAttrs("fa-badge", n))
		s.renderSegs(b, n.Segs, scope)
		b.WriteString(`</span>`)
	case "tabs":
		active := activeTab(n, scope)
		fmt.Fprintf(b, `<div%s>`, regionAttrs("fa-tabs", n.ID, n))
		b.WriteString(`<div class="fa-tabstrip" role="tablist">`)
		for _, tb := range n.Children {
			sel := ""
			if tb.Value == active {
				sel = ` aria-selected="true"`
			}
			fmt.Fprintf(b, `<button class="fa-tab" role="tab"%s data-fa-tab="%s" data-fa-tab-bind="%s">%s</button>`,
				sel, html.EscapeString(tb.Value), html.EscapeString(n.Bind), s.attrText(tb.Label, scope))
		}
		b.WriteString(`</div>`)
		// Only the active tab's body renders — on both sides — so its children are
		// addressed under the tab's own index and the inactive tabs' regions simply
		// do not exist until the client selects them and asks for them.
		for i, tb := range n.Children {
			if tb.Value == active {
				rd.children(b, tb.Children, scope, childPath(path, i))
			}
		}
		b.WriteString(`</div>`)
	case "button":
		fmt.Fprintf(b, `<button%s data-fa-action="%s">`, nodeAttrs("", n), html.EscapeString(n.Action))
		s.renderSegs(b, n.Segs, scope)
		b.WriteString(`</button>`)
	case "list":
		rows := rd.rows(n, scope, path)
		fmt.Fprintf(b, `<div%s>`, regionAttrs("", n.ID, n))
		// One request per aggregate under this list, not one per row: the counts
		// beside twenty posts are twenty pinned values of one question, and asking
		// them one at a time would be an N+1 across the network.
		rd.prefetchCounts(n, rows, scope)
		fanout := rd.mat.fanout
		rd.mat.fanout = len(rows)
		// Each row is addressed the way the client's fillList addresses it — by id
		// for a table's rows, by ordinal for a `[T]` state cell's, which have no
		// id to be told apart by (listRowPath). Everything recorded under a row —
		// a nested region's rows, every aggregate value — is reachable only at the
		// address the client recomputes, so this is the one rule, in one place.
		for i, r := range rows {
			child := cloneScope(scope)
			child[n.Var] = r
			rd.children(b, n.Children, child, s.listRowPath(n, path, i, r))
			rd.mat.path = path // the children moved it; the next row starts from here
		}
		rd.mat.fanout = fanout
		b.WriteString(`</div>`)
	case "if":
		// `if` is control flow, not a box. The IR says so — internal/ir/build.go's
		// `case ast.If` emits a node holding children and no element — and this
		// renderer used to mint one anyway, unconditionally: a component with four
		// `if` branches put four siblings into its parent, three of them empty, so
		// a grid gave each of them a cell and the one branch that rendered landed
		// in whichever column its ordinal fell in.
		//
		// So an `if` that is not a region writes no element at all; its children
		// are the parent's children, which is what the IR always said.
		show := truthy(eval(n.Cond, scope))
		if n.ID == "" {
			if show {
				rd.children(b, n.Children, scope, path)
			}
			break
		}
		// A top-level `if` does have one thing of its own: `data-fa-region`, the
		// element the client re-fills when the condition's state changes. That has
		// to exist even while the branch is false, so it stays — with
		// `display:contents`, which generates no box of its own, only its children.
		// An empty one is then invisible to flex/grid instead of occupying a track.
		// (An `if` carries no class/style/anchor of its own, so regionAttrs writes
		// only the region id and cannot collide with the style written here.)
		fmt.Fprintf(b, `<div%s style="display:contents">`, regionAttrs("", n.ID, n))
		if show {
			rd.children(b, n.Children, scope, path)
		}
		b.WriteString(`</div>`)
	case "match":
		fmt.Fprintf(b, `<div%s>`, regionAttrs("", n.ID, n))
		rd.match(b, n, scope, path)
		b.WriteString(`</div>`)
	case "input":
		val := html.EscapeString(toStr(scope[n.Bind]))
		// `password`/`newpassword` are this node with an autocomplete token in
		// Value — the same cell, the same event, the same hydration, and the two
		// attributes a browser needs to mask the box, offer its own reveal, and
		// tell a password manager whether this is the account's secret or a new
		// one. The token is written through verbatim rather than mapped from a
		// variant name, so this renderer and facet.js cannot hold two versions of
		// which token a keyword means.
		secret := ""
		if n.Value != "" {
			secret = fmt.Sprintf(` type="password" autocomplete="%s"`, html.EscapeString(n.Value))
		}
		fmt.Fprintf(b, `<input%s%s data-fa-input="%s" value="%s" placeholder="%s">`,
			nodeAttrs("", n), secret, n.Bind, val, s.attrText(n.Placeholder, scope))
	// The four controls added alongside `input` render here. Each is the same
	// three facts — which cell, what it currently holds, how the actor changes it
	// — written as the markup a browser already knows how to operate, so the
	// first paint is a working control before any script has run. Every arm has a
	// mirror in runtime/assets/facet.js's render0, character for character;
	// runtime/control_test.go pins the two together.
	case "textarea":
		// The value sits in the element's content, because that is where a
		// textarea's default value lives — a no-JS first paint shows the draft.
		fmt.Fprintf(b, `<textarea%s data-fa-input="%s" placeholder="%s">%s</textarea>`,
			nodeAttrs("fa-textarea", n), html.EscapeString(n.Bind),
			s.attrText(n.Placeholder, scope), html.EscapeString(toStr(scope[n.Bind])))
	case "checkbox":
		// `toggle` is this node with Value=="switch": one cell contract, one
		// event, one refresh path, and a role/class that says which affordance
		// the author asked for.
		base, role := "fa-checkbox", ""
		if n.Value == "switch" {
			base, role = "fa-toggle", ` role="switch"`
		}
		checked := ""
		if truthy(scope[n.Bind]) {
			checked = " checked"
		}
		fmt.Fprintf(b, `<label%s><input type="checkbox" data-fa-input="%s"%s%s><span>%s</span></label>`,
			nodeAttrs(base, n), html.EscapeString(n.Bind), role, checked, s.attrText(n.Label, scope))
	case "radio":
		// A radio group is one node, not N nodes that have to be told they belong
		// together: the bound cell IS the group, so `name` is the cell's name and
		// the browser's own one-of-N behaviour falls out of it.
		fmt.Fprintf(b, `<div%s role="radiogroup">`, regionAttrs("fa-radio", optionRegionID(n), n))
		cur := toStr(scope[n.Bind])
		rd.eachOption(n, scope, path, func(value, label string) {
			checked := ""
			if value == cur {
				checked = " checked"
			}
			fmt.Fprintf(b, `<label class="fa-radio-option"><input type="radio" name="%s" value="%s" data-fa-input="%s"%s><span>%s</span></label>`,
				html.EscapeString(n.Bind), html.EscapeString(value),
				html.EscapeString(n.Bind), checked, html.EscapeString(label))
		})
		b.WriteString(`</div>`)
	case "overlay":
		fmt.Fprintf(b, `<div%s>`, regionAttrs("", n.ID, n))
		if truthy(scope[n.Bind]) {
			rd.overlay(b, n, scope, path)
		}
		b.WriteString(`</div>`)
	case "typeahead":
		listID := "ta-" + n.ID
		fmt.Fprintf(b, `<input class="fa-typeahead" data-fa-input="%s" list="%s" value="%s" placeholder="%s">`,
			n.Bind, listID, html.EscapeString(toStr(scope[n.Bind])), s.attrText(n.Placeholder, scope))
		fmt.Fprintf(b, `<datalist id="%s">`, listID)
		for _, v := range distinctFieldValues(scope[n.Coll], n.Value) {
			fmt.Fprintf(b, `<option value="%s">`, html.EscapeString(v))
		}
		b.WriteString(`</datalist>`)
	case "link":
		// A link's label is segments like every other label; its destination is
		// either a flat literal or interpolated segments, whichever the source was,
		// so a static link stays one string on the wire.
		label := s.attrText(n.Label, scope)

		href := n.Path
		if n.Route {
			// The destination IS the value: it is a path, not a value going into
			// one, so it is not escaped per segment — and it is only a link if it
			// names a route this app serves. Anything else renders as plain text.
			// An off-site URL arriving as a value is "anything else" on purpose:
			// only a destination the author wrote may leave this app.
			href = s.segsToString(n.PathSegs, scope)
			if !isAppRoute(s.ir.Routes, href) {
				fmt.Fprintf(b, `<span%s>%s</span>`, nodeAttrs("fa-link", n), label)
				return
			}
		} else if len(n.PathSegs) > 0 {
			href = linkHref(n.PathSegs, func(segs []ir.Seg) string {
				return s.segsToString(segs, scope)
			})
		}

		// An external destination is re-checked against the scheme allowlist here,
		// on the string that is about to reach the browser — see safeExternalHref.
		rel := ""
		if n.External {
			if !safeExternalHref(href) {
				fmt.Fprintf(b, `<span%s>%s</span>`, nodeAttrs("fa-link", n), label)
				return
			}
			// `rel` is unconditional and not an author's to forget. `noreferrer`
			// keeps this app's URL — which may carry a route parameter naming a row
			// or a person — out of the Referer header sent to a third party;
			// `noopener` costs nothing and is already correct if a `target` is ever
			// added. There is deliberately no `target="_blank"`: which tab a reader's
			// navigation lands in is that reader's, and a runtime that decides it for
			// every off-site link has taken the back button away from all of them.
			rel = ` rel="noopener noreferrer"`
		}

		fmt.Fprintf(b, `<a%s href="%s"%s>%s</a>`,
			nodeAttrs("fa-link", n), html.EscapeString(href), rel, label)
	case "select":
		fmt.Fprintf(b, `<select%s data-fa-input="%s">`, regionAttrs("fa-select", optionRegionID(n), n), n.Bind)
		cur := toStr(scope[n.Bind])
		rd.eachOption(n, scope, path, func(value, label string) {
			sel := ""
			if value == cur {
				sel = " selected"
			}
			fmt.Fprintf(b, `<option value="%s"%s>%s</option>`,
				html.EscapeString(value), sel, html.EscapeString(label))
		})
		b.WriteString(`</select>`)
	case "form":
		fmt.Fprintf(b, `<form%s data-fa-form="%s">`, nodeAttrs("fa-form", n), html.EscapeString(n.Action))
		rd.children(b, n.Children, scope, path)
		fmt.Fprintf(b, `<button type="submit">%s</button></form>`, s.attrText(n.Label, scope))
	case "upload":
		fmt.Fprintf(b, `<label%s>%s<input type="file" data-fa-upload="%s"></label>`,
			nodeAttrs("fa-upload", n), s.attrText(n.Label, scope), n.Bind)
	case "use":
		comp := s.byComponent[n.Name]
		if comp == nil {
			return
		}
		// "fa-use" is the default only when there is no region id, mirroring the
		// client's `el("div", node.id ? null : "fa-use")` — a `use` that is its own
		// addressable region is styled by the caller, not by this default.
		useBase := "fa-use"
		if n.ID != "" {
			useBase = ""
		}
		fmt.Fprintf(b, `<div%s>`, regionAttrs(useBase, n.ID, n))
		child := cloneScope(scope)
		for i, p := range comp.Params {
			var v any
			if i < len(n.Args) {
				v = eval(n.Args[i], scope)
			}
			child[p.Name] = coerce(v, p.Type)
		}
		// The component body renders inline at the call site, so its nodes are
		// addressed under this `use` node's path — one component used twice gets
		// two distinct sets of region keys, which is what makes them addresses.
		rd.children(b, comp.View, child, path)
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
// newSessionID mints a session identifier from the OS CSPRNG.
//
// These used to be a process counter — `s1`, `s2`, `s-<instance>-3` — so the
// identifier of every session the server had ever issued was known by
// construction, and the number of them was public. What stood between a guess
// and a hijack was the cookie's signature alone, which makes one secret the
// entire defence and leaves nothing behind it.
//
// It also matters where the raw identifier travels *without* that signature.
// Under clustering the sid is the address of a `__session` node in the shared
// database, so anything able to read that kind could enumerate `s1..sN`; the
// signature never enters that path at all. 192 bits from `crypto/rand` removes
// the class rather than defending it, and costs nothing — the previous
// justification for the counter (a per-instance prefix, so two instances never
// collide on a session key) is satisfied better by randomness than by a
// coordinated namespace.
func newSessionID() string {
	return randomToken(24)
}

// rotateSession issues a NEW identifier for an existing session and retires the
// old one, returning the id the caller must use from here on.
//
// This is the fix for session fixation, and the reason the rule is "rotate on
// every privilege change" rather than "rotate on login": an attacker obtains an
// ordinary session from the app — just by visiting it — gets a victim's browser
// to adopt that identifier, and waits. `signIn` used to stamp the identity onto
// whatever identifier was already there, so the attacker's planted session
// became the victim's authenticated one without a password ever changing hands.
// The cookie being signed does not help: the attacker's session is one this
// server legitimately issued and legitimately signed.
//
// Logout is the same rule in the other direction. Clearing the identity but
// keeping the identifier means anyone who captured it while the user was signed
// in still holds a session the server will accept.
//
// The old id is dropped from the local table and from the shared store, so a
// clustered instance cannot resurrect it from underneath the rotation.
func (s *Server) rotateSession(w http.ResponseWriter, old string) string {
	fresh := newSessionID()

	s.mu.Lock()
	ses := s.sessions[old]
	if ses == nil {
		// Nothing to carry over; the caller still gets a fresh identifier so
		// the outcome does not depend on whether the session was live.
		s.mu.Unlock()
		s.setSessionCookie(w, fresh)
		return fresh
	}

	delete(s.sessions, old)
	s.sessions[fresh] = ses
	s.obs.metrics.setSessions(int64(len(s.sessions)))
	s.mu.Unlock()

	s.dropSharedSession(old)
	s.persistSession(fresh)
	s.setSessionCookie(w, fresh)

	return fresh
}

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
	sid := newSessionID()
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
			// evalRowPredicate, not eval: this predicate is answered once per row at
			// one render path, so an aggregate inside it has no address and must not
			// be memoized under the list's.
			if evalRowPredicate(n.Where, child) {
				filtered = append(filtered, r)
			}
		}
	}
	out := sortRows(filtered, n.Order, n.Desc)
	if n.Limit != nil {
		if lim := toInt(eval(n.Limit, scope)); lim > 0 && len(out) > lim {
			out = out[:lim]
		}
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

// coerceParam is coerce at a boundary that can refuse.
//
// `coerce` is total: it is called from rendering and expression evaluation,
// which have nowhere to report a failure, so an uninterpretable value becomes
// the type's zero. That is the right answer there and the wrong one here. An
// action argument arrives from a caller — a form post, a JSON API request, a
// route parameter — and substituting 0 for something the caller wrote turned a
// malformed request into a successful write of the wrong row: `reply("abc", …)`
// answered `{"ok":true}` and stored `tweet: 0`, a reply attached to a post that
// does not exist, with nothing anywhere recording that it happened.
//
// So this reports whether the value was interpretable, and the caller turns a
// `false` into a 400. What it refuses is decided by the argument's JSON shape,
// not only by the text it holds: `{"args":[true]}` for an `int` parameter is no
// more a number than `"abc"` is, and the total conversions answer 1 for it — and
// 0 for `{}` — with exactly the confidence they used to answer 0 for "abc". A
// shape-blind boundary reaches the same wrong-row write by a different door.
//
// What each declared parameter type accepts, and why:
//
//	int, money, date  A JSON number (float64 out of the decoder, truncated toward
//	                  zero as `toInt` does), or a number written as text — the
//	                  normal case, since every route parameter and form field is
//	                  text. Null, and text that is empty or blank, are an omitted
//	                  argument rather than a malformed one: they yield the zero,
//	                  because refusing them would reject every unfilled optional
//	                  form field and every webhook body that leaves a field out
//	                  (see webhook.go, which maps an absent field to nil on
//	                  purpose). Refused: true/false, objects, arrays, and text
//	                  that is not a number.
//	bool              True/false; text, which is true when non-empty — the rule
//	                  both interpreters already share, and what a checkbox's ""
//	                  means; a number, true when non-zero; null, the omitted
//	                  argument, which is false. Refused: objects and arrays.
//	text              Text, a number, true/false (a JSON body may well carry an
//	                  id or a flag where the action declares text), and null, the
//	                  empty argument. Refused: objects and arrays.
//	anything else     An enum name or an entity name; passed through untouched,
//	                  as `coerce` does. This function is not handed the IR, so it
//	                  cannot tell the two apart — and an enum's value is text
//	                  while an entity's is a whole row record, so any refusal by
//	                  shape here would have to reject one of them wrongly.
//	                  Whether text names a member of its enum is a question about
//	                  the value, not about its shape, and is not this gate's.
//
// Objects and arrays are refused for every scalar type because no parameter can
// be declared record- or list-typed (`parseSignature` refuses a list outright),
// so a structured argument is malformed whichever scalar it is aimed at. The
// total conversions do have readings for them — `toStr` joins an array with
// commas, `truthy` calls any map true — but those are rendering conventions the
// two interpreters agree on, not interpretations of an argument.
//
// []byte is text throughout: it is the shape a database driver hands back for a
// text column, and `toStr`/`toInt`/`truthy` all read it as the string it holds.
// Accepting it on the same terms as a string is what keeps this gate from
// disagreeing with the conversions it guards.
func coerceParam(v any, typ string) (any, bool) {
	switch typ {
	case "int", "money", "date":
		switch t := v.(type) {
		case nil:
			return zero(typ), true
		case string:
			return numericArg(t, typ)
		case []byte:
			return numericArg(string(t), typ)
		case bool, map[string]any, []any:
			return nil, false
		default:
			// One definition of "is a number": the same helper the evaluator uses
			// to decide whether a value is numeric at all.
			if _, isNum := numeric(v); !isNum {
				return nil, false
			}
		}
	case "bool", "text":
		switch v.(type) {
		case map[string]any, []any:
			return nil, false
		}
	}

	return coerce(v, typ), true
}

// numericArg interprets a numeric parameter that arrived as text. Blank is an
// omitted argument (the zero), text that is not a number is a refusal.
func numericArg(s, typ string) (any, bool) {
	if strings.TrimSpace(s) == "" {
		return zero(typ), true
	}
	n, ok := parseNumericText(s)
	if !ok {
		return nil, false
	}
	return n, true
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

// constraintError validates a proposed row against its entity's declarative field
// constraints, returning a friendly message (or "" if all pass). For a `set`,
// changedField names the single field being written and excludeID is the row being
// updated (so a @unique scan ignores it); for an `add`, changedField is "" (check
// every field) and excludeID is nil.
func (s *Server) constraintError(entity string, row record, changedField string, excludeID any) string {
	def, ok := s.entityByName(entity)
	if !ok {
		return ""
	}
	for i := range def.Fields {
		f := &def.Fields[i]
		if changedField != "" && f.Name != changedField {
			continue
		}
		if !f.Required && !f.Unique && f.Min == nil && f.Max == nil && f.Matches == "" {
			continue
		}
		val, present := row[f.Name]
		if f.Required && (!present || val == nil || toStr(val) == "") {
			return f.Name + " is required"
		}
		if !present || val == nil {
			continue // an absent optional value skips the value-shape checks
		}
		if f.Min != nil || f.Max != nil {
			if f.Type == "text" {
				n := len([]rune(toStr(val)))
				if f.Min != nil && n < *f.Min {
					return fmt.Sprintf("%s must be at least %d characters", f.Name, *f.Min)
				}
				if f.Max != nil && n > *f.Max {
					return fmt.Sprintf("%s must be at most %d characters", f.Name, *f.Max)
				}
			} else {
				n := toInt(val)
				if f.Min != nil && n < *f.Min {
					return fmt.Sprintf("%s must be at least %d", f.Name, *f.Min)
				}
				if f.Max != nil && n > *f.Max {
					return fmt.Sprintf("%s must be at most %d", f.Name, *f.Max)
				}
			}
		}
		if re := s.fieldRE[entity+"."+f.Name]; re != nil && !re.MatchString(toStr(val)) {
			return f.Name + " is not in the required format"
		}
		if f.Unique {
			for _, r := range s.entities[entity] {
				m, ok := r.(record)
				if !ok {
					continue
				}
				if excludeID != nil && equal(m["id"], excludeID) {
					continue
				}
				if equal(m[f.Name], val) {
					return fmt.Sprintf("%s must be unique", f.Name)
				}
			}
		}
	}
	return ""
}

// liveRows returns the rows that belong in the in-memory working set: all of them
// for a normal entity, or only the non-archived ones for a @softdelete entity (its
// archived rows persist in the store but are hidden from every read).
func liveRows(rows []any, softDelete bool) []any {
	if !softDelete {
		return rows
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		if m, ok := r.(record); ok && truthy(m["archived"]) {
			continue
		}
		out = append(out, r)
	}
	return out
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
<html lang="en"%s>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
%s
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
  /* A row lays its children out horizontally. A box directly inside a row is a
     structural column (a wireframe region) and stretches; inline items (buttons,
     text, images — e.g. an action bar) stay compact. */
  .fa-row { display: flex; flex-direction: row; gap: .75rem; align-items: flex-start; flex-wrap: wrap; }
  .fa-row > * { min-width: 0; }
  .fa-row > .fa-box { flex: 1 1 0; border: none; background: transparent; padding: 0; }
  .fa-row > button, .fa-row > .fa-text, .fa-row > .fa-image { flex: 0 0 auto; }
  /* Action-bar buttons: muted, borderless, highlight on hover — X-style. */
  .fa-row > button { background: transparent; border: none; color: var(--fa-muted); padding: .25rem .5rem; align-self: center; }
  .fa-row > button:hover { color: var(--fa-accent); }
  /* Only a row of structural columns collapses to a stack on a narrow viewport; an
     action bar (no box children) stays horizontal. */
  @media (max-width: 720px) {
    .fa-row:has(> .fa-box) { flex-direction: column; }
    .fa-row:has(> .fa-box) > .fa-box { width: 100%%; flex-basis: 100%%; }
  }
  /* image: avatars by default — a rounded, fixed square that sits inline. */
  .fa-image { width: 44px; height: 44px; border-radius: 50%%; object-fit: cover; background: var(--fa-card-border); }
  .fa-text { font-variant-numeric: tabular-nums; }
  .fa-icon { display: inline-block; width: 1.15em; height: 1.15em; vertical-align: -.18em;
             background: var(--fa-icon-bg, none) center/contain no-repeat; }
  .fa-video { max-width: 100%%; border-radius: var(--fa-radius); background: #000; }
  .fa-richtext { line-height: 1.6; }
  .fa-richtext h1, .fa-richtext h2, .fa-richtext h3 { margin: .6em 0 .3em; line-height: 1.25; }
  .fa-richtext h1 { font-size: 1.5rem; } .fa-richtext h2 { font-size: 1.25rem; } .fa-richtext h3 { font-size: 1.1rem; }
  .fa-richtext p { margin: .5em 0; } .fa-richtext ul { margin: .5em 0; padding-left: 1.25rem; }
  .fa-richtext code { font-family: ui-monospace, monospace; font-size: .9em;
                      background: var(--fa-card-border); padding: .1em .3em; border-radius: 4px; }
  .fa-richtext pre { background: var(--fa-card-border); padding: .7rem .9rem; border-radius: var(--fa-radius); overflow:auto; }
  .fa-richtext pre code { background: none; padding: 0; }
  .fa-richtext blockquote { margin: .5em 0; padding: .1em 0 .1em .9rem;
                            border-left: 3px solid var(--fa-border); color: var(--fa-muted); }
  /* heading: the browser's own scale per level IS the semantics, so nothing is
     imposed on it; only the margin, which a flex box/row would read as a gap. */
  .fa-heading { margin: 0; }
  .fa-badge { display: inline-flex; align-items: center; justify-content: center; min-width: 1.2rem; padding: 0 .4rem;
              font-size: .72rem; font-weight: 700; line-height: 1.5; border-radius: 999px;
              background: var(--fa-accent); color: #fff; }
  .fa-tabs { display: flex; flex-direction: column; gap: .6rem; }
  .fa-tabstrip { display: flex; flex-direction: row; gap: .25rem; border-bottom: 1px solid var(--fa-border); }
  .fa-tab { background: transparent; border: none; border-bottom: 2px solid transparent; border-radius: 0;
            color: var(--fa-muted); padding: .5rem .9rem; cursor: pointer; align-self: auto; font-weight: 600; }
  .fa-tab:hover { color: var(--fa-fg); }
  .fa-tab[aria-selected=true] { color: var(--fa-fg); border-bottom-color: var(--fa-accent); }
  .fa-form { display: flex; flex-direction: column; gap: .5rem; align-items: stretch; }
  .fa-use { display: contents; }
  /* overlay: a dimmed backdrop centering a card panel. */
  .fa-overlay-backdrop { position: fixed; inset: 0; background: rgba(0,0,0,.45);
    display: flex; align-items: center; justify-content: center; padding: 1rem; z-index: 50; }
  .fa-overlay-panel { background: var(--fa-bg); color: var(--fa-fg); border-radius: calc(var(--fa-radius) + 4px);
    border: 1px solid var(--fa-card-border); padding: 1.1rem 1.25rem; max-width: 32rem; width: 100%%;
    max-height: 85vh; overflow: auto; box-shadow: 0 12px 40px rgba(0,0,0,.3); }
  .fa-typeahead { width: 100%%; }
  /* controls: a textarea fills its row; a checkbox/toggle/radio option is a
     clickable label whose box sits before its words. */
  .fa-textarea { font: inherit; width: 100%%; min-height: 5rem; resize: vertical;
                 padding: .4rem .6rem; border: 1px solid var(--fa-border);
                 border-radius: var(--fa-radius); background: var(--fa-bg); color: var(--fa-fg); }
  .fa-checkbox, .fa-toggle, .fa-radio-option { display: inline-flex; gap: .45rem; align-items: center; cursor: pointer; }
  .fa-checkbox input, .fa-toggle input, .fa-radio-option input { accent-color: var(--fa-accent); }
  .fa-toggle input { width: 2.2rem; height: 1.2rem; }
  .fa-radio { display: flex; flex-direction: column; gap: .35rem; }
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
<style id="fa-css">%s</style>
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
