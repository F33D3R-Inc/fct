package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// `remove item in Entity where <cond>` — the filtered delete that powers unfollow
// (delete a row by a non-id key). It must lower to a `remove` stmt carrying the
// item variable + predicate, distinct from the by-id form.
const removeApp = `app R:
    auth
    entity Follow:
        id: int
        follower: text
        followee: text
    policy member:
        actor != "guest"
    action follow(who: text):
        requires member
        add Follow { follower: actor, followee: who }
    action unfollow(who: text):
        requires member
        remove f in Follow where f.follower == actor && f.followee == who
    action drop(id: int):
        requires member
        remove Follow(id)
    view Home at "/":
        box:
            text "{count(Follow)}"
`

func TestRemoveFilteredLowering(t *testing.T) {
	g, err := String(removeApp)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	stmt := func(action string) ir.Stmt {
		for _, a := range g.Actions {
			if a.Name == action {
				if len(a.Body) != 1 {
					t.Fatalf("%s: want 1 stmt, got %d", action, len(a.Body))
				}
				return a.Body[0]
			}
		}
		t.Fatalf("action %q not found", action)
		return ir.Stmt{}
	}

	// Filtered form: op remove, item var bound, predicate present, no by-id key.
	uf := stmt("unfollow")
	if uf.Op != "remove" || uf.Entity != "Follow" {
		t.Fatalf("unfollow: want remove on Follow, got op=%q entity=%q", uf.Op, uf.Entity)
	}
	if uf.Var != "f" {
		t.Errorf("unfollow: want item var f, got %q", uf.Var)
	}
	if uf.Where == nil {
		t.Error("unfollow: filtered remove lost its predicate")
	}
	if uf.Key != nil {
		t.Error("unfollow: filtered remove should have no by-id key")
	}

	// By-id form still lowers the old way: a key, no var/where.
	d := stmt("drop")
	if d.Op != "remove" || d.Key == nil {
		t.Fatalf("drop: want by-id remove with a key, got op=%q key=%v", d.Op, d.Key)
	}
	if d.Where != nil || d.Var != "" {
		t.Error("drop: by-id remove should carry no item var/predicate")
	}
}

// The filtered predicate is checked for purity like any read expression — an
// impure builtin in the `where` is a compile error.
func TestRemoveFilteredRejectsImpureWhere(t *testing.T) {
	_, err := String(`app R:
    entity Tick:
        id: int
        at: int
    action sweep():
        remove t in Tick where t.at < now()
    view H at "/":
        box:
            text "{count(Tick)}"
`)
	if err == nil || !strings.Contains(err.Error(), "pure") {
		t.Fatalf("want a purity error for now() in a remove filter, got %v", err)
	}
}
