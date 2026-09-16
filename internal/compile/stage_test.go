package compile

import (
	"testing"

	"facet/internal/ir"
)

// stage/sprite (ast.Stage/ast.Sprite) is the canvas/tile/sprite scene node —
// the game-engine work's first primitive. It compiles to a "stage" node
// carrying Width/Height/Tiles and one "sprite" child per layer, each a
// Range (the same header a `for` carries) plus X/Y/Facing/Image/Label.
func TestStageCompiles(t *testing.T) {
	g := mustCompile(t, `app G:
    entity Piece:
        id: int
        x: int
        y: int
        facing: int
        name: text
        icon: text
    state background: text = "1,2,3"
    view Main at "/":
        stage width 400 height 300:
            tiles from background
            sprite for m in Piece at (m.x, m.y) facing m.facing image m.icon label "{m.name}"
`)
	k := kindCounts(g.View)
	if k["stage"] != 1 {
		t.Fatalf("view should contain one stage node, kinds=%v", k)
	}
	if k["sprite"] != 1 {
		t.Fatalf("view should contain one sprite node, kinds=%v", k)
	}

	var stage ir.Node
	walkNodes(g.View, func(n *ir.Node) {
		if n.Kind == "stage" {
			stage = *n
		}
	})
	if stage.Width != 400 || stage.Height != 300 {
		t.Fatalf("stage size = %dx%d, want 400x300", stage.Width, stage.Height)
	}
	if stage.Tiles == nil {
		t.Fatalf("stage.Tiles should be lowered from `tiles from background`")
	}
	if len(stage.Children) != 1 {
		t.Fatalf("stage should carry its sprite as a child, got %d children", len(stage.Children))
	}
	sp := stage.Children[0]
	if sp.Var != "m" || sp.Coll != "Piece" {
		t.Fatalf("sprite range = %s in %s, want m in Piece", sp.Var, sp.Coll)
	}
	if sp.X == nil || sp.Y == nil {
		t.Fatalf("sprite should lower both x and y expressions")
	}
	if sp.Facing == nil {
		t.Fatalf("sprite should lower an optional facing expression when written")
	}
	if sp.Image == nil {
		t.Fatalf("sprite should lower its (required) image expression")
	}
	if len(sp.Label) == 0 {
		t.Fatalf("sprite should lower its optional label segments when written")
	}

	// A stage's own region depends on the collection its sprite draws from — a
	// new/moved Piece must redraw it, the same way a `for`'s region depends on
	// its collection.
	if len(g.DepGraph["Piece"]) == 0 {
		t.Errorf("stage region should depend on Piece (sprite collection), deps=%v", g.DepGraph)
	}
	if len(g.DepGraph["background"]) == 0 {
		t.Errorf("stage region should depend on background (tiles source), deps=%v", g.DepGraph)
	}
}

// A sprite's `image` is mandatory; a stage with no tiles and no sprites is
// rejected at parse time (nothing to draw). Both are parser-level errors, so
// this only exercises the lowering-time checks: an unknown row field in a
// draw expression is still caught, exactly as it is in a `for`'s `where`.
func TestStageRejectsUnknownRowField(t *testing.T) {
	_, err := String(`app G:
    entity Piece:
        id: int
        x: int
        y: int
    view Main at "/":
        stage width 100 height 100:
            sprite for m in Piece at (m.x, m.y) image m.nope
`)
	if err == nil {
		t.Fatal("expected an unknown-field error for m.nope, got none")
	}
}
