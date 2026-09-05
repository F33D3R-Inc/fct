package runtime

// Regions: the unit of data a client receives.
//
// The runtime used to treat `s.entities[name]` as the database. A page render
// read whole collections out of RAM, filtered and sorted them in Go, and then
// shipped those same collections to the browser in `fa-state` — so rendering
// twenty rows of a fifty-thousand-row table cost eight megabytes of HTML and a
// client-side copy of the entire table, including rows the viewer had no
// business seeing. No database works that way: data lives in pages, a query
// goes through an index, and what comes back is a *result set*.
//
// This file is that result set, in both directions:
//
//   - `listRows` resolves a `for` region through `Store.Query` — the node's
//     `where`/`by`/`limit` *is* a Query — instead of scanning the mirror.
//   - a render records what each region resolved to (`renderer.regions`), and
//     the page ships exactly those rows, addressed by the region's render path.
//     The client renders from them; it never receives a collection to filter.
//   - the value of every aggregate the render evaluated ships alongside them
//     (`materializer`), because a `count(...)` with no collection to scan would
//     otherwise re-render as 0 the moment the client took over.
//   - `POST /region` re-answers the page when a state cell a region's `where`
//     reads changes, which is the round trip that used to be free only because
//     the client held the whole table.
//
// The one thing that decides what an actor may receive of an entity row is
// `visibleRows`, and every path that hands rows to a client goes through it.
// The `@requires` field gate was previously applied in exactly one place (the
// JSON API), and the page path — a different producer of the same data — walked
// straight around it and served the gated field in plaintext. Two producers is
// how that happens; there is one now.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"facet/internal/ir"
)

// ── what an actor may receive ───────────────────────────────────────────────

// visibleRows is the single answer to "what may this actor receive of these
// rows". It drops every `@requires`-gated field whose read policy the scope's
// actor fails. The JSON API, the page bootstrap, the region endpoint and the
// live stream all call this — an entity row reaches a client through no other
// door, because a second, parallel filter is exactly what let `fa-state` serve
// what `GET /api/<Entity>` withheld.
//
// A nil/empty scope has no actor, so every gate fails and every gated field is
// dropped: that is the correct reading for the SSE stream, which fans one
// payload out to all subscribers and cannot authorize any of them.
func (s *Server) visibleRows(entity string, rows any, scope map[string]any) any {
	if len(s.gated[entity]) == 0 {
		return rows // no gated fields on this entity: nothing to decide, no copy
	}
	if scope == nil {
		scope = map[string]any{} // never nil: a policy with an aggregate writes its item var into the scope
	}
	return stripFields(rows, s.gateForActor(entity, scope))
}

// visibleRowList is visibleRows for a row slice, keeping the slice type so the
// render can go on using it.
func (s *Server) visibleRowList(entity string, rows []any, scope map[string]any) []any {
	out, _ := s.visibleRows(entity, any(rows), scope).([]any)
	return out
}

// ── list resolution: a `for` region is a Query ──────────────────────────────

// listPageSize is how many rows one store page fetches while a residual
// predicate is being applied. It is a page size, not a result cap: the loop
// keeps asking for pages until the node's `limit` is satisfied or the store is
// exhausted, so a filter the store cannot answer costs a scan — as it does in
// any database — but never a wrong (silently short) answer.
const listPageSize = 2000

// listRows resolves one `for` node to the rows it renders.
//
// The node already carries every part of a query — `Coll`, `Where`, `Order`,
// `Desc`, `Limit` — so it is compiled into one and handed to the store, which
// has the indexes. What the store cannot evaluate (a `contains(...)` substring
// test, a correlated `exists(...)`) stays behind as a residual predicate
// applied to each page as it arrives, the same split a planner draws between an
// index condition and a recheck filter.
//
// A `for` over a `[T]` state cell is not a table; it has no store to ask, so it
// keeps the in-memory path (`selectRows`) it always had.
// listRowsMore is listRows for a list that declares `more`: the rows, and
// whether the `limit` cut any off. The store answers `limit` rows and cannot
// say whether an eleventh existed, so the question is asked as `limit + 1`
// rows and the answer read off the count — the same pushdown, one row wider,
// which is the whole cost of knowing. The extra row is dropped before it can
// render. A list with no `more` (or no `limit`) is never cut off in a way
// anyone acts on, so it takes the plain path.
func (s *Server) listRowsMore(n ir.Node, scope map[string]any) ([]any, bool) {
	if n.More == "" || n.Limit == nil {
		return s.listRows(n, scope), false
	}
	limit := toInt(eval(n.Limit, scope))
	if limit <= 0 {
		return nil, false
	}
	probe := n
	probe.Limit = &ir.Expr{Kind: "lit", Val: limit + 1, VType: "int"}
	rows := s.listRows(probe, scope)
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

func (s *Server) listRows(n ir.Node, scope map[string]any) []any {
	ent, isEntity := s.entityByName(n.Coll)
	if !isEntity || s.store == nil {
		rows, _ := scope[n.Coll].([]any)
		return selectRows(rows, n, scope)
	}

	limit := 0
	if n.Limit != nil {
		limit = toInt(eval(n.Limit, scope))
		if limit <= 0 {
			return nil // `limit 0` asks for nothing; asking the store would be a page of rows nobody renders
		}
	}

	push, residual := s.splitPredicate(ent, n, scope)
	query := Query{Entity: n.Coll, Where: push, ItemVar: n.Var, Order: n.Order, Desc: n.Desc}

	// No residual: the store answers exactly the question, so one page of `limit`
	// rows is the whole render — the common case, and one round trip.
	if residual == nil && limit > 0 {
		query.Limit = limit
		rows, _, err := s.store.Query(query)
		if err != nil {
			return s.listFallback(n, scope, err)
		}
		return rows
	}

	// Residual (or unbounded) read: page the keyset cursor, applying what the
	// store could not, until the limit is met or the rows run out.
	query.Limit = listPageSize
	var out []any
	child := cloneScope(scope)
	mat := materializerOf(scope)
	for {
		rows, next, err := s.store.Query(query)
		if err != nil {
			return s.listFallback(n, scope, err)
		}
		// A correlated `count(...)`/`exists(...)` in the residual asks one question
		// per candidate row. Asked one at a time that is an N+1 across the network,
		// so the page's questions are hoisted into one grouped read first — the
		// same batch a rendered list's per-row counts get, answering exactly the
		// same queries, keyed the same way. Whatever it could not hoist falls to the
		// working set below rather than becoming a request per row.
		s.hoistResidual(mat, n.Var, residual, rows, scope)
		fanout := mat.setFanout(len(rows))
		for _, r := range rows {
			if residual != nil {
				child[n.Var] = r
				if !evalRowPredicate(residual, child) {
					continue
				}
			}
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				mat.setFanout(fanout)
				return out
			}
		}
		mat.setFanout(fanout)
		if next == "" {
			return out
		}
		query.After = next
	}
}

// hoistResidual batches the correlated cardinality questions one page of
// candidate rows is about to ask, so a residual `exists(...)`/`count(...)` costs
// one grouped read per page instead of one read per row.
//
// It answers only what `resolveAgg` would have asked for anyway, keyed by the
// same canonical query, so a hoisted answer and a per-row one are the same
// answer to the same question — the cache is a cache, never a different plan.
func (s *Server) hoistResidual(m *materializer, rowVar string, residual *ir.Expr, rows []any, scope map[string]any) {
	if m == nil || residual == nil || s.store == nil || len(rows) < 2 {
		return // one row asks one question; there is nothing to batch
	}
	for _, agg := range exprAggregates(residual, rowVar) {
		s.hoistAgg(m, agg, rowVar, rows, scope)
	}
}

// setFanout installs a new fanout and returns the previous one, so a caller can
// restore it. A nil materializer (an action, a policy, the API) has none.
func (m *materializer) setFanout(n int) int {
	if m == nil {
		return 0
	}
	prev := m.fanout
	m.fanout = n
	return prev
}

// listFallback answers a list from the in-memory working set when the store
// refused the read, and says so loudly. It is a degraded mode, not a design: a
// store that cannot answer a query the runtime believed it could push down is a
// bug in the pushdown split, and the page staying up is what makes it
// observable instead of fatal.
func (s *Server) listFallback(n ir.Node, scope map[string]any, err error) []any {
	s.obs.log.Error("list query failed; falling back to the in-memory working set",
		"entity", n.Coll, "error", err)
	rows, _ := scope[n.Coll].([]any)
	return selectRows(rows, n, scope)
}

// splitPredicate compiles a list's `where` into the part the store can evaluate
// and the part it cannot.
//
// Every conjunct is first partially evaluated against the render scope: a `for`
// filter reads the item *and* whatever is in scope around it (a route
// parameter, a state cell, `actor`), and the closed-over half is a constant at
// render time. `t.author == handle` is not a pushable predicate until `handle`
// becomes the literal "alice" — after which it is exactly the indexed equality
// FacetQL declared an index for.
//
// A conjunct that still does not compile (a call, an aggregate, a comparison
// between two outer values) is returned as the residual. Splitting on `&&`
// first matters: `a == 1 && contains(b, q)` pushes the sargable half down and
// rechecks only the other, rather than scanning the whole table for both.
//
// A @softdelete entity gets its `archived == false` conjunct here, because the
// store returns archived rows and only the mirror's loader used to hide them.
func (s *Server) splitPredicate(ent ir.Entity, n ir.Node, scope map[string]any) (push, residual *ir.Expr) {
	var conjuncts []*ir.Expr
	if ent.SoftDelete {
		conjuncts = append(conjuncts, &ir.Expr{Kind: "bin", Op: "==",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: n.Var}, Field: "archived"},
			R: &ir.Expr{Kind: "lit", Val: false, VType: "bool"}})
	}
	conjuncts = append(conjuncts, splitConj(foldOuter(n.Where, n.Var, scope))...)

	for _, c := range conjuncts {
		if pushable(typeLiterals(c, ent, n.Var), n.Var) {
			push = andExpr(push, typeLiterals(c, ent, n.Var))
		} else {
			residual = andExpr(residual, c)
		}
	}
	return push, residual
}

// splitConj flattens a predicate into its top-level `&&` conjuncts.
func splitConj(e *ir.Expr) []*ir.Expr {
	if e == nil {
		return nil
	}
	if e.Kind == "bin" && e.Op == "&&" {
		return append(splitConj(e.L), splitConj(e.R)...)
	}
	return []*ir.Expr{e}
}

// andExpr conjoins two predicates, either of which may be absent.
func andExpr(a, b *ir.Expr) *ir.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &ir.Expr{Kind: "bin", Op: "&&", L: a, R: b}
}

// foldOuter replaces every subexpression that does not mention the item
// variable with the literal it evaluates to in the surrounding scope. What
// survives is a predicate over the row alone — the only kind a store can serve.
//
// Only `bin` and `un` are descended into. Everything else that does mention the
// item (a call, an aggregate, an entity lookup) is left whole: folding inside it
// could not make it pushable, and rebuilding it would only risk changing what it
// means.
func foldOuter(e *ir.Expr, itemVar string, scope map[string]any) *ir.Expr {
	if e == nil {
		return nil
	}
	if !mentions(e, itemVar) {
		return litExpr(eval(e, scope))
	}
	switch e.Kind {
	case "bin":
		return &ir.Expr{Kind: "bin", Op: e.Op,
			L: foldOuter(e.L, itemVar, scope), R: foldOuter(e.R, itemVar, scope)}
	case "un":
		return &ir.Expr{Kind: "un", Op: e.Op, X: foldOuter(e.X, itemVar, scope)}
	}
	return e
}

// mentions reports whether an expression reads the named variable anywhere.
func mentions(e *ir.Expr, name string) bool {
	if e == nil {
		return false
	}
	if e.Kind == "ref" && e.Name == name {
		return true
	}
	if e.Name == name && (e.Kind == "agg" || e.Kind == "eget") {
		return true // the collection itself is named — treat as a read of it
	}
	for _, sub := range e.Kids() {
		if mentions(sub, name) {
			return true
		}
	}
	return false
}

// litExpr wraps a runtime value as a typed literal. The type comes from the Go
// value; `typeLiterals` re-types it against the column it is compared with,
// which is what keeps a text route parameter from being pushed down as text
// against an int column.
func litExpr(v any) *ir.Expr {
	switch t := v.(type) {
	case bool:
		return &ir.Expr{Kind: "lit", Val: t, VType: "bool"}
	case int, int64, float64:
		return &ir.Expr{Kind: "lit", Val: toInt(t), VType: "int"}
	}
	return &ir.Expr{Kind: "lit", Val: toStr(v), VType: "text"}
}

// typeLiterals re-types a folded literal to match the column it is compared
// against. `/post/:id` binds `id` as text, so `t.id == id` folds to a text
// literal — which the in-memory comparison accepted (it compares numerically)
// and a typed column would not. The comparison is retyped, not the column.
func typeLiterals(e *ir.Expr, ent ir.Entity, itemVar string) *ir.Expr {
	if e == nil {
		return nil
	}
	if e.Kind != "bin" {
		return e
	}
	if e.Op == "&&" || e.Op == "||" {
		return &ir.Expr{Kind: "bin", Op: e.Op,
			L: typeLiterals(e.L, ent, itemVar), R: typeLiterals(e.R, ent, itemVar)}
	}
	out := *e
	if f, ok := comparedField(e.L, ent, itemVar); ok && e.R != nil && e.R.Kind == "lit" {
		out.R = litFor(f, toStr(e.R.Val))
	}
	if f, ok := comparedField(e.R, ent, itemVar); ok && e.L != nil && e.L.Kind == "lit" {
		out.L = litFor(f, toStr(e.L.Val))
	}
	return &out
}

// comparedField resolves `item.field` to the field it names.
func comparedField(e *ir.Expr, ent ir.Entity, itemVar string) (ir.Field, bool) {
	if e == nil || e.Kind != "get" || e.Obj == nil || e.Obj.Kind != "ref" || e.Obj.Name != itemVar {
		return ir.Field{}, false
	}
	return fieldOf(ent, e.Field)
}

// pushable reports whether a predicate compiles to the pushed-down subset every
// Store promises to serve. It asks `exprSQL` rather than restating the rule,
// so "what can be pushed down" has one definition and cannot drift into a
// predicate the runtime believes is indexed and the store answers wrongly.
func pushable(e *ir.Expr, itemVar string) bool {
	if e == nil {
		return false
	}
	var args []any
	_, err := exprSQL(e, itemVar, &args)
	return err == nil
}

// ── materialized aggregates ─────────────────────────────────────────────────

// The client cannot evaluate an aggregate it has no rows for.
//
// `count(l in Like where l.tweet == t.id)` resolves by scanning a collection,
// and once collections stop crossing to the browser there is nothing to scan:
// every per-row count would re-render as 0 the moment the client took over. The
// answer is the same one Stage 1 gives for lists — send what the render
// computed, not what it computed it from. The server evaluates each aggregate
// once (against its in-memory working set, exactly as before) and ships the
// *value*, addressed by the render path it was evaluated at plus the position of
// that aggregate in the page's IR.
//
// Both sides number the aggregates by walking the same shipped IR in the same
// fixed order, so the address needs nothing shared between them — the same
// property that makes render paths work. A value the client cannot find falls
// back to scanning, which is what a policy or a client action (neither of which
// the server renders) still does.
//
// This does not change how an aggregate is *resolved*: the authority still
// counts rows in RAM, and that is why the mirror is still there. It changes only
// where the answer is computed — which is the whole of Stage 1.
type materializer struct {
	s     *Server
	index map[*ir.Expr]int // aggregate expression -> its position in this page's IR
	path  string           // the render path currently being evaluated
	out   map[string]any   // address -> value

	// counts caches cardinality reads by the question they answer (the canonical
	// pushed-down predicate), not by where they were asked. Two rows that pin the
	// same id ask the same question once, and a hoisted batch (prefetchCounts)
	// fills this in ahead of the render that will consume it.
	counts map[string]int
	// fanout is how many rows the innermost enclosing `for` is rendering. It is
	// the guard against turning one aggregate into a round trip per row: an
	// aggregate the batch could not hoist stays on the in-memory working set
	// rather than becoming an N+1 across the network.
	fanout int
	// unaddressed is the nesting depth of evaluations that have no render
	// address, and therefore no memo. See (*materializer).perRow.
	unaddressed int
}

// matKey is the address of one aggregate: where it was evaluated, and which
// aggregate it is.
func matKey(path string, i int) string { return path + "|" + strconv.Itoa(i) }

// materializerKey is the scope slot the render pass passes its collector in.
// `@` cannot start an identifier, so it can never collide with a state cell, an
// entity, or a bound variable — and clientState drops every `@` key that is not
// part of the wire contract.
const materializerKey = "@m"

// bindingsKey is the scope slot the render pass passes its page's top-level
// interpolations in, under the same `@` rule as materializerKey.
//
// A binding id (`b0`, `b1`, …) is an address on ONE page: the compiler mints
// them per page, starting at b0 every time, because they become `data-fa-bind`
// attributes in a document and only one page's document exists at a time. The
// client honours that — the bootstrap ships this page's `pg.Bindings` and
// nothing else — and this is how the server does.
//
// It used to hold one map for the whole app, built by folding every page's
// bindings together at boot. A union of a per-page namespace is not a union, it
// is a collision: the last page indexed won every id, so first paint evaluated
// some other page's expression in this page's scope. It rendered that page's
// value where the two happened to overlap and nothing at all where they did not
// — a route parameter the winning expression never mentions is simply absent —
// and the client then repainted it correctly on hydration, which is why it read
// as a mystery rather than as a bug with a cause.
const bindingsKey = "@b"

// pageBindings indexes one page's top-level interpolations by the id the
// renderers address them with.
func pageBindings(pg *ir.Page) map[string]*ir.Expr {
	if pg == nil {
		return nil
	}
	out := make(map[string]*ir.Expr, len(pg.Bindings))
	for i := range pg.Bindings {
		out[pg.Bindings[i].ID] = pg.Bindings[i].Expr
	}
	return out
}

// bindingExpr resolves a `data-fa-bind` id to the expression it stands for, in
// the page this render is rendering. An id the page does not have resolves to
// nothing rather than panicking: eval is nil-safe, and a missing binding is a
// blank, which is what the client does with the same miss.
func bindingExpr(scope map[string]any, id string) *ir.Expr {
	binds, _ := scope[bindingsKey].(map[string]*ir.Expr)
	return binds[id]
}

// perRow evaluates f with the path-addressed memo suspended, and returns what f
// returned.
//
// An aggregate's address is (render path, position in the page's IR). That pair
// identifies one *evaluation* only while the path advances with the bindings —
// which is true of the render walk, and false of a predicate applied to a set of
// candidate rows. A `for`'s `where` is evaluated once per candidate row at the
// list's single path, so `record` wrote row one's answer to that address and
// `lookup` handed it back for every row after it: `exists(...)` became whatever
// it was for the first row and `count(...)` likewise. A correlated sub-query in
// a `where` was, in effect, evaluated once and then not at all.
//
// Inside f the value has no address, so it is neither looked up nor recorded —
// it is computed for the row that is actually bound. The `counts` cache is
// deliberately still live: it is keyed by the canonical pushed-down query, which
// already carries the outer row's folded literals, so it is a cache of questions
// rather than of positions and stays correct here.
func (m *materializer) perRow(f func() any) any {
	if m == nil {
		return f()
	}
	m.unaddressed++
	defer func() { m.unaddressed-- }()
	return f()
}

// record notes an aggregate's value if this expression is one the client will
// re-evaluate. It returns the value unchanged so eval can tail into it.
func (m *materializer) record(e *ir.Expr, v any) any {
	if m == nil || m.unaddressed > 0 {
		return v
	}
	if i, ok := m.index[e]; ok {
		m.out[matKey(m.path, i)] = v
	}
	return v
}

// lookup returns a previously recorded value for this aggregate at the current
// path. Within one render the same aggregate at the same path is the same
// question, so this doubles as memoization: an aggregate is scanned once.
func (m *materializer) lookup(e *ir.Expr) (any, bool) {
	if m == nil || m.unaddressed > 0 {
		return nil, false
	}
	i, ok := m.index[e]
	if !ok {
		return nil, false
	}
	v, ok := m.out[matKey(m.path, i)]
	return v, ok
}

// evalRowPredicate answers a predicate for one row of a set the surrounding
// render addresses as a whole: the item variable is already bound in scope, and
// every aggregate the predicate reaches is evaluated for *this* row rather than
// read back from the address the row before it wrote.
//
// It is the single door for the two row-at-a-time filters — `listRows`'s
// residual and `selectRows` over a `[T]` cell — so the rule about what an
// aggregate address means lives in one place.
func evalRowPredicate(pred *ir.Expr, scope map[string]any) bool {
	return truthy(evalPerRow(pred, scope))
}

// evalPerRow evaluates an expression for one row of a set the surrounding render
// path does not distinguish — a filter's predicate, or the value an aggregate
// reduces over each row.
//
// The two used to be one call because a predicate was the only thing answered
// per row. A reduced expression is the second, and it needs exactly the same
// suppression: an aggregate or lookup nested inside it has no address of its
// own, so recording one would memoize the first row's answer and hand it to
// every row after it.
func evalPerRow(e *ir.Expr, scope map[string]any) any {
	m := materializerOf(scope)
	return m.perRow(func() any { return eval(e, scope) })
}

// materializerOf pulls the render pass's collector out of a scope. Actions,
// policies and the API evaluate with no collector and are unaffected.
func materializerOf(scope map[string]any) *materializer {
	m, _ := scope[materializerKey].(*materializer)
	return m
}

// aggIndex numbers every aggregate and entity-lookup expression a page's client
// will re-evaluate, in one fixed walk of the IR that page ships: the view tree
// (pre-order, and within a node: every interpolated value it carries — see
// ir.Node.SegLists — then cond, where, limit, val, a `use`'s args, then
// children), then the page's bindings, then every component's view.
// assets/facet.js walks the identical structure in the identical order.
//
// It is a pure function of the page, so it is computed once and cached.
// pageComponents is the set of components this page can actually render: the
// ones its view names with `use`, plus the ones those name, transitively.
//
// It exists because an app assembled from a facet library defines far more
// components than any one route renders. f33d3r.com defines 607 and its home
// page reaches a few dozen; shipping all of them put 323 KB of component trees
// in front of every visitor to render 21 KB of page. A component the route
// cannot reach is not a smaller download, it is a download of somebody else's
// page.
//
// The closure is exact rather than conservative because `use` is the only way a
// component is instantiated and it names its target statically — there is no
// dynamic dispatch to be unable to see through. A `use`'s own children are the
// slot content the caller passed, so they are walked too: that is where a card
// wrapping a button reaches the button.
//
// The order is the app's, and that matters more than the saving: assets/facet.js
// numbers a page's aggregates by walking the components it was shipped, and
// [Server.aggIndex] numbers them by walking this. Two walks over two different
// lists would hand the client an address for every aggregate after the first
// component, which is a wrong number rather than a missing one.
func (s *Server) pageComponents(pg *ir.Page) []ir.Component {
	s.compMu.Lock()
	defer s.compMu.Unlock()

	if cs, ok := s.pageComps[pg.Path]; ok {
		return cs
	}

	used := map[string]bool{}

	var nodes func([]ir.Node)
	nodes = func(list []ir.Node) {
		for i := range list {
			nd := &list[i]
			// Marked before the walk, not after: the compiler proves component use
			// acyclic, and this holds even if that ever stops being true.
			if nd.Kind == "use" && !used[nd.Name] {
				if c := s.byComponent[nd.Name]; c != nil {
					used[nd.Name] = true
					nodes(c.View)
				}
			}
			nodes(nd.Children)
		}
	}
	nodes(pg.View)

	out := make([]ir.Component, 0, len(used))
	for i := range s.ir.Components {
		if used[s.ir.Components[i].Name] {
			out = append(out, s.ir.Components[i])
		}
	}

	if s.pageComps == nil {
		s.pageComps = map[string][]ir.Component{}
	}
	s.pageComps[pg.Path] = out

	return out
}

func (s *Server) aggIndex(pg *ir.Page) map[*ir.Expr]int {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	if idx, ok := s.aggIdx[pg.Path]; ok {
		return idx
	}
	idx := map[*ir.Expr]int{}
	n := 0
	var expr func(*ir.Expr)
	expr = func(e *ir.Expr) {
		if e == nil {
			return
		}
		if e.Kind == "agg" || e.Kind == "eget" {
			idx[e] = n
			n++
		}
		for _, k := range e.Kids() {
			expr(k)
		}
	}
	var nodes func([]ir.Node)
	nodes = func(list []ir.Node) {
		for i := range list {
			nd := &list[i]
			for _, segs := range nd.SegLists() {
				for _, sg := range segs {
					expr(sg.Expr)
				}
			}
			expr(nd.Cond)
			expr(nd.Where)
			expr(nd.Limit)
			expr(nd.Val)
			// A `use` evaluates its arguments during the render, so they are
			// materialized. A button's or form's arguments are evaluated on dispatch,
			// which the server never renders — those keep their collection instead
			// (see clientColls).
			if nd.Kind == "use" {
				for _, a := range nd.Args {
					expr(a)
				}
			}
			nodes(nd.Children)
		}
	}
	nodes(pg.View)
	for i := range pg.Bindings {
		expr(pg.Bindings[i].Expr)
	}
	for _, c := range s.pageComponents(pg) {
		nodes(c.View)
	}
	if s.aggIdx == nil {
		s.aggIdx = map[string]map[*ir.Expr]int{}
	}
	s.aggIdx[pg.Path] = idx
	return idx
}

// ── the render pass ─────────────────────────────────────────────────────────

// renderer is one server-side render. It writes the HTML and, in the same pass,
// records what every list region resolved to, keyed by that region's render
// path — because those rows, and nothing wider, are what the client receives.
type renderer struct {
	s       *Server
	regions map[string][]any
	// more records, per region path, that a `for … more <action>` list had rows
	// held back by its `limit` — the one fact about a page the client cannot
	// recompute from the rows it was handed, and the fact its "More" control
	// exists on. Only lists that declare `more` have an entry.
	more map[string]bool
	mat  *materializer // collects the aggregate values the client cannot recompute
}

// newRenderer starts a render pass for one page and installs its aggregate
// collector into the scope the render evaluates against.
func (s *Server) newRenderer(pg *ir.Page, scope map[string]any) *renderer {
	mat := &materializer{s: s, index: s.aggIndex(pg), out: map[string]any{}, counts: map[string]int{}}
	scope[materializerKey] = mat
	scope[bindingsKey] = pageBindings(pg)
	return &renderer{s: s, regions: map[string][]any{}, more: map[string]bool{}, mat: mat}
}

// Render paths address what was rendered, not what exists.
//
// A region's id (`l0`, `t2`) only exists for a region at the top level of a
// page: the IR builder does not mint one for a list nested inside a `tabs`, a
// component or another `for`, because there is no single element to re-fill.
// Those lists still render rows, and the client still has to address them, so
// the key is the node's position in the tree that was actually rendered:
//
//	/2/0/1          the second root node's first child's second child
//	/2/0#17/1       ...inside the row with id 17 of an enclosing `for` over a table
//	/2/0/3#/1       ...inside the fourth row of a `for` over a `[T]` state cell
//
// The client walks the same IR with the same rules and computes the same
// string, so "which rows are these" needs no allocation of ids and no state
// shared between the two sides. Including the enclosing row's address is what
// keeps a nested list from colliding with its own siblings across rows —
// see listRowPath for what a row's address is.
func childPath(path string, i int) string { return path + "/" + strconv.Itoa(i) }

// rowPath appends a row's identity to a base path. A row that is a record is
// identified by its `id`; a row that is not — a `[text]` state cell's element —
// has no identity to append, which is why the caller folds the row's ordinal
// into the base first (listRowPath). It mirrors assets/facet.js's rowPath,
// including the `#` that is written even when there is no id: the two sides must
// produce the same bytes, and an address is not allowed to change shape.
func rowPath(base string, row any) string {
	id := ""
	if rec, ok := row.(record); ok {
		id = toStr(rec["id"])
	}
	return base + "#" + id
}

// listRowPath is the base path for row i of a `for`, and it is the whole of the
// rule — the client's fillList (assets/facet.js) branches on exactly this:
//
//   - a `for` over a `[T]` state cell folds the row's ordinal into the path
//     first, then appends the (usually empty) identity: `/2/0/3#`. Those rows
//     carry no id — a `[text]` cell's elements are plain strings — so addressing
//     them by identity alone would collapse every row of the list onto the one
//     address `/2/0#`. That is one region key and one aggregate slot for the
//     whole list, and since materializer.lookup doubles as memoization, row two
//     would render row one's `count(...)`.
//   - a `for` over an entity is a query, and its rows are records: the `id` is a
//     stable identity, which an ordinal is not — it survives the row moving
//     within the page, so a nested region keeps its address across a re-order.
//
// The branch is the client's branch, in the client's order: a name that is a
// state cell is a state cell here too, because `node.coll in stateType` is what
// the client tests first. Both halves must spell an address identically or the
// aggregate values and nested-region rows this render records are written to
// addresses the hydrated client never asks for.
func (s *Server) listRowPath(n ir.Node, path string, i int, row any) string {
	if s.isStateCell(n.Coll) {
		return rowPath(childPath(path, i), row)
	}
	return rowPath(path, row)
}

// isStateCell reports whether a name is one of the app's state cells. Every
// state cell ships in the page's IR (`reqIR.States` is sent whole), so this is
// exactly the client's `name in stateType` — including a server-placed or
// `@private` cell, which the client still knows the *name* of even though it
// never receives the value.
func (s *Server) isStateCell(name string) bool {
	for _, st := range s.ir.States {
		if st.Name == name {
			return true
		}
	}
	return false
}

// ── POST /region: re-answer one region ──────────────────────────────────────

// regionRequest is what the client asks for: the page it is on, the region it
// needs, and the client-held state to answer under. The state is the client's
// half of the store — a `where` that reads a search box can only be evaluated
// with the search box's current value, which the authority has never seen.
type regionRequest struct {
	Path string `json:"path"`
	// Key names one region; the reply then carries its rows under "rows". An empty
	// key asks only for the page's data as a whole ("regions" + "aggs").
	Key   string         `json:"key"`
	State map[string]any `json:"state"`
}

// handleRegion answers "what rows does this region hold, for this actor, under
// this client state" with rows — not HTML. The client's renderer stays
// authoritative over the DOM and the payload stays the size of a page of rows.
//
// It is the same authority the page render is, and deliberately so: it resolves
// the route the same way, refuses the same guard, evaluates against a scope
// built from the *server's* identity and entities (never the client's claim of
// them), and gates every row through `visibleRows`. A region endpoint that
// skipped any of that would be the page's authorization with a nicer URL and
// none of the checks.
//
// It answers by running the page's render and returning what the render
// collected. That costs one page render to serve one region, and buys the
// guarantee that a region's rows are computed in exactly one place: any second
// implementation of "resolve the region named by this key" would be a second
// definition of the query, free to drift from the one that painted the page.
// The other regions the render already resolved ride along in `regions`, so a
// state change that feeds several regions costs one round trip, not one each.
func (s *Server) handleRegion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// Rate-limited and CSRF-checked like every other authenticated POST: this
	// endpoint reads on behalf of the session cookie, so it must not be callable
	// as an ambient-authority request from another origin.
	if !s.guardMutation(w, r, true) {
		return
	}
	var req regionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pg, params := s.pageFor(req.Path)
	if pg == nil {
		http.Error(w, "unknown page", http.StatusNotFound)
		return
	}
	sid := s.session(w, r)

	store := s.fullStore(sid)
	for k, v := range params {
		store[k] = v
	}
	// The same binding handlePage makes: this endpoint answers by running that
	// page's render, so it must run it in that page's scope. `req.Path` is already
	// the value trusted to choose the page above, so this widens nothing.
	store["route"] = req.Path
	// The client's state is authoritative only over the cells the compiler placed
	// on the client. Anything else it sends — a server state cell, `actor`, `role`,
	// an entity collection — is ignored, so a forged body can widen a `where` but
	// never its own identity or the rows it is allowed to see.
	for _, st := range s.ir.States {
		if st.Placement != ir.Client {
			continue
		}
		if v, ok := req.State[st.Name]; ok {
			store[st.Name] = coerce(v, st.Type)
		}
	}
	// The route guard is the page's, enforced identically: a region of a page this
	// actor may not open is not readable a row at a time either.
	if pg.Requires != "" && !s.guardOK(pg.Requires, store) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	rd := s.newRenderer(pg, store)
	var discard strings.Builder
	for i, n := range pg.View {
		rd.node(&discard, n, store, childPath("", i))
	}
	out := map[string]any{"regions": rd.regions, "more": rd.more, "aggs": rd.mat.out}
	// `rows` answers the region that was named; `regions` and `aggs` carry
	// everything else this render resolved, so a state change that feeds several
	// regions — or that moves a count outside them — costs one round trip. A
	// request that names no region is asking for exactly that: the page's data
	// under this state, which is what a write to an entity invalidates.
	if req.Key != "" {
		rows, ok := rd.regions[req.Key]
		if !ok {
			http.Error(w, "unknown region", http.StatusNotFound)
			return
		}
		if rows == nil {
			rows = []any{}
		}
		out["rows"] = rows
	}
	// Rows that arrive here are rendered by facet.js, which cannot sign a media
	// reference: the signatures ride along, keyed by the value the row holds.
	if g := mediaGrants(out); len(g) > 0 {
		out["media"] = g
	}
	writeJSON(w, out)
}

// ── what still has to reach the client as a collection ──────────────────────

// collRead is what one entity is read for on the client: the set of fields its
// expressions touch, or `all` when a read could not be narrowed to fields (a
// bare reference to the collection, whose use we cannot see). It is the shape of
// the answer to "how little of this table does the browser actually need".
type collRead struct {
	all    bool
	fields map[string]bool
}

func (c *collRead) field(name string) {
	if c.all || name == "" {
		return
	}
	if c.fields == nil {
		c.fields = map[string]bool{}
	}
	c.fields[name] = true
}

// read returns (creating) the record for one entity.
func readOf(m map[string]*collRead, name string) *collRead {
	if c := m[name]; c != nil {
		return c
	}
	c := &collRead{}
	c.field("id") // identity is read by every aggregate that compares or joins rows
	m[name] = c
	return c
}

// clientColls names the entity collections this page's *client* evaluator reads
// as collections, and how much of each it reads.
//
// The client interprets the same IR the server does, and `count(...)`,
// `exists(...)`, `sum(...)` and `Entity(id).field` all resolve by scanning rows
// in scope. Those rows have to be there or every per-row count on the page
// re-renders as 0 the moment the client takes over. A `for` is deliberately not
// counted: its rows now arrive materialized, which is the entire point.
//
// What does cross is narrowed to the fields those expressions read — a
// `count(n in Notification where n.user == actor && !n.read)` needs `user` and
// `read`, not everybody's notification text — so the residue is as small as the
// analysis can prove it must be.
//
// This is the residue, and it is temporary. When FacetQL gains the predicated
// count primitive (AGENT_LOG §28, `POST /nodes/count`) an aggregate becomes a
// query like any other and this function returns nothing.
func (s *Server) clientColls(pg *ir.Page) map[string]*collRead {
	out := map[string]*collRead{}
	isEnt := map[string]bool{}
	for _, e := range s.ir.Entities {
		if !isReservedEntity(e.Name) {
			isEnt[e.Name] = true
		}
	}
	collect := func(e *ir.Expr) { collectReads(e, isEnt, out) }

	walkNodeExprs(pg.View, isEnt, out)
	for _, b := range pg.Bindings {
		collectBareRefs(b.Expr, isEnt, out)
	}
	for _, c := range s.ir.Components {
		walkNodeExprs(c.View, isEnt, out)
	}
	// Route guards run on the client too (it hides links to routes the actor may
	// not enter), so a policy reading a collection needs it.
	for _, rt := range s.ir.Routes {
		if pol := s.byPolicy[rt.Requires]; pol != nil {
			collect(pol.Expr)
		}
	}
	// Client-placed actions run entirely in the browser. Server-placed bodies are
	// *not* walked even though the client predicts optimistic ones: a prediction
	// is by definition provisional and the authority's reply replaces it, so it is
	// not a reason to ship a table.
	for _, a := range s.ir.Actions {
		if a.Placement != ir.Client {
			continue
		}
		for _, st := range a.Body {
			collect(st.Value)
			collect(st.Key)
			collect(st.Where)
			for _, f := range st.Fields {
				collect(f.Expr)
			}
			for _, arg := range st.Args {
				collect(arg)
			}
		}
	}
	return out
}

// walkNodeExprs collects the collection reads a *rendered* node tree still needs
// on the client.
//
// An aggregate in a view is not one of them: the render evaluates it and ships
// the value (see materializer), so the rows behind it never have to cross. What
// is left is what the server does not evaluate during a render — a button's or
// form's arguments, which are evaluated on dispatch — plus a typeahead's
// completion list and any bare reference to a collection, whose use we cannot
// see and therefore cannot narrow.
func walkNodeExprs(nodes []ir.Node, isEnt map[string]bool, out map[string]*collRead) {
	for _, n := range nodes {
		for _, segs := range n.SegLists() {
			for _, sg := range segs {
				collectBareRefs(sg.Expr, isEnt, out)
			}
		}
		collectBareRefs(n.Cond, isEnt, out)
		if n.Coll != "" && !isEnt[n.Coll] {
			// A repeat over a `[T]` state cell is filtered on the client (selectRows),
			// once per row — so an aggregate in its `where` is a read the client
			// performs and the render cannot answer for it. A per-row predicate has no
			// render address (see (*materializer).perRow), so there is no materialized
			// value to fall back on: the rows it scans have to be here.
			collectReads(n.Where, isEnt, out)
		} else {
			collectBareRefs(n.Where, isEnt, out)
		}
		collectBareRefs(n.Limit, isEnt, out)
		collectBareRefs(n.Val, isEnt, out)
		for _, a := range n.Args {
			if n.Kind == "use" {
				collectBareRefs(a, isEnt, out) // evaluated by the render → materialized
			} else {
				collectReads(a, isEnt, out) // button/form: evaluated on dispatch, here
			}
		}
		if n.Kind == "typeahead" && isEnt[n.Coll] {
			// The completion list is built by scanning the rows client-side, and it
			// reads exactly one field off each.
			readOf(out, n.Coll).field(n.Value)
		}
		walkNodeExprs(n.Children, isEnt, out)
	}
}

// collectBareRefs records only the collections named as plain values, where we
// cannot see what is done with them and so cannot prove any narrowing is safe.
// An aggregate's or lookup's own source is deliberately not recorded — the
// render ships its answer instead.
func collectBareRefs(e *ir.Expr, isEnt map[string]bool, out map[string]*collRead) {
	if e == nil {
		return
	}
	if e.Kind == "ref" && isEnt[e.Name] {
		readOf(out, e.Name).all = true
	}
	for _, sub := range e.Kids() {
		collectBareRefs(sub, isEnt, out)
	}
}

// collectReads records every entity an expression reads as a whole collection —
// an aggregate's source, an `Entity(id).field` lookup, or a bare reference — and
// which of its fields that read touches.
func collectReads(e *ir.Expr, isEnt map[string]bool, out map[string]*collRead) {
	if e == nil {
		return
	}
	switch e.Kind {
	case "agg":
		if isEnt[e.Name] {
			c := readOf(out, e.Name)
			c.field(e.Field) // sum/avg/min/max over a bare column read that column
			// …and over an expression read whatever that expression reads off the
			// row, exactly as the filter does. Missing these would ship the rows with
			// the summed columns projected away, and the client would total zeros.
			for _, f := range itemFieldsOf(e.Sel, e.Var) {
				c.field(f)
			}
			for _, f := range itemFieldsOf(e.Where, e.Var) {
				c.field(f)
			}
		}
	case "eget":
		if isEnt[e.Name] {
			readOf(out, e.Name).field(e.Field)
		}
	case "ref":
		if isEnt[e.Name] {
			// The collection is named as a value, and we cannot see what is done with
			// it. Nothing can be narrowed away safely, so nothing is.
			readOf(out, e.Name).all = true
		}
	}
	for _, sub := range e.Kids() {
		collectReads(sub, isEnt, out)
	}
}

// itemFieldsOf lists the fields an expression reads off one bound variable. A
// read of anything else in scope (the enclosing row of a `for`, a state cell) is
// not a read of this collection and must not widen it.
func itemFieldsOf(e *ir.Expr, itemVar string) []string {
	var out []string
	var walk func(*ir.Expr)
	walk = func(e *ir.Expr) {
		if e == nil {
			return
		}
		if e.Kind == "get" && e.Obj != nil && e.Obj.Kind == "ref" && e.Obj.Name == itemVar {
			out = append(out, e.Field)
		}
		for _, sub := range e.Kids() {
			walk(sub)
		}
	}
	walk(e)
	return out
}

// projectRows narrows rows to what a client may have of them: the fields its own
// evaluator reads (`keep`, nil meaning all of them) minus the @requires-gated
// fields this actor fails (`drop`). One pass, so the two rules cannot be applied
// in different places and disagree.
func projectRows(rows any, keep, drop map[string]bool) any {
	if keep == nil && len(drop) == 0 {
		return rows
	}
	list, ok := rows.([]any)
	if !ok {
		return rows
	}
	out := make([]any, len(list))
	for i, r := range list {
		rec, ok := r.(record)
		if !ok {
			out[i] = r
			continue
		}
		c := make(record, len(rec))
		for k, v := range rec {
			if drop[k] || (keep != nil && !keep[k]) {
				continue
			}
			c[k] = v
		}
		out[i] = c
	}
	return out
}

// streamEntities is the union of every page's client collection reads: what the
// live stream may carry, since one SSE payload reaches subscribers on every
// page. Everything outside it changes by announcement only — the client is told
// the entity changed and re-asks for the regions that read it.
//
// It is a pure function of the IR, so it is computed once and reused; a stream
// that recomputed it per connection would walk every page of every app on every
// subscribe.
func (s *Server) streamEntities() map[string]*collRead {
	s.streamOnce.Do(func() {
		s.streamEnts = map[string]*collRead{}
		for i := range s.ir.Pages {
			for name, c := range s.clientColls(&s.ir.Pages[i]) {
				cur := readOf(s.streamEnts, name)
				if c.all {
					cur.all = true
				}
				for f := range c.fields {
					cur.field(f)
				}
			}
		}
	})
	return s.streamEnts
}

// keepFields is the field filter for one collection read (nil = every field).
func (c *collRead) keepFields() map[string]bool {
	if c == nil || c.all {
		return nil
	}
	return c.fields
}

// entityKeys is the sorted-free list of names in a delta map, used to tell
// clients which entities changed without shipping any of them.
func entityKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── aggregates resolve through the store ────────────────────────────────────
//
// `count(...)` and `exists(...)` used to scan the in-memory mirror, which is why
// the mirror had to hold every row of every entity. FacetQL now answers both
// directly (`POST /nodes/count`, `POST /nodes/count_by`), so a render asks the
// database the question instead of reading the data and counting it here.
//
// The thing to get right is the *shape* of the asking. The same aggregate
// appears once per rendered row — `count(l in Like where l.tweet == t.id)` for
// twenty posts — and issuing one request per row is an N+1 across the network,
// which is the workaround this project's rules forbid. So a list region hoists:
// before it renders its rows it works out, for each aggregate under it, the one
// column the predicate pins per row, asks for all twenty answers in a single
// `CountBy`, and seeds the cache the render then reads. One round trip per
// aggregate shape, not per row. (Measured by the engine's own benchmark on 50 000
// Likes: twenty counts 13.7 ms, whole-kind grouping 153 ms, pinned values
// 0.93 ms.)
//
// An aggregate that cannot be hoisted and sits inside a multi-row list stays on
// the mirror. That is deliberate: falling back to a request per row would be
// exactly the fan-out the hoist exists to avoid, and a slower correct answer from
// RAM is better than a fast wrong architecture.

// aggQuery compiles an aggregate to the cardinality read that answers it, plus
// the canonical key that identifies the question. A predicate the store cannot
// push down has no query — the caller then falls back rather than guessing.
func (s *Server) aggQuery(e *ir.Expr, scope map[string]any) (Query, string, bool) {
	if e.Kind != "agg" || s.store == nil {
		return Query{}, "", false
	}
	ent, ok := s.entityByName(e.Name)
	if !ok {
		return Query{}, "", false
	}
	if !storeAnswers(e, ent) {
		return Query{}, "", false
	}
	itemVar := e.Var
	if itemVar == "" {
		itemVar = "item" // an unfiltered count(Entity) binds nothing
	}
	var pred *ir.Expr
	if ent.SoftDelete {
		// The store keeps archived rows; only the mirror's loader hid them. A count
		// that included them would differ from the list beside it.
		pred = &ir.Expr{Kind: "bin", Op: "==",
			L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: itemVar}, Field: "archived"},
			R: &ir.Expr{Kind: "lit", Val: false, VType: "bool"}}
	}
	for _, c := range splitConj(foldOuter(e.Where, itemVar, scope)) {
		c = typeLiterals(c, ent, itemVar)
		if !pushable(c, itemVar) {
			return Query{}, "", false
		}
		pred = andExpr(pred, c)
	}
	q := Query{Entity: e.Name, Where: pred, ItemVar: itemVar}
	return q, queryKey(q), true
}

// storeAnswers reports whether the store can answer this aggregate at all.
//
// `count`/`exists` reduce rows and need nothing from the schema. The other four
// reduce a column, so the column has to exist and has to be one the language's
// reducer folds — integer arithmetic, meaning `int` or `money`. A `min` over a
// text column is not refused here so much as left where it already is: the
// interpreter reduces it with toInt, which is not what any database would do,
// so pushing it down would make the answer depend on which path served it.
//
// Sel is the general reduced expression — `sum(l.qty * l.unitPrice in ...)`.
// That is a value computed per row, not a stored column, so there is nothing for
// a store to reduce and the mirror keeps it.
func storeAnswers(e *ir.Expr, ent ir.Entity) bool {
	switch e.Op {
	case "count", "exists":
		return true
	case "sum", "avg", "min", "max":
		if e.Sel != nil || e.Field == "" {
			return false
		}
		f, ok := fieldOf(ent, e.Field)
		return ok && numericField(f)
	}
	return false
}

// aggCacheKey identifies one answer in the materializer's cache: the question
// (the canonical pushed-down predicate) plus the reduction asked of it. The
// reduction is part of the key because a `sum` and the `count` beside it share a
// predicate and must not share a slot — and because `avg` reads both of them.
func aggCacheKey(question string, spec AggSpec) string {
	return question + "|" + spec.Func + "|" + spec.Field
}

// queryKey identifies a cardinality question. It is a canonical serialization of
// the pushed-down read, so two aggregates that ask the same thing — the same
// predicate reached from different rows, or the same count evaluated twice in one
// render — resolve to one request.
func queryKey(q Query) string {
	pred, err := json.Marshal(q.Where)
	if err != nil {
		pred = []byte("?")
	}
	return q.Entity + "|" + q.ItemVar + "|" + string(pred)
}

// resolveAgg answers an aggregate from the database rather than from the mirror,
// when it can do so without turning one aggregate into a round trip per row.
//
// `avg` is composed here rather than asked of the store, because the language
// defines it as sum ÷ count in integer arithmetic (reduceAgg) and that division
// must have exactly one implementation. Composing it costs a second cached
// lookup, not a second round trip per row: the hoist fills both slots.
func (m *materializer) resolveAgg(e *ir.Expr, scope map[string]any) (any, bool) {
	if m == nil || m.s == nil {
		return nil, false
	}
	q, key, ok := m.s.aggQuery(e, scope)
	if !ok {
		return nil, false
	}
	switch e.Op {
	case "count", "exists":
		n, ok := m.answer(q, key, AggSpec{})
		if !ok {
			return nil, false
		}
		if e.Op == "exists" {
			return n > 0, true
		}
		return n, true

	case "avg":
		total, ok := m.answer(q, key, AggSpec{Func: "sum", Field: e.Field})
		if !ok {
			return nil, false
		}
		n, ok := m.answer(q, key, AggSpec{})
		if !ok {
			return nil, false
		}
		if n == 0 {
			return 0, true // the empty average, exactly as reduceAgg has it
		}
		return total / n, true

	default: // sum | min | max
		return m.answer(q, key, AggSpec{Func: e.Op, Field: e.Field})
	}
}

// answer reads one reduction out of the cache, asking the store when the cache
// does not hold it and asking is not an N+1.
//
// An empty spec means the row count, which has its own typed call; anything else
// is a column reduction. Both go through here so the cache, the fan-out guard
// and the fall-back-to-the-mirror rule are written once.
func (m *materializer) answer(q Query, question string, spec AggSpec) (int, bool) {
	key := aggCacheKey(question, spec)
	if n, cached := m.counts[key]; cached {
		return n, true
	}
	// Not in the batch a list hoisted. Asking now is one request; inside a
	// multi-row list that is one request per row, so the mirror answers instead.
	if m.fanout > 1 {
		return 0, false
	}
	var n int
	var err error
	if spec.Func == "" {
		n, err = m.s.store.Count(q)
	} else {
		n, err = m.s.store.Aggregate(q, spec)
	}
	if err != nil {
		m.s.obs.log.Error("aggregate failed; falling back to the in-memory working set",
			"entity", q.Entity, "func", spec.Func, "field", spec.Field, "error", err)
		return 0, false
	}
	m.counts[key] = n
	return n, true
}

// prefetchCounts answers, in one request per aggregate, every question the rows
// about to be rendered will ask.
func (rd *renderer) prefetchCounts(n ir.Node, rows []any, scope map[string]any) {
	if rd.mat == nil || rd.s.store == nil || len(rows) < 2 {
		return // one row asks one question; there is nothing to batch
	}
	for _, agg := range rd.s.rowAggregates(n) {
		rd.s.hoistAgg(rd.mat, agg, n.Var, rows, scope)
	}
}

// exprAggregates collects the aggregates inside one expression whose predicate
// reads the named row variable — the ones whose answer changes with the outer
// row, and so the ones a batch can hoist. It is the one walk both the render's
// per-row aggregates and a residual predicate's are found by.
//
// Whether the store can actually answer a given one is decided later, in
// aggQuery: this walk only has the expression, not the entity it ranges over.
func exprAggregates(e *ir.Expr, rowVar string) []*ir.Expr {
	if e == nil {
		return nil
	}
	var out []*ir.Expr
	if e.Kind == "agg" && isAggOp(e.Op) && mentions(e.Where, rowVar) {
		out = append(out, e)
	}
	for _, sub := range e.Kids() {
		out = append(out, exprAggregates(sub, rowVar)...)
	}
	return out
}

// isAggOp reports whether an op names one of the language's aggregates.
func isAggOp(op string) bool {
	switch op {
	case "count", "exists", "sum", "avg", "min", "max":
		return true
	}
	return false
}

// rowAggregates collects the aggregates under a list node whose predicate reads
// the row — the ones that would otherwise be asked once per row.
// It descends into the components the body uses, because that is where a feed's
// per-row counts actually live (`use PostCard(..., count(...), ...)`).
func (s *Server) rowAggregates(n ir.Node) []*ir.Expr {
	var out []*ir.Expr
	seen := map[string]bool{}
	expr := func(e *ir.Expr) { out = append(out, exprAggregates(e, n.Var)...) }
	var nodes func([]ir.Node, int)
	nodes = func(list []ir.Node, depth int) {
		if depth > 8 {
			return // a defensive bound; the compiler already proves component use acyclic
		}
		for i := range list {
			nd := &list[i]
			for _, segs := range nd.SegLists() {
				for _, sg := range segs {
					expr(sg.Expr)
				}
			}
			expr(nd.Cond)
			expr(nd.Where)
			expr(nd.Limit)
			expr(nd.Val)
			for _, a := range nd.Args {
				expr(a)
			}
			if nd.Kind == "use" && !seen[nd.Name] {
				if comp := s.byComponent[nd.Name]; comp != nil {
					seen[nd.Name] = true
					nodes(comp.View, depth+1)
					seen[nd.Name] = false
				}
			}
			nodes(nd.Children, depth+1)
		}
	}
	nodes(n.Children, 0)
	return out
}

// hoistAgg turns one aggregate's per-row questions into one grouped request.
//
// It works from the concrete predicates rather than symbolically: each row's
// predicate is compiled exactly as `resolveAgg` would compile it, and the column
// to group by is the one equality whose literal is all that differs between them.
// Deriving the group that way is what guarantees the cached answers land under
// the keys the render will actually look up — a symbolic reconstruction could
// drift from the real compilation and quietly cache nothing.
func (s *Server) hoistAgg(m *materializer, e *ir.Expr, rowVar string, rows []any, scope map[string]any) {
	child := cloneScope(scope)
	preds := make([]*ir.Expr, 0, len(rows))
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		child[rowVar] = r
		q, key, ok := s.aggQuery(e, child)
		if !ok {
			return // one row the store cannot answer means the batch is not sound
		}
		preds = append(preds, q.Where)
		keys = append(keys, key)
	}
	itemVar := e.Var
	if itemVar == "" {
		itemVar = "item"
	}
	for _, field := range equalityFields(preds[0], itemVar) {
		rest, values, ok := splitPinned(preds, itemVar, field)
		if !ok {
			continue
		}
		q := Query{Entity: e.Name, Where: rest, ItemVar: itemVar}
		// The reductions this aggregate's answers are read from — exactly the
		// slots resolveAgg will look in, derived the same way, so the batch
		// cannot fill a key nothing reads. `avg` needs two, being sum ÷ count.
		for _, spec := range aggSpecs(e) {
			var grouped map[string]int
			var err error
			if spec.Func == "" {
				grouped, err = s.store.CountBy(q, field, distinct(values))
			} else {
				grouped, err = s.store.AggregateBy(q, spec, field, distinct(values))
			}
			if err != nil {
				s.obs.log.Error("grouped aggregate failed; falling back to the in-memory working set",
					"entity", e.Name, "group", field, "func", spec.Func, "error", err)
				return
			}
			for i, key := range keys {
				m.counts[aggCacheKey(key, spec)] = grouped[toStr(values[i])]
			}
		}
		return
	}
}

// aggSpecs lists the store reductions one aggregate's answer is composed from.
//
// One for `count`/`exists` (the empty spec, meaning the row count) and one for
// `sum`/`min`/`max`. `avg` is two, because the language defines it as sum ÷
// count — and the pair is listed here rather than assembled at each call site so
// that the hoist and resolveAgg cannot disagree about which slots exist.
func aggSpecs(e *ir.Expr) []AggSpec {
	switch e.Op {
	case "avg":
		return []AggSpec{{Func: "sum", Field: e.Field}, {}}
	case "sum", "min", "max":
		return []AggSpec{{Func: e.Op, Field: e.Field}}
	default: // count | exists
		return []AggSpec{{}}
	}
}

// equalityFields lists the item's columns a predicate pins with an equality —
// the candidates for the column a rendered page varies per row.
func equalityFields(pred *ir.Expr, itemVar string) []string {
	var out []string
	for _, c := range splitConj(pred) {
		if c.Kind != "bin" || c.Op != "==" {
			continue
		}
		for _, side := range [2]*ir.Expr{c.L, c.R} {
			if side != nil && side.Kind == "get" && side.Obj != nil &&
				side.Obj.Kind == "ref" && side.Obj.Name == itemVar {
				out = append(out, side.Field)
			}
		}
	}
	return out
}

// splitPinned checks that a set of per-row predicates differ in exactly one
// place — the literal pinned to `field` — and returns the predicate they share
// plus the per-row values. When the remainder differs between rows there is no
// single grouped read that answers them all, and it says so.
func splitPinned(preds []*ir.Expr, itemVar, field string) (rest *ir.Expr, values []any, ok bool) {
	var restKey string
	for i, pred := range preds {
		var mine *ir.Expr
		var others *ir.Expr
		for _, c := range splitConj(pred) {
			if v, isPin := pinnedValue(c, itemVar, field); isPin && mine == nil {
				mine = v
				continue
			}
			others = andExpr(others, c)
		}
		if mine == nil {
			return nil, nil, false // this row does not pin the column at all
		}
		key := queryKey(Query{ItemVar: itemVar, Where: others})
		if i == 0 {
			restKey, rest = key, others
		} else if key != restKey {
			return nil, nil, false // the rows differ in more than the pinned value
		}
		values = append(values, litValue(mine))
	}
	return rest, values, len(values) == len(preds)
}

// pinnedValue returns the literal a conjunct pins `item.field` to, if that is
// what the conjunct is.
func pinnedValue(c *ir.Expr, itemVar, field string) (*ir.Expr, bool) {
	if c == nil || c.Kind != "bin" || c.Op != "==" {
		return nil, false
	}
	isField := func(e *ir.Expr) bool {
		return e != nil && e.Kind == "get" && e.Field == field &&
			e.Obj != nil && e.Obj.Kind == "ref" && e.Obj.Name == itemVar
	}
	if isField(c.L) && c.R != nil && c.R.Kind == "lit" {
		return c.R, true
	}
	if isField(c.R) && c.L != nil && c.L.Kind == "lit" {
		return c.L, true
	}
	return nil, false
}

// distinct removes repeats: two rows that pin the same id are one question, and
// the engine caps how many values one grouped read may carry.
func distinct(values []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(values))
	for _, v := range values {
		k := toStr(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}
