package ast

import "sort"

// What the compiler knows but will not fail on.
//
// # Why this exists at all
//
// A picture with no `alt` is invisible to a screen reader and to a search
// engine, and until `alt` existed there was no way to give one — so every image
// this stack has ever rendered was undescribed, in a component library of ~265
// components that a company site, a journal and a storefront are all built from.
// Adding the attribute closes the hole for code written from now on. It does
// nothing about the code that already exists, because the renderers write
// `alt=""` when nothing is said and `alt=""` is *correct markup* for a
// decorative image — so the omission produces no error, no wrong output, and no
// symptom anyone editing the file can see.
//
// # Why it is advice and not an error
//
// A missing `alt` deserves to be an error: silence is how the hole got to be
// everywhere. It cannot be one. Making it fatal today stops four repositories
// compiling — every `image` in `facets/`, `website/`, `apps/` and this repo's own
// `library/` and `examples/` was written before the attribute existed, and none
// of them can be fixed by the change that introduces the rule. A compiler change
// that requires a simultaneous edit to every caller in the stack is not a rule,
// it is an outage.
//
// A warning is the honest instrument, and it needs somewhere to surface. There
// is no warning channel in this compiler: ir.Build returns one error or none,
// and every caller treats it as pass/fail. So the rule is stated here, as data,
// over the syntax tree — which is what it is actually about — and the language
// server publishes it as a Warning diagnostic on the line (internal/lsp). Any
// other consumer that grows a channel for advice can call the same function and
// get the same answer; nothing has to re-derive the rule to report it.
//
// # Why an explicit empty is silent
//
// `alt ""` is a statement: this picture carries no information a reader loses by
// skipping it — a rule, a texture, a decorative mark. It is a legitimate and
// common answer, and a warning an author cannot answer is a warning they turn
// off. So the parser records whether the author wrote the attribute (AltSet),
// not merely whether the value was empty, and this reports only the case where
// nobody decided.

// Advice is one thing worth telling the author about a file that compiles.
type Advice struct {
	Line int
	Msg  string
}

// Advise reports the accessibility statements a file leaves unmade.
//
// It runs on the parsed file alone — no imports, no lowering, no placement —
// because every rule here is local to the line it is about. That also means it
// still works on a file that does not build on its own, which is most of a
// component library: `facets/ui/figure.fct` is a fragment, and a fragment is
// exactly where the undescribed pictures live.
func Advise(app *App) []Advice {
	var out []Advice
	visit := func(n Node) {
		switch t := n.(type) {
		case Image:
			if !t.AltSet {
				out = append(out, Advice{t.Line, `this image has no alt text, so a screen reader and a search engine see nothing here. ` +
					`Describe it — image "…" alt "A chart of weekly signups" — or say it is decorative on purpose with alt ""`})
			}
		case Video:
			if !t.AltSet {
				out = append(out, Advice{t.Line, `this video has no alt text, so the player has no accessible name — a reader hears "video" and nothing else. ` +
					`Name it: video "…" alt "The build, start to finish"`})
			}
		}
	}
	for _, v := range app.Views {
		WalkNodes(v.Root, visit)
	}
	for _, c := range app.Components {
		WalkNodes(c.Root, visit)
	}
	for _, l := range app.Layouts {
		WalkNodes(l.Root, visit)
	}
	WalkNodes(app.Frame, visit)
	WalkNodes(app.Content, visit)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// WalkNodes visits every node in a tree, at any depth.
//
// It is the read-only sibling of spliceInto (slot.go), and it recurses into the
// same set of body-carrying nodes for the same reason that traversal gives: a
// node this walk cannot see is a node some pass handles in silence. The two
// lists must stay identical — one rewrites what it finds and the other only
// looks, but "which nodes have children" is one fact, and a picture inside a
// `match` arm inside a component is exactly where a library keeps its pictures.
func WalkNodes(nodes []Node, visit func(Node)) {
	for _, n := range nodes {
		visit(n)
		switch t := n.(type) {
		case Modified:
			WalkNodes([]Node{t.Inner}, visit)
		case Box:
			WalkNodes(t.Children, visit)
		case Row:
			WalkNodes(t.Children, visit)
		case If:
			WalkNodes(t.Body, visit)
		case For:
			WalkNodes(t.Body, visit)
		case Overlay:
			WalkNodes(t.Body, visit)
		case Form:
			WalkNodes(t.Body, visit)
		case Use:
			WalkNodes(t.Body, visit)
		case Tabs:
			for _, tb := range t.Tabs {
				WalkNodes(tb.Body, visit)
			}
		case Match:
			for _, cs := range t.Cases {
				WalkNodes(cs.Body, visit)
			}
			WalkNodes(t.Else, visit)
		}
	}
}
