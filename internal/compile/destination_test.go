package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// findLinks collects every link node in a compiled app, in render order.
func findLinks(nodes []ir.Node) []ir.Node {
	var out []ir.Node
	for _, n := range nodes {
		if n.Kind == "link" {
			out = append(out, n)
		}
		out = append(out, findLinks(n.Children)...)
	}
	return out
}

// A destination is now one of four things, and this pins which is which. The
// distinction that carries the safety property is literal-vs-value: an author's
// absolute URL leaves the app, a computed one still may not.
func TestTheFormsADestinationCanTake(t *testing.T) {
	src := `app A:
    state repo: text = "fct" @client
    view V at "/":
        link "GitHub" -> "https://github.com/F33D3R-Inc/facetql"
        link "Repo" -> "https://github.com/F33D3R-Inc/{repo}"
        link "Mail" -> "mailto:hi@f33d3r.com"
        link "Plain" -> "http://example.com"
        link "Install" -> "#install"
        link "Docs install" -> "/docs#install"
        box anchor "install":
            text "Install"
    view Docs at "/docs":
        text "docs"
`
	app, err := String(src)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}

	links := findLinks(app.Pages[0].View)
	if len(links) != 6 {
		t.Fatalf("got %d links, want 6", len(links))
	}

	for i, want := range []bool{true, true, true, true, false, false} {
		if links[i].External != want {
			t.Errorf("link %d (%q): External = %v, want %v",
				i, links[i].Path, links[i].External, want)
		}
		// No destination the author wrote is ever a route expression: those are the
		// runtime-value form, and the checks that follow from that are different.
		if links[i].Route {
			t.Errorf("link %d (%q) was classified as a route expression", i, links[i].Path)
		}
	}

	// The interpolated external keeps the segments (so the value is escaped into
	// its path segment), and its literal prefix is the author's origin.
	if links[1].Path != "" || len(links[1].PathSegs) == 0 {
		t.Errorf("interpolated external link kept a flat path %q", links[1].Path)
	}
	if got := links[1].PathSegs[0].Lit; got != "https://github.com/F33D3R-Inc/" {
		t.Errorf("interpolated external link's literal origin = %q", got)
	}

	// The anchor is the author's name, on the element, and is NOT the runtime's
	// region id — the two live in different fields on purpose.
	var box ir.Node
	for _, n := range app.Pages[0].View {
		if n.Anchor != "" {
			box = n
		}
	}
	if box.Anchor != "install" {
		t.Fatalf("no node carries the author's anchor; got %+v", box.Anchor)
	}
	if box.ID == box.Anchor {
		t.Errorf("the author's anchor was written into the runtime's region id")
	}
}

// Every refusal, including the ones that existed before external links and must
// survive them. The `javascript:`/`data:` cases are the reason the scheme set is
// an allowlist: they fail at COMPILE time for a literal, not at render.
func TestTheBoundariesOfALinkDestination(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a javascript: destination is a compile error",
		src: `app A:
    view V at "/":
        link "x" -> "javascript:alert(1)"
`,
		want: `uses the "javascript" scheme`,
	}, {
		name: "a data: destination is a compile error",
		src: `app A:
    view V at "/":
        link "x" -> "data:text/html,<script>alert(1)</script>"
`,
		want: `uses the "data" scheme`,
	}, {
		name: "an external destination may not interpolate its host",
		src: `app A:
    state h: text = "x" @client
    view V at "/":
        link "x" -> "https://{h}/a"
`,
		want: "interpolates its host",
	}, {
		name: "a protocol-relative destination is not a path",
		src: `app A:
    view V at "/":
        link "x" -> "//evil.com/x"
`,
		want: "is protocol-relative",
	}, {
		name: "an internal link to a route no view serves is still a compile error",
		src: `app A:
    view V at "/":
        link "x" -> "/nope"
`,
		want: "no view serves that route",
	}, {
		name: "the path half of an anchor destination is still route-checked",
		src: `app A:
    view V at "/":
        link "x" -> "/nope#install"
        box anchor "install":
            text "i"
`,
		want: "no view serves that route",
	}, {
		name: "a link to an anchor nothing declares is a compile error",
		src: `app A:
    view V at "/":
        link "x" -> "#nosuch"
        box anchor "install":
            text "i"
`,
		want: "no node declares that anchor",
	}, {
		name: "an anchor destination may not be computed",
		src: `app A:
    state s: text = "a" @client
    view V at "/":
        link "x" -> "#{s}"
        box anchor "install":
            text "i"
`,
		want: "is not an anchor name",
	}, {
		name: "an anchor name is restricted to what a fragment, an id and a selector share",
		src: `app A:
    view V at "/":
        box anchor "not an id":
            text "i"
`,
		want: "invalid anchor",
	}, {
		name: "a destination that is neither a path nor a URL nor a whole route",
		src: `app A:
    view V at "/":
        link "x" -> "docs"
`,
		want: "must start with `/`",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("expected a compile error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a compile error containing %q, got: %v", c.want, err)
			}
		})
	}
}

// A class value interpolates; a style value does not. The split is deliberate —
// a class is a token list with a stateable safe set, a style declaration is not —
// so it is pinned rather than left to be "fixed" later by symmetry.
//
// What is no longer pinned is the silence. `style "width: {n}%"` used to compile
// and emit the braces onto the page, which is why a progress bar needed 51
// hardcoded width classes; it is a compile error now (see TestLiteralBraces).
func TestClassInterpolatesAndStyleDoesNot(t *testing.T) {
	app, err := String(`app A:
    state tone: text = "c" @client
    view V at "/":
        box class "x-rung-c-{tone}" style "top:0":
            text "hi"
`)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}

	box := app.Pages[0].View[0]
	if len(box.ClassSegs) == 0 {
		t.Fatalf("class did not interpolate: %+v", box)
	}
	if box.Class != "" {
		t.Errorf("an interpolated class also filled the flat field: %q", box.Class)
	}
	if box.ClassSegs[0].Lit != "x-rung-c-" {
		t.Errorf("class literal prefix = %q", box.ClassSegs[0].Lit)
	}
	if box.Style != "top:0" {
		t.Errorf("style = %q, want the literal value — style does not interpolate", box.Style)
	}

	// A literal class still lands in the flat field every existing consumer reads.
	app, err = String(`app A:
    view V at "/":
        box class "x-band":
            text "hi"
`)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	if got := app.Pages[0].View[0]; got.Class != "x-band" || len(got.ClassSegs) != 0 {
		t.Errorf("a literal class did not stay flat: class=%q segs=%v", got.Class, got.ClassSegs)
	}
}
