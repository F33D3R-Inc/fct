package runtime

import (
	"testing"

	"facet/internal/compile"
)

// When `post` runs, its trigger fires `fanout`; `fanout` in turn triggers `tally`.
// One user action drives a synchronous chain of system-authority reactions — so a
// single `post` leaves one Post, one Notice (from fanout), and one Tally (from
// tally), proving reactions fire and chain.
const chainApp = `app Chain:
    entity Post:
        id: int
        body: text
    entity Notice:
        id: int
        msg: text
    entity Tally:
        id: int
        n: int
    action post(body: text):
        add Post { body: body }
    action fanout():
        add Notice { msg: "new post" }
    action tally():
        add Tally { n: 1 }
    on post -> fanout
    on fanout -> tally
    view Home at "/":
        box:
            text "{count(Post)}"
`

func TestTriggerChainFires(t *testing.T) {
	g, err := compile.String(chainApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}

	count := func(ent string) int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.entities[ent])
	}

	post := srv.byAction["post"]
	_, status, msg := srv.runAction("u1", post, []any{"hello"})
	if status != 200 {
		t.Fatalf("post: want 200, got %d (%s)", status, msg)
	}

	// post wrote one Post; its trigger ran fanout (one Notice); fanout's trigger ran
	// tally (one Tally). The whole chain completed synchronously.
	if got := count("Post"); got != 1 {
		t.Fatalf("Post: want 1, got %d", got)
	}
	if got := count("Notice"); got != 1 {
		t.Fatalf("fanout reaction should have written one Notice, got %d", got)
	}
	if got := count("Tally"); got != 1 {
		t.Fatalf("chained tally reaction should have written one Tally, got %d", got)
	}
}

// A reaction runs with system authority, so it passes an admin-gated policy even
// though the triggering action was run by an ordinary user.
const gatedReactionApp = `app Gated:
    auth
    entity Audit:
        id: int
        msg: text
    policy admins:
        role == "admin"
    action ping():
        add Audit { msg: "ping" }
    action record():
        requires admins
        add Audit { msg: "system recorded" }
    on ping -> record
    view Home at "/":
        box:
            text "{count(Audit)}"
`

func TestReactionRunsWithSystemAuthority(t *testing.T) {
	g, err := compile.String(gatedReactionApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}

	count := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.entities["Audit"])
	}

	// A guest session runs ping (open). Its reaction `record` requires admins —
	// which passes because reactions run under the system (admin) identity.
	_, status, msg := srv.runAction("guest1", srv.byAction["ping"], nil)
	if status != 200 {
		t.Fatalf("ping: want 200, got %d (%s)", status, msg)
	}
	if got := count(); got != 2 {
		t.Fatalf("want 2 Audit rows (ping + system record), got %d", got)
	}
}
