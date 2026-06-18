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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"facet/internal/ir"
)

//go:embed assets/facet.js
var clientJS []byte

// Server serves one compiled app.
type Server struct {
	ir       *ir.IR
	byAction map[string]*ir.Action
	byBind   map[string]*ir.Binding
	store    Store

	mu       sync.Mutex
	entities map[string][]any          // durable, shared (the in-memory working set; the Store is the source of truth)
	nextID   map[string]int            // per-entity id counter
	sessions map[string]map[string]any // sid -> per-session server (scalar) state
	actors   map[string]string         // sid -> actor identity (signed-in username, else "guest")
	roles    map[string]string         // sid -> actor role (auth)
	nextSID  int

	subsMu sync.Mutex           // guards subs
	subs   map[chan []byte]bool // live SSE connections (shared-state fan-out)
}

// New builds a server for a compiled IR, opening the Postgres entity store
// (FACET_DATABASE_URL) and loading its rows into the in-memory working set. It
// fails if the database cannot be reached — the app does not run without it.
func New(graph *ir.IR) (*Server, error) {
	s := &Server{
		ir:       graph,
		byAction: map[string]*ir.Action{},
		byBind:   map[string]*ir.Binding{},
		entities: map[string][]any{},
		nextID:   map[string]int{},
		sessions: map[string]map[string]any{},
		actors:   map[string]string{},
		roles:    map[string]string{},
		subs:     map[chan []byte]bool{},
	}
	for i := range graph.Actions {
		s.byAction[graph.Actions[i].Name] = &graph.Actions[i]
	}
	for i := range graph.Bindings {
		s.byBind[graph.Bindings[i].ID] = &graph.Bindings[i]
	}

	store, err := openStore(os.Getenv("FACET_DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	s.store = store
	loaded, err := store.Init(graph.Entities)
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("load entity data: %w", err)
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
	return s, nil
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
	mux.HandleFunc("/", s.handlePage)
	return mux
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
	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}()

	// Send a current snapshot of all entities so a client that connected after a
	// change cannot miss it (closes the page-load → stream-open race).
	s.mu.Lock()
	snap := map[string]any{}
	for ent, rows := range s.entities {
		if ent == reservedUserEntity {
			continue // never stream the credential store
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

// broadcast pushes a deltas payload to every live client (non-blocking; a client
// whose buffer is full is skipped — it will catch up on its next snapshot).
func (s *Server) broadcast(deltas map[string]any) {
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
	pg := s.pageFor(r.URL.Path)
	if pg == nil {
		http.NotFound(w, r)
		return
	}
	sid := s.session(w, r)
	store := s.fullStore(sid)

	var body strings.Builder
	for _, n := range pg.View {
		s.renderNode(&body, n, store)
	}

	// Ship the IR with this page's view/bindings/depgraph (the client runtime
	// reads those three fields), not every page.
	reqIR := *s.ir
	reqIR.View = pg.View
	reqIR.Bindings = pg.Bindings
	reqIR.DepGraph = pg.DepGraph
	reqIR.Pages = nil
	irJSON, _ := json.Marshal(&reqIR)
	stateJSON, _ := json.Marshal(store)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, page, html.EscapeString(s.ir.App), body.String(), irJSON, stateJSON)
}

// pageFor returns the view served at an exact path, or nil.
func (s *Server) pageFor(path string) *ir.Page {
	for i := range s.ir.Pages {
		if s.ir.Pages[i].Path == path {
			return &s.ir.Pages[i]
		}
	}
	return nil
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

	// permission gate — the authority's job.
	for _, pol := range act.Requires {
		if !truthy(eval(s.policyExpr(pol), scope)) {
			return nil, http.StatusForbidden, "forbidden: " + pol
		}
	}

	deltas := map[string]any{}
	entChanged := map[string]bool{}
	sess := s.sessions[sid]
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
			s.persist(s.store.Save(st.Entity, row))
			entChanged[st.Entity] = true
		case "set":
			key := eval(st.Key, scope)
			for _, r := range s.entities[st.Entity] {
				if m, ok := r.(record); ok && equal(m["id"], key) {
					m[st.Field] = eval(st.Value, scope)
					s.persist(s.store.Save(st.Entity, m))
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
					s.persist(s.store.Delete(st.Entity, key))
					entChanged[st.Entity] = true
					break
				}
			}
		case "clear":
			s.entities[st.Entity] = []any{}
			s.persist(s.store.Clear(st.Entity))
			entChanged[st.Entity] = true
		}
		// keep entity collections in scope fresh for later statements.
		for ent := range entChanged {
			scope[ent] = s.entities[ent]
		}
	}
	// Shared (entity) changes fan out to every live client over SSE — including
	// this one — so all tabs converge with no refresh. Per-session scalar deltas
	// stay private and ride back on this response. (Durability already happened,
	// row by row, through the Store above.)
	if len(entChanged) > 0 {
		entDeltas := map[string]any{}
		for ent := range entChanged {
			entDeltas[ent] = s.entities[ent]
		}
		s.broadcast(entDeltas)
	}
	return deltas, http.StatusOK, ""
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
// server state + the session actor. Client states are not here (the authority
// cannot see them; the compiler guarantees server actions never read them).
func (s *Server) scope(sid string) map[string]any {
	scope := map[string]any{"actor": s.actors[sid], "role": s.roles[sid]}
	for ent, rows := range s.entities {
		if ent == reservedUserEntity {
			continue // credentials never enter a render scope or reach a client
		}
		scope[ent] = rows
	}
	for k, v := range s.sessions[sid] {
		scope[k] = v
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

func (s *Server) policyExpr(name string) *ir.Expr {
	for i := range s.ir.Policies {
		if s.ir.Policies[i].Name == name {
			return s.ir.Policies[i].Expr
		}
	}
	return nil
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
		if e.Name == reservedUserEntity {
			continue // never expose the credential store
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
		ap := apiAction{Name: a.Name, Requires: a.Requires}
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
	if name == "" || strings.Contains(name, "/") || name == reservedUserEntity {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		rows, ok := s.entities[name]
		snapshot := append([]any{}, rows...)
		s.mu.Unlock()
		if !ok {
			http.Error(w, "unknown entity", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"rows": snapshot})

	case http.MethodPost:
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "deltas": deltas})

	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// ── jobs: scheduled server actions ────────────────────────────────────────────

// StartJobs launches the app's scheduled actions: each `on start` job runs once
// now, and each `every Ns` job runs on a ticker — all under a synthetic `system`
// session/actor on the authority. Entity changes they make fan out over SSE just
// like any other action. It returns immediately; tickers run in the background.
func (s *Server) StartJobs() {
	if len(s.ir.Jobs) == 0 {
		return
	}
	const systemSID = "__system"
	s.mu.Lock()
	s.sessions[systemSID] = s.newSessionState()
	s.actors[systemSID] = "system"
	s.mu.Unlock()

	for _, j := range s.ir.Jobs {
		act := s.byAction[j.Action]
		if act == nil {
			continue
		}
		if j.OnStart {
			s.runAction(systemSID, act, nil)
		}
		if j.Every > 0 {
			go func(a *ir.Action, every int) {
				t := time.NewTicker(time.Duration(every) * time.Second)
				defer t.Stop()
				for range t.C {
					s.runAction(systemSID, a, nil)
				}
			}(act, j.Every)
		}
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
	}
}

// ── sessions + persistence ───────────────────────────────────────────────────

func (s *Server) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("fa_sid"); err == nil {
		s.mu.Lock()
		_, ok := s.sessions[c.Value]
		s.mu.Unlock()
		if ok {
			return c.Value
		}
	}
	s.mu.Lock()
	s.nextSID++
	sid := fmt.Sprintf("s%d", s.nextSID)
	s.sessions[sid] = s.newSessionState()
	// Without auth, identity comes from `?as=` (a dev convenience). With auth, the
	// session starts as an anonymous guest until login.
	actor := "guest"
	if !s.ir.Auth {
		if as := r.URL.Query().Get("as"); as != "" {
			actor = as
		}
	}
	s.actors[sid] = actor
	s.roles[sid] = "guest"
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "fa_sid", Value: sid, Path: "/", HttpOnly: true})
	return sid
}

// newSessionState is a fresh per-session map of the server-placed state cells at
// their declared defaults.
func (s *Server) newSessionState() map[string]any {
	store := map[string]any{}
	for _, st := range s.ir.States {
		if st.Placement == ir.Server {
			store[st.Name] = eval(st.Init, map[string]any{})
		}
	}
	return store
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
	case "int":
		return toInt(v)
	case "bool":
		return truthy(v)
	default:
		return toStr(v)
	}
}

func zero(typ string) any {
	switch typ {
	case "int":
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
<style>
  body { font: 16px/1.5 system-ui, sans-serif; margin: 2.5rem auto; max-width: 34rem; color: #111; padding: 0 1rem; }
  .fa-box { display: flex; flex-direction: column; gap: .5rem; align-items: stretch; }
  .fa-box .fa-box { border: 1px solid #e3e3e3; border-radius: 10px; padding: .6rem .8rem; }
  .fa-text { font-variant-numeric: tabular-nums; }
  button { font: inherit; padding: .4rem .8rem; border: 1px solid #888; border-radius: 8px;
           background: #fff; cursor: pointer; align-self: flex-start; }
  button:active { transform: translateY(1px); }
  input { font: inherit; padding: .4rem .6rem; border: 1px solid #888; border-radius: 8px; }
  [data-fa-bind] { font-weight: 600; }
</style>
</head>
<body>
<div id="fa-root" data-fa-mount>%s</div>
<script type="application/json" id="fa-ir">%s</script>
<script type="application/json" id="fa-state">%s</script>
<script src="/facet.js"></script>
</body>
</html>
`
