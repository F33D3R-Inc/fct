package compile

import (
	"strings"
	"testing"

	"facet/internal/ast"
	"facet/internal/parser"
)

// `alt` is an interpolatable attribute like `placeholder`: same segments, same
// lowering, same escaper — and it must join SegLists, or it is invisible to
// every walk that asks what a node reads (which is exactly how a link's
// interpolated label and path were once invisible to all three of them).

func altApp(body string) string {
	return "app A:\n    entity Post:\n        id: int\n        cover: text\n        caption: text\n" +
		"    view Home at \"/\":\n        box:\n" + body + "\n"
}

func TestAltLowersToInterpolatedSegments(t *testing.T) {
	g, err := String(altApp(`            for p in Post:
                image "{p.cover}" alt "{p.caption}"`))
	if err != nil {
		t.Fatal(err)
	}
	img := g.Pages[0].View[0].Children[0].Children[0]
	if img.Kind != "image" {
		t.Fatalf("node kind = %q", img.Kind)
	}
	if len(img.Alt) != 1 || img.Alt[0].Expr == nil {
		t.Fatalf("alt = %#v, want one interpolated segment", img.Alt)
	}
	var found bool
	for _, segs := range img.SegLists() {
		if len(segs) == 1 && segs[0].Expr != nil && segs[0].Expr.Field == "caption" {
			found = true
		}
	}
	if !found {
		t.Error("Node.SegLists() does not include Alt — every dependency walk is blind to it")
	}
}

// A video has no `alt` attribute in HTML, but it has the same hole: a player
// with no accessible name. One keyword, one lowering, two attributes — the same
// argument `toggle` and `checkbox` are one kind for.
func TestVideoTakesTheSameAlt(t *testing.T) {
	g, err := String(altApp(`            video "/clip.mp4" alt "The build, start to finish"`))
	if err != nil {
		t.Fatal(err)
	}
	v := g.Pages[0].View[0].Children[0]
	if v.Kind != "video" || len(v.Alt) != 1 || v.Alt[0].Lit != "The build, start to finish" {
		t.Fatalf("video alt = %#v", v.Alt)
	}
}

// Absence and an explicit empty are DIFFERENT STATEMENTS. `alt ""` says the
// picture is decorative and a reader loses nothing by skipping it; saying
// nothing says only that nobody decided. Both render `alt=""`, because that is
// the correct markup for a picture with no description — so the difference is
// carried in the syntax tree and reported, not baked into the output.
func TestAnUnstatedAltIsAdvisedAndAnEmptyOneIsNot(t *testing.T) {
	src := `app A:
    view Home at "/":
        box:
            image "/a.png"
            image "/b.png" alt ""
            image "/c.png" alt "A chart"
            video "/d.mp4"
            upload bind u
            upload bind u2 label "Choose"
`
	// The two uploads are here to prove they are SILENT. An upload has no
	// preview element to describe, and its label already defaults to "Upload", so
	// the file input always has an accessible name — it is not the same hole.
	app, err := parser.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, a := range ast.Advise(app) {
		lines = append(lines, a.Line)
	}
	want := []int{4, 7} // the image with no alt, and the video
	if len(lines) != len(want) {
		t.Fatalf("advice on lines %v, want %v (%v)", lines, want, ast.Advise(app))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("advice on lines %v, want %v", lines, want)
		}
	}
	if msg := ast.Advise(app)[0].Msg; !strings.Contains(msg, `alt ""`) {
		t.Errorf("the advice must name the decorative spelling, got %q", msg)
	}
}

// An image inside a component, inside a wrapper, inside a loop is still an
// image — the walk has to reach every node that carries a body or it under-
// reports in exactly the files a component library keeps its pictures in.
func TestAdviceReachesNestedNodes(t *testing.T) {
	src := `app A:
    entity Post:
        id: int
        cover: text
    component Card(src: text):
        box:
            if src != "":
                image "{src}"
    view Home at "/":
        box:
            for p in Post:
                use Card(p.cover)
`
	app, err := parser.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	got := ast.Advise(app)
	if len(got) != 1 || got[0].Line != 8 {
		t.Fatalf("advice = %v, want one on line 8", got)
	}
}
