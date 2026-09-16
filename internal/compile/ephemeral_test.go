package compile

import (
	"strings"
	"testing"
)

// @ephemeral (ast.Entity.Ephemeral / ir.Entity.Ephemeral) is the game-engine
// work's second primitive: an entity that is real to every query/action path
// but never durable — the runtime keeps it only in its in-memory working set
// (see runtime/server.go attachStore/commit). This proves it lowers correctly
// and leaves an ordinary entity's compiled shape untouched.
func TestEphemeralEntityCompiles(t *testing.T) {
	g := mustCompile(t, `app G:
    entity Position @ephemeral:
        id: int
        owner: text
        zone: int
        x: int
        y: int
    entity Player:
        id: int
        handle: text
        currentZone: int
    view Main at "/":
        text "hi"
`)
	var pos, player *entityLike
	for i := range g.Entities {
		e := &g.Entities[i]
		if e.Name == "Position" {
			pos = &entityLike{e.Name, e.Ephemeral}
		}
		if e.Name == "Player" {
			player = &entityLike{e.Name, e.Ephemeral}
		}
	}
	if pos == nil {
		t.Fatal("Position entity missing from compiled graph")
	}
	if !pos.ephemeral {
		t.Fatal("Position should compile with Ephemeral=true")
	}
	if player == nil {
		t.Fatal("Player entity missing from compiled graph")
	}
	if player.ephemeral {
		t.Fatal("Player (no @ephemeral) should compile with Ephemeral=false — it must default false, not inherit true from an unrelated entity")
	}
}

type entityLike struct {
	name      string
	ephemeral bool
}

// @ephemeral and @softdelete together are refused at build time: an
// ephemeral row has nothing durable to archive into, so the combination is a
// compile error, not a silent no-op on one of the two annotations.
func TestEphemeralAndSoftDeleteRejected(t *testing.T) {
	_, err := String(`app G:
    entity Position @ephemeral @softdelete:
        id: int
        x: int
    view Main at "/":
        text "hi"
`)
	if err == nil {
		t.Fatal("expected a build error combining @ephemeral and @softdelete, got none")
	}
	if !strings.Contains(err.Error(), "@ephemeral") || !strings.Contains(err.Error(), "@softdelete") {
		t.Fatalf("error should name both annotations, got: %v", err)
	}
}

// The zone-scoped "live broadcast" requirement composes @ephemeral with an
// ordinary `read:` policy — no separate subscription mechanism — reusing the
// same entity-lookup-in-read: shape already proven in apps/journal/data.fct
// (`read: Post(post).published || Post(post).author == actor`). This proves
// that shape also compiles for an @ephemeral entity, gated on the viewer's
// OWN row in another entity via `exists`.
func TestEphemeralEntitySupportsScopedReadPolicy(t *testing.T) {
	mustCompile(t, `app G:
    entity Player:
        id: int
        handle: text
        currentZone: int
    entity Position @ephemeral:
        id: int
        owner: text
        zone: int
        x: int
        y: int
        read: exists(p in Player where p.handle == actor && p.currentZone == zone)
    view Main at "/":
        text "hi"
`)
}
