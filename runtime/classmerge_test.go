package runtime

import (
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"facet/internal/ir"
)

// classMergeCases exercises the class/style escape hatch's merge rule: a node's
// hardcoded base class comes first, the author's `class "..."` is appended after
// it (so an author can override), and `style "..."` becomes the inline style
// verbatim. Cases cover every combination of "is there a base class" and "did the
// author supply one" — the exact split that let tabs/form/upload/list/if/match/
// overlay/use silently drop the author's modifier while box/row/text/... (which
// always went through nodeAttrs) kept it.
var classMergeCases = []struct{ base, class, style, anchor string }{
	{"", "", "", ""},
	{"fa-tabs", "", "", ""},
	{"", "x-tabs", "", ""},
	{"fa-tabs", "x-tabs", "", ""},
	{"fa-form", "x-form", "color:blue", ""},
	{"", "", "color:red", ""},
	{"fa-upload", "x-upload x-big", "display:flex", ""},
	{"", "x-only", "border:1px solid red", ""},
	{"fa-box", "", "", "install"},
	{"fa-box", "x-band", "top:0", "install-2"},
	{"", "", "", "Section_3"},
}

var attrRE = regexp.MustCompile(`(?:^| )class="([^"]*)"|(?:^| )style="([^"]*)"|(?:^| )id="([^"]*)"`)

// parseAttrs pulls the class and style values back out of what nodeAttrs wrote,
// unescaping them, so they compare against the client's unescaped DOM strings.
// nodeAttrs never writes a `class=""` or `style=""` (it omits the attribute
// entirely when there is nothing to put in it), so which alternative matched is
// enough to tell the two groups apart.
func parseAttrs(t *testing.T, attrs string) (class, style, id string) {
	t.Helper()
	for _, m := range attrRE.FindAllStringSubmatch(attrs, -1) {
		switch {
		case strings.Contains(m[0], `class="`):
			class = html.UnescapeString(m[1])
		case strings.Contains(m[0], `style="`):
			style = html.UnescapeString(m[2])
		default:
			id = html.UnescapeString(m[3])
		}
	}
	return class, style, id
}

// TestNodeAttrsMergesClassAndStyleLikeTheClient pins the class/style merge rule
// (fct/runtime/server.go's nodeAttrs) against a mirror of the two lines in
// fct/runtime/assets/facet.js's render() that do the same job on the client,
// following the house pattern in link_test.go: run the mirror under node over
// the same cases the Go function sees, and diff.
//
// It exists because that split was exactly how tabs/form/upload/list/if/match/
// overlay/use lost the author's `class`/`style` on first paint while the client
// applied it uniformly to every element node — two implementations of one rule,
// agreeing only where a test happened to look.
func TestNodeAttrsMergesClassAndStyleLikeTheClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
function classText(segs, sc) { return ""; } // no interpolated class in these cases
function applyAttrs(base, node) {
  let e = { className: base }, style = "", id = "";
  const cls = node.classSegs ? classText(node.classSegs, sc) : node.class;
  if (cls) e.className = (e.className ? e.className + " " : "") + cls;
  if (node.style) style = node.style;
  if (node.anchor) id = node.anchor;
  return [e.className, style, id];
}
const cases = `)
	script.WriteString("[")
	for i, c := range classMergeCases {
		if i > 0 {
			script.WriteString(",")
		}
		b, _ := json.Marshal([4]string{c.base, c.class, c.style, c.anchor})
		script.WriteString(string(b))
	}
	script.WriteString(`];
console.log(JSON.stringify(cases.map(([base, cls, style, anchor]) => applyAttrs(base, {class: cls, style, anchor}))));
`)

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got [][3]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(got) != len(classMergeCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(classMergeCases))
	}

	for i, c := range classMergeCases {
		attrs := nodeAttrs(c.base, ir.Node{Class: c.class, Style: c.style, Anchor: c.anchor})
		gotClass, gotStyle, gotID := parseAttrs(t, attrs)
		wantClass, wantStyle, wantID := got[i][0], got[i][1], got[i][2]

		if gotClass != wantClass {
			t.Errorf("case %+v: server class %q, client class %q", c, gotClass, wantClass)
		}
		if gotStyle != wantStyle {
			t.Errorf("case %+v: server style %q, client style %q", c, gotStyle, wantStyle)
		}
		if gotID != wantID {
			t.Errorf("case %+v: server id %q, client id %q", c, gotID, wantID)
		}
	}
}

// The mirror above must be the code that ships, or the parity it proves is a
// coincidence — same failure this test exists to catch, one level up.
func TestClientClassStyleMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}

	for _, want := range []string{
		`const cls = node.classSegs ? classText(node.classSegs, sc) : node.class;`,
		`if (cls) e.className = (e.className ? e.className + " " : "") + cls;`,
		`if (node.style) e.setAttribute("style", node.style);`,
		`if (node.anchor) e.setAttribute("id", node.anchor);`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the mirror checked by\n"+
				"TestNodeAttrsMergesClassAndStyleLikeTheClient has drifted from the shipped client", want)
		}
	}
}

// The values an interpolated class token must survive. A class attribute is a
// whitespace-separated token LIST, so the character that matters most is the
// space: an unfiltered value carrying one adds a class it was never given a slot
// for, which is the class-level equivalent of a path value carrying a `/` and
// quietly becoming two segments. The rest are characters that mean something to a
// selector, to the markup, or to neither.
var classTokenCases = []string{
	"warm",
	"warm big",       // would add a second class
	"a\tb",           // so would any other whitespace
	"a\nb",           //
	"x\" onclick=\"", // cannot reach the markup, but must not survive as a token
	"a.b",            // a selector separator
	"a#b",
	"a>b",
	"100%",
	"ünïcøde",
	"",
	"--x-tone",
}

// The server writes a class into markup and the client assigns it through
// `className`. Neither escapes for the other, so the filtered value has to be the
// same on both sides — a class that differs between first paint and hydration is
// an element whose appearance changes the instant the page becomes interactive.
func TestClientClassTokenEscapeMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
  function escapeClassToken(s) {
    let out = "";
    for (const c of s) if (/[A-Za-z0-9\-_]/.test(c)) out += c;
    return out;
  }
const cases = `)
	b, _ := json.Marshal(classTokenCases)
	script.Write(b)
	script.WriteString(";\nconsole.log(JSON.stringify(cases.map(escapeClassToken)));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(got) != len(classTokenCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(classTokenCases))
	}

	for i, in := range classTokenCases {
		if want := escapeClassToken(in); got[i] != want {
			t.Errorf("escapeClassToken(%q): client %q, server %q", in, got[i], want)
		}
	}

	// The case the rule exists for, stated.
	if got := escapeClassToken("warm big"); got != "warmbig" {
		t.Errorf(`escapeClassToken("warm big") = %q, want "warmbig" — a value must `+
			`fill its token and not add another`, got)
	}
}

// An interpolated class is assembled the same way on both sides: literals through
// untouched so the author's own spaces and prefixes survive, interpolated runs
// filtered. This is the class analogue of link_test.go's href parity.
func TestClientClassTextMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	segs := func(v string) []ir.Seg {
		return []ir.Seg{
			{Lit: "x-rung x-rung-c-"},
			{Expr: &ir.Expr{Kind: "lit", Val: v, VType: "text"}},
			{Lit: " x-end"},
		}
	}

	var script strings.Builder
	script.WriteString(`
  function escapeClassToken(s) {
    let out = "";
    for (const c of s) if (/[A-Za-z0-9\-_]/.test(c)) out += c;
    return out;
  }
  function classText(segs) {
    let out = "";
    for (const seg of segs || []) {
      if (seg.expr) out += escapeClassToken(seg.expr);
      else out += seg.lit || "";
    }
    return out;
  }
const cases = `)
	b, _ := json.Marshal(classTokenCases)
	script.Write(b)
	script.WriteString(`;
console.log(JSON.stringify(cases.map(v =>
  classText([{lit: "x-rung x-rung-c-"}, {expr: v}, {lit: " x-end"}]))));
`)

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}

	s := &Server{}
	for i, v := range classTokenCases {
		want := classText(segs(v), func(ss []ir.Seg) string { return s.segsToString(ss, nil) })
		if got[i] != want {
			t.Errorf("classText(%q): client %q, server %q", v, got[i], want)
		}
	}
}

// The two mirrors above must be the code that ships.
func TestClientClassTextMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}

	for _, want := range []string{
		`for (const c of s) if (/[A-Za-z0-9\-_]/.test(c)) out += c;`,
		`if (seg.expr || seg.bind) out += escapeClassToken(segsToStr([seg], sc));`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the mirror checked by\n"+
				"TestClientClassTokenEscapeMatchesServer has drifted from the shipped client", want)
		}
	}
}
