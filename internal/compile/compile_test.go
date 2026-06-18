package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

func mustCompile(t *testing.T, src string) *ir.IR {
	t.Helper()
	g, err := String(src)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}
	return g
}

func find[T any](s []T, ok func(T) bool) (T, bool) {
	for _, v := range s {
		if ok(v) {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// The core claim: placement is inferred, not authored. A default cell is
// authoritative (server); an @client cell is ephemeral (client); and an
// action's domain is decided by its write set.
func TestPlacementInference(t *testing.T) {
	g := mustCompile(t, `
app Counter:
    state count: int = 0
    state bonus: int = 0 @client
    action increment:
        count = count + 1
    action addBonus:
        bonus = bonus + 1
    view Main:
        box:
            text "{count + bonus}"
`)
	st := func(name string) ir.State {
		v, _ := find(g.States, func(s ir.State) bool { return s.Name == name })
		return v
	}
	if st("count").Placement != ir.Server {
		t.Errorf("count: want server, got %s", st("count").Placement)
	}
	if st("bonus").Placement != ir.Client {
		t.Errorf("bonus: want client, got %s", st("bonus").Placement)
	}
	act := func(name string) ir.Action {
		v, _ := find(g.Actions, func(a ir.Action) bool { return a.Name == name })
		return v
	}
	if act("increment").Placement != ir.Server {
		t.Errorf("increment writes authoritative state -> want server, got %s", act("increment").Placement)
	}
	if act("addBonus").Placement != ir.Client {
		t.Errorf("addBonus writes only client state -> want client, got %s", act("addBonus").Placement)
	}
}

// A binding over two states depends on both, so a mutation to either patches it.
func TestDependencyGraph(t *testing.T) {
	g := mustCompile(t, `
app A:
    state count: int = 0
    state bonus: int = 0 @client
    action inc:
        count = count + 1
    view Main:
        box:
            text "{count}"
            text "{count + bonus}"
`)
	// count feeds two bindings (b0 and the b1 total); bonus feeds one (b1).
	if got := len(g.DepGraph["count"]); got != 2 {
		t.Errorf("count should feed 2 bindings, got %d (%v)", got, g.DepGraph["count"])
	}
	if got := len(g.DepGraph["bonus"]); got != 1 {
		t.Errorf("bonus should feed 1 binding, got %d (%v)", got, g.DepGraph["bonus"])
	}
}

// Placement soundness: the server authority cannot read ephemeral client state,
// so a server action that does is a compile error.
func TestServerActionCannotReadClientState(t *testing.T) {
	_, err := String(`
app Bad:
    state count: int = 0
    state cursor: int = 0 @client
    action mix:
        count = count + cursor
    view Main:
        box:
            text "{count}"
`)
	if err == nil {
		t.Fatal("expected a placement-soundness error, got nil")
	}
	if !strings.Contains(err.Error(), "client-only state") {
		t.Errorf("error should explain the client-state read, got: %v", err)
	}
}

// An action that mutates an entity is authoritative -> server, no matter what.
func TestEntityActionsArePlacedServer(t *testing.T) {
	g := mustCompile(t, `
app Board:
    entity Post:
        id: int
        body: text
    state draft: text = "" @client
    action post(body: text):
        add Post { body: body }
    action clearDraft:
        draft = ""
    view Main:
        box:
            for p in Post:
                text "{p.body}"
`)
	act := func(n string) ir.Action {
		v, _ := find(g.Actions, func(a ir.Action) bool { return a.Name == n })
		return v
	}
	if act("post").Placement != ir.Server {
		t.Errorf("post mutates an entity -> want server, got %s", act("post").Placement)
	}
	if act("clearDraft").Placement != ir.Client {
		t.Errorf("clearDraft writes only client state -> want client, got %s", act("clearDraft").Placement)
	}
	// the list over Post depends on the Post collection
	if len(g.DepGraph["Post"]) == 0 {
		t.Errorf("Post collection should feed the list region")
	}
}

// Policies gate authoritative actions; a client-placed action cannot be gated.
func TestRequiresForcesServer(t *testing.T) {
	_, err := String(`
app A:
    state x: int = 0 @client
    policy admin:
        actor == "admin"
    action go:
        requires admin
        x = 1
    view M:
        box:
            text "{x}"
`)
	if err == nil || !strings.Contains(err.Error(), "client-placed") {
		t.Fatalf("expected requires-on-client-action error, got: %v", err)
	}
}

// Button calls are arity-checked against the action signature.
func TestActionArityChecked(t *testing.T) {
	_, err := String(`
app A:
    entity Post:
        id: int
        body: text
    action like(id: int):
        set Post(id).body = "x"
    view M:
        box:
            button "like" -> like
`)
	if err == nil || !strings.Contains(err.Error(), "argument") {
		t.Fatalf("expected arity error, got: %v", err)
	}
}

// Two-way input requires a @client state (you cannot two-way bind authority).
func TestInputRequiresClientState(t *testing.T) {
	_, err := String(`
app A:
    state name: text = ""
    view M:
        box:
            input bind name
`)
	if err == nil || !strings.Contains(err.Error(), "client") {
		t.Fatalf("expected input/authoritative error, got: %v", err)
	}
}

// A policy used as a view condition is inlined to its predicate.
func TestPolicyInlinedInView(t *testing.T) {
	g := mustCompile(t, `
app A:
    policy admin:
        actor == "admin"
    action noop:
        requires admin
        clear Thing
    entity Thing:
        id: int
    view M:
        box:
            if admin:
                text "secret"
`)
	// the `if` cond should be the inlined comparison, not a bare ref.
	var found bool
	for _, n := range g.View[0].Children {
		if n.Kind == "if" && n.Cond != nil && n.Cond.Kind == "bin" && n.Cond.Op == "==" {
			found = true
		}
	}
	if !found {
		t.Errorf("policy `admin` should be inlined into the if condition as a comparison")
	}
}

// A derive is inlined wherever it is read, so a binding over it depends on the
// derive's *base* cells: changing either input patches the same binding.
func TestDeriveInlinedAndTracked(t *testing.T) {
	g := mustCompile(t, `
app Counter:
    state count: int = 0
    state bonus: int = 0 @client
    derive total: int = count + bonus
    action inc:
        count = count + 1
    view Main:
        box:
            text "{total}"
`)
	d, ok := find(g.Derives, func(d ir.Derive) bool { return d.Name == "total" })
	if !ok || d.Expr == nil || d.Expr.Kind != "bin" {
		t.Fatalf("total should be a derive lowered to a bin expr, got %+v", d)
	}
	// the binding reads `total`, which inlines to count+bonus -> both feed it.
	if len(g.DepGraph["count"]) != 1 || len(g.DepGraph["bonus"]) != 1 {
		t.Errorf("a binding over `total` should be fed by both count and bonus; got count=%v bonus=%v",
			g.DepGraph["count"], g.DepGraph["bonus"])
	}
}

// Aggregates range over an entity collection, so they track that collection: a
// mutation to the entity refreshes the count/sum.
func TestAggregateDepsAndPlacement(t *testing.T) {
	g := mustCompile(t, `
app Chirp:
    entity Post:
        id: int
        likes: int
    derive postCount: int = count(Post)
    derive totalLikes: int = sum(Post.likes)
    action like(id: int):
        set Post(id).likes = Post(id).likes + 1
    view Main:
        box:
            text "{postCount} posts, {totalLikes} likes"
            for p in Post:
                text "{p.likes}"
`)
	c, _ := find(g.Derives, func(d ir.Derive) bool { return d.Name == "postCount" })
	if c.Expr == nil || c.Expr.Kind != "agg" || c.Expr.Op != "count" || c.Expr.Name != "Post" {
		t.Errorf("postCount should be a count agg over Post, got %+v", c.Expr)
	}
	s, _ := find(g.Derives, func(d ir.Derive) bool { return d.Name == "totalLikes" })
	if s.Expr == nil || s.Expr.Op != "sum" || s.Expr.Field != "likes" {
		t.Errorf("totalLikes should be a sum agg over Post.likes, got %+v", s.Expr)
	}
	// Post feeds the two aggregate bindings and the list region.
	if len(g.DepGraph["Post"]) < 3 {
		t.Errorf("Post should feed both aggregate bindings and the list, got %v", g.DepGraph["Post"])
	}
}

// Soundness composes through derives: a server action that reads a derive which
// transitively reads client-only state is still a compile error.
func TestServerActionCannotReadDerivedClientState(t *testing.T) {
	_, err := String(`
app Bad:
    state count: int = 0
    state bonus: int = 0 @client
    derive total: int = count + bonus
    action mix:
        count = total
    view Main:
        box:
            text "{count}"
`)
	if err == nil || !strings.Contains(err.Error(), "client-only state") {
		t.Fatalf("expected derived-client-state soundness error, got: %v", err)
	}
}

// Aggregates must range over a real entity; sum needs an existing field.
func TestAggregateValidation(t *testing.T) {
	for _, src := range []string{
		"app A:\n    state n: int = 0\n    derive d: int = count(n)\n    view M:\n        box:\n            text \"{d}\"",
		"app A:\n    entity Post:\n        id: int\n    derive d: int = sum(Post.nope)\n    view M:\n        box:\n            text \"{d}\"",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected an aggregate validation error for:\n%s", src)
		}
	}
}

// `requires` accepts policies, not derives.
func TestRequiresRejectsDerive(t *testing.T) {
	_, err := String(`
app A:
    entity Thing:
        id: int
    derive d: int = count(Thing)
    action go:
        requires d
        clear Thing
    view M:
        box:
            text "{d}"
`)
	if err == nil || !strings.Contains(err.Error(), "derive") {
		t.Fatalf("expected requires-a-derive error, got: %v", err)
	}
}

// An entity field may be typed as another entity (a relation), read back with a
// nested lookup. An unknown entity type is a compile error.
func TestEntityRelations(t *testing.T) {
	g := mustCompile(t, `
app Mail:
    entity User:
        id: int
        name: text
    entity Message:
        id: int
        to: User
        body: text
    action send(u: int, b: text):
        add Message { to: u, body: b }
    view M:
        box:
            for m in Message:
                text "{User(m.to).name}: {m.body}"
`)
	msg, _ := find(g.Entities, func(e ir.Entity) bool { return e.Name == "Message" })
	to, _ := find(msg.Fields, func(f ir.Field) bool { return f.Name == "to" })
	if to.Type != "User" {
		t.Errorf("Message.to should be a User relation, got %q", to.Type)
	}
	if _, err := String("app B:\n    entity M:\n        id: int\n        to: Ghost\n    view V:\n        box:\n            text \"x\""); err == nil {
		t.Error("expected an unknown-entity-type error for a dangling relation")
	}
}

// An effectful builtin (now/rand) makes an action impure, so the placement
// calculus pins it to the server — and impurity is rejected in pure contexts.
func TestEffectfulBuiltinsPlacementAndPurity(t *testing.T) {
	g := mustCompile(t, `
app Clock:
    entity Tick:
        id: int
        at: int
    action mark:
        add Tick { at: now() }
    view M:
        box:
            text "{count(Tick)}"
`)
	mark, _ := find(g.Actions, func(a ir.Action) bool { return a.Name == "mark" })
	if mark.Placement != ir.Server {
		t.Errorf("an action using now() must be server-placed, got %s", mark.Placement)
	}
	// impurity is forbidden in pure contexts (derives, policies, views).
	for _, src := range []string{
		"app A:\n    derive d: int = now()\n    view M:\n        box:\n            text \"{d}\"",
		"app A:\n    state x: int = 0\n    view M:\n        box:\n            text \"{now()}\"",
		"app A:\n    entity T:\n        id: int\n    action a:\n        add T { id: now(1) }\n    view M:\n        box:\n            text \"x\"",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected an impurity/arity error for:\n%s", src)
		}
	}
}

// Soundness is symmetric: a server action (here forced server by now()) cannot
// write ephemeral client-only state.
func TestServerActionCannotWriteClientState(t *testing.T) {
	_, err := String(`
app A:
    state t: int = 0 @client
    action stamp:
        t = now()
    view M:
        box:
            text "{t}"
`)
	if err == nil || !strings.Contains(err.Error(), "client-only state") {
		t.Fatalf("expected server-writes-client soundness error, got: %v", err)
	}
}

// Jobs compile to scheduled, server-placed, zero-arg actions.
func TestJobs(t *testing.T) {
	g := mustCompile(t, `
app J:
    entity Log:
        id: int
    action rotate:
        clear Log
    job nightly every 1h -> rotate
    view M:
        box:
            text "{count(Log)}"
`)
	j, ok := find(g.Jobs, func(j ir.Job) bool { return j.Name == "nightly" })
	if !ok || j.Action != "rotate" || j.Every != 3600 {
		t.Fatalf("nightly job should run rotate every 3600s, got %+v", j)
	}
	// a job must name a zero-arg, server-placed action.
	for _, src := range []string{
		"app A:\n    state x: int = 0 @client\n    action c:\n        x = 1\n    job j every 1s -> c\n    view M:\n        box:\n            text \"{x}\"",
		"app A:\n    entity L:\n        id: int\n    action p(n: int):\n        clear L\n    job j on start -> p\n    view M:\n        box:\n            text \"x\"",
		"app A:\n    entity L:\n        id: int\n    job j every 1s -> nope\n    view M:\n        box:\n            text \"x\"",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected an invalid-job error for:\n%s", src)
		}
	}
}

func findNode(nodes []ir.Node, kind string) (ir.Node, bool) {
	for _, n := range nodes {
		if n.Kind == kind {
			return n, true
		}
		if c, ok := findNode(n.Children, kind); ok {
			return c, true
		}
	}
	return ir.Node{}, false
}

// `for` is the query primitive: where filters, by orders, limit caps — and a
// state the filter reads feeds the list region so it re-queries reactively.
func TestForQuery(t *testing.T) {
	g := mustCompile(t, `
app Feed:
    entity Post:
        id: int
        likes: int
    state minLikes: int = 0 @client
    view M:
        box:
            for p in Post where p.likes >= minLikes by likes desc limit 2:
                text "{p.likes}"
`)
	list, ok := findNode(g.View, "list")
	if !ok {
		t.Fatal("no list node produced")
	}
	if list.Where == nil || list.Where.Kind != "bin" || list.Where.Op != ">=" {
		t.Errorf("where should lower to a >= comparison, got %+v", list.Where)
	}
	if list.Order != "likes" || !list.Desc || list.Limit != 2 {
		t.Errorf("order/desc/limit wrong: order=%q desc=%v limit=%d", list.Order, list.Desc, list.Limit)
	}
	// the filter reads minLikes, so changing it must refresh the list region.
	if len(g.DepGraph["minLikes"]) == 0 {
		t.Errorf("minLikes (read by the where filter) should feed the list region; deps=%v", g.DepGraph["minLikes"])
	}
	// malformed clauses are rejected.
	for _, src := range []string{
		"app A:\n    entity P:\n        id: int\n    view M:\n        box:\n            for p in P limit abc:\n                text \"{p.id}\"",
		"app A:\n    entity P:\n        id: int\n    view M:\n        box:\n            for p in P where:\n                text \"{p.id}\"",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected a malformed-for error for:\n%s", src)
		}
	}
}

// A `where` filter must be pure — it runs on both server and client.
func TestForWhereMustBePure(t *testing.T) {
	_, err := String(`
app A:
    entity P:
        id: int
        n: int
    view M:
        box:
            for p in P where p.n > rand(10):
                text "{p.n}"
`)
	if err == nil || !strings.Contains(err.Error(), "effectful") {
		t.Fatalf("expected an impurity error in a where filter, got: %v", err)
	}
}

// `auth` turns on the built-in identity: a managed user entity, the
// signup/login/logout actions, and the `role` builtin in policies.
func TestAuth(t *testing.T) {
	g := mustCompile(t, `
app A:
    auth
    entity Post:
        id: int
    policy admin:
        role == "admin"
    action wipe:
        requires admin
        clear Post
    view M:
        box:
            if actor != "guest":
                text "hi {actor}"
            button "out" -> logout
`)
	if !g.Auth {
		t.Error("auth flag should be set")
	}
	for _, n := range []string{"signup", "login", "logout"} {
		if _, ok := find(g.Actions, func(a ir.Action) bool { return a.Name == n }); !ok {
			t.Errorf("auth should inject the %q action", n)
		}
	}
	if _, ok := find(g.Entities, func(e ir.Entity) bool { return e.Name == "FacetUser" }); !ok {
		t.Error("auth should inject the managed FacetUser entity")
	}
	// reserved names can't be redefined under auth.
	for _, src := range []string{
		"app A:\n    auth\n    entity X:\n        id: int\n    action login(u: text):\n        clear X\n    view M:\n        box:\n            text \"x\"",
		"app A:\n    auth\n    entity FacetUser:\n        id: int\n    view M:\n        box:\n            text \"x\"",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected a reserved-name error for:\n%s", src)
		}
	}
}

// Each view is a page at a route; links must point at real routes; routes are
// unique.
func TestPagesAndLinks(t *testing.T) {
	g := mustCompile(t, `
app A:
    view Home at "/":
        box:
            link "about" -> "/about"
    view About at "/about":
        box:
            text "about"
            link "home" -> "/"
`)
	if len(g.Pages) != 2 {
		t.Fatalf("want 2 pages, got %d", len(g.Pages))
	}
	routes := map[string]string{}
	for _, p := range g.Pages {
		routes[p.Path] = p.Name
	}
	if routes["/"] != "Home" || routes["/about"] != "About" {
		t.Errorf("routes wrong: %v", routes)
	}
	// a default route is derived from the view name when `at` is omitted.
	g2 := mustCompile(t, "app A:\n    view Main:\n        box:\n            text \"a\"\n    view Profile:\n        box:\n            text \"b\"")
	r2 := map[string]bool{}
	for _, p := range g2.Pages {
		r2[p.Path] = true
	}
	if !r2["/"] || !r2["/profile"] {
		t.Errorf("default routes wrong: %v", r2)
	}
	// dangling link and duplicate route are errors.
	if _, err := String("app A:\n    view H at \"/\":\n        box:\n            link \"x\" -> \"/nope\""); err == nil {
		t.Error("expected a dangling-link error")
	}
	if _, err := String("app A:\n    view H at \"/\":\n        box:\n            text \"a\"\n    view G at \"/\":\n        box:\n            text \"b\""); err == nil {
		t.Error("expected a duplicate-route error")
	}
}

func TestUnknownReferences(t *testing.T) {
	for _, src := range []string{
		"app A:\n    state x: int = 0\n    action go:\n        x = x + missing\n    view M:\n        box:\n            text \"{x}\"",
		"app A:\n    state x: int = 0\n    view M:\n        box:\n            text \"{y}\"",
		"app A:\n    state x: int = 0\n    view M:\n        box:\n            button \"go\" -> nope",
	} {
		if _, err := String(src); err == nil {
			t.Errorf("expected error for:\n%s", src)
		}
	}
}
