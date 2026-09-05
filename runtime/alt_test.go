package runtime

import (
	"os"
	"strings"
	"testing"

	"facet/internal/ir"
)

// `alt` reaches the markup through the same escaper every other interpolated
// attribute does (attrtext_test.go proves that escaper cannot be left), so what
// is left to pin here is the pair of rules the two renderers have to agree on:
// which attribute each element gets, and what an empty value does to it.
func TestServerWritesAltOnImagesAndAriaLabelOnVideo(t *testing.T) {
	s := &Server{ir: &ir.IR{}}
	rd := &renderer{s: s, regions: map[string][]any{}, mat: &materializer{s: s, out: map[string]any{}, counts: map[string]int{}}}
	lit := func(v string) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: "text"} }

	cases := []struct {
		name, want string
		node       ir.Node
	}{{
		// The status quo for every image in four repositories: correct markup for
		// a decorative picture, which is why the omission has to be reported
		// rather than rendered differently.
		name: "no alt still writes an empty alt",
		node: ir.Node{Kind: "image", Segs: []ir.Seg{{Lit: "/a.png"}}},
		want: `<img class="fa-image" src="/a.png" alt="">`,
	}, {
		name: "a described image carries its description",
		node: ir.Node{Kind: "image", Segs: []ir.Seg{{Lit: "/a.png"}}, Alt: []ir.Seg{{Lit: "A chart"}}},
		want: `<img class="fa-image" src="/a.png" alt="A chart">`,
	}, {
		name: "an interpolated description cannot leave the attribute",
		node: ir.Node{Kind: "image", Segs: []ir.Seg{{Lit: "/a.png"}}, Alt: []ir.Seg{{Expr: lit(`" onerror="x`)}}},
		want: `<img class="fa-image" src="/a.png" alt="&#34; onerror=&#34;x">`,
	}, {
		// aria-label="" names nothing; it is worse than no attribute, because a
		// reader is told there is a name and handed an empty one.
		name: "a video with no alt gets no aria-label",
		node: ir.Node{Kind: "video", Segs: []ir.Seg{{Lit: "/c.mp4"}}},
		want: `<video class="fa-video" controls src="/c.mp4"></video>`,
	}, {
		name: "a named video gets an aria-label",
		node: ir.Node{Kind: "video", Segs: []ir.Seg{{Lit: "/c.mp4"}}, Alt: []ir.Seg{{Lit: "The build"}}},
		want: `<video class="fa-video" controls src="/c.mp4" aria-label="The build"></video>`,
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			rd.node(&b, c.node, map[string]any{}, "")
			if got := b.String(); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// SegLists and the client's segLists are the same list in the same ORDER: the
// two sides address aggregates by position in a walk that runs over it, so a
// list one side reads and the other skips shifts every number after it and every
// aggregate from there on resolves to another one's value. Adding `alt` to one
// and not the other is exactly that failure, so the order is asserted here on
// both sides rather than left to whether some test app happens to interpolate an
// aggregate into an alt.
func TestSegListsHasTheSameShapeOnBothSides(t *testing.T) {
	n := ir.Node{
		Segs:        []ir.Seg{{Lit: "segs"}},
		Label:       []ir.Seg{{Lit: "label"}},
		Placeholder: []ir.Seg{{Lit: "placeholder"}},
		PathSegs:    []ir.Seg{{Lit: "pathSegs"}},
		ClassSegs:   []ir.Seg{{Lit: "classSegs"}},
		Alt:         []ir.Seg{{Lit: "alt"}},
		Poster:      []ir.Seg{{Lit: "poster"}},
	}
	want := []string{"segs", "label", "placeholder", "pathSegs", "classSegs", "alt", "poster"}
	got := n.SegLists()
	if len(got) != len(want) {
		t.Fatalf("SegLists returned %d lists, want %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != 1 || got[i][0].Lit != want[i] {
			t.Fatalf("SegLists()[%d] is %v, want the %s list", i, got[i], want[i])
		}
	}

	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	mirror := "const out = [nd.segs, nd.label, nd.placeholder, nd.pathSegs, nd.classSegs, nd.alt, nd.poster];"
	if !strings.Contains(string(raw), mirror) {
		t.Errorf("assets/facet.js's segLists is no longer %q — it and ir.Node.SegLists\n"+
			"must list the same attributes in the same order", mirror)
	}
}

// The client's half of the alt rule, at the source that ships.
func TestClientAltMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	for _, want := range []string{
		`img.alt = attrText(node.alt, sc);`,
		`const name = attrText(node.alt, sc);`,
		`if (name) v.setAttribute("aria-label", name);`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the client half of the\n"+
				"alt rule has drifted from what this file checks", want)
		}
	}
}
