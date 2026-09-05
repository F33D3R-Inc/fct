package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"facet/internal/ir"
)

// A heading's level is a value, so the element name is computed at render — on
// both sides, from the same expression, with no message passing between them.
// That is one rule implemented twice, which is the shape this repo has produced
// twelve bugs from, so it is pinned here the way link_test.go, classmerge_test.go
// and attrtext_test.go pin theirs.
//
// The compiler refuses every level it can prove wrong (see internal/ir/heading.go),
// so what is left here is what it cannot prove: a level that arrives as a value.
// The renderers are total — like toStr and toInt, they have nowhere to report a
// failure from — so an out-of-range level clamps into 1..6 rather than producing
// an `<h0>`, and both sides must clamp identically or first paint and hydration
// disagree about the shape of the document.
var headingLevelCases = []any{
	1, 2, 3, 4, 5, 6,
	0, 7, -3, 99,
	"3", "0", "", "two",
	true, nil, 2.7,
}

func TestServerRendersHeadingsAtTheirLevel(t *testing.T) {
	s := &Server{ir: &ir.IR{}}
	rd := &renderer{s: s, regions: map[string][]any{}, mat: &materializer{s: s, out: map[string]any{}, counts: map[string]int{}}}

	lit := func(v any, vtype string) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: vtype} }
	cases := []struct {
		name, want string
		node       ir.Node
	}{{
		name: "a level is the element, and the class is the node's",
		node: ir.Node{Kind: "heading", Level: lit(2, "int"), Segs: []ir.Seg{{Lit: "Replies"}}},
		want: `<h2 class="fa-heading">Replies</h2>`,
	}, {
		name: "an author's class rides alongside the built-in one",
		node: ir.Node{Kind: "heading", Level: lit(3, "int"), Segs: []ir.Seg{{Lit: "T"}}, Class: "x-section-title"},
		want: `<h3 class="fa-heading x-section-title">T</h3>`,
	}, {
		name: "an out-of-range level clamps rather than inventing an element",
		node: ir.Node{Kind: "heading", Level: lit(9, "int"), Segs: []ir.Seg{{Lit: "T"}}},
		want: `<h6 class="fa-heading">T</h6>`,
	}, {
		name: "the words are escaped exactly as a text node's are",
		node: ir.Node{Kind: "heading", Level: lit(1, "int"), Segs: []ir.Seg{{Expr: lit(`<script>`, "text")}}},
		want: `<h1 class="fa-heading">&lt;script&gt;</h1>`,
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

// The clamp, run on both sides over the same values.
func TestClientHeadingLevelMatchesTheServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
function toInt(v) {
  if (typeof v === "number") return Math.trunc(v);
  if (v === true) return 1;
  if (typeof v === "string") {
    const t = v.trim();
    if (!/^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/.test(t)) return 0;
    return Math.trunc(Number(t));
  }
  return 0;
}
function headingLevel(v) { const n = toInt(v); return n < 1 ? 1 : n > 6 ? 6 : n; }
const cases = `)
	b, err := json.Marshal(headingLevelCases)
	if err != nil {
		t.Fatalf("encoding the cases: %v", err)
	}
	script.Write(b)
	script.WriteString(";\nconsole.log(JSON.stringify(cases.map(headingLevel)));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}
	var got []int
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(got) != len(headingLevelCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(headingLevelCases))
	}
	for i, v := range headingLevelCases {
		if want := headingLevel(v); got[i] != want {
			t.Errorf("level %#v: the client would render <h%d>, the server renders <h%d>", v, got[i], want)
		}
	}
}

// The mirror above must be the code that ships, or the parity it proves is a
// coincidence — the same failure this test exists to catch, one level up.
func TestClientHeadingMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	for _, want := range []string{
		`function headingLevel(v) { const n = toInt(v); return n < 1 ? 1 : n > 6 ? 6 : n; }`,
		`const h = el("h" + headingLevel(ev(node.level, sc)), "fa-heading");`,
		`appendSegs(h, node.segs, sc);`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the client half of the\n"+
				"heading rule has drifted from what this file checks", want)
		}
	}
}
