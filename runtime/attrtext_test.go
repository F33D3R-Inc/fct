package runtime

import (
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"strings"
	"testing"

	"facet/internal/ir"
)

// The values an interpolated attribute must survive. Every one of them is a
// character that means something in the markup the server builds by hand:
// the quote it wraps every attribute in, the quote it does not, the angle
// brackets that open a tag, the ampersand that starts an entity, and the exact
// sequence that turns a placeholder into an event handler.
var attrTextCases = []string{
	`plain`,
	`a "quoted" word`,
	`it's`,
	`" onmouseover="alert(1)`,
	`' onmouseover='alert(1)`,
	`</script><script>alert(1)</script>`,
	`5 > 3 && 2 < 4`,
	`&#34;`, // an already-escaped value must not be double-decoded into a quote
	`</textarea>`,
	``,
}

// segsFor renders one hostile value the way a component's argument reaches an
// attribute: a literal prefix, then the value as an interpolated segment.
func segsFor(v string) []ir.Seg {
	return []ir.Seg{
		{Lit: "hint: "},
		{Expr: &ir.Expr{Kind: "lit", Val: v, VType: "text"}},
	}
}

// An interpolated value in an attribute must not be able to leave it.
//
// The server writes every attribute as `name="…"`, so the only escape is a `"`;
// the only way to inject a handler is to close the attribute and open another.
// This asserts the property rather than an exact encoding: whatever escaper is
// used, its output must contain no raw quote, no angle bracket, and must decode
// back to exactly the value that went in.
func TestAnInterpolatedAttributeCannotLeaveItsQuotes(t *testing.T) {
	s := &Server{}

	for _, v := range attrTextCases {
		got := s.attrText(segsFor(v), nil)

		for _, bad := range []string{`"`, `'`, `<`, `>`} {
			if strings.Contains(got, bad) {
				t.Errorf("attrText(%q) = %q, which still contains a raw %s — "+
					"a value carrying that character can close the attribute the "+
					"renderer opened and start one of its own", v, got, bad)
			}
		}

		if want := "hint: " + v; html.UnescapeString(got) != want {
			t.Errorf("attrText(%q) = %q, which decodes to %q, want %q",
				v, got, html.UnescapeString(got), want)
		}
	}
}

// The server escapes because it builds markup; the client assigns through a DOM
// property and never escapes at all. That is one rule with two spellings, which
// is the shape this repo has now found three bugs in — so it is pinned here the
// way link_test.go and classmerge_test.go pin theirs: run the shipped client's
// half under node over the same values, and require that what the server wrote
// decodes to exactly what the client would put in the property.
//
// A divergence here is not cosmetic. If the client produced more than the server
// escaped, hydration would write characters the first paint had neutralised.
func TestClientAttrTextMatchesWhatTheServerEscaped(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
function toStr(v) { return v == null ? "" : String(v); }
function ev(e) { return e.val; }
function segsToStr(segs, sc) {
  let out = "";
  for (const seg of segs || []) {
    if (seg.lit != null) out += seg.lit;
    else if (seg.expr) out += toStr(ev(seg.expr, sc));
  }
  return out;
}
function attrText(segs, sc) { return segsToStr(segs, sc); }
const cases = `)

	var payload [][]ir.Seg
	for _, v := range attrTextCases {
		payload = append(payload, segsFor(v))
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the cases: %v", err)
	}
	script.Write(b)
	script.WriteString(";\nconsole.log(JSON.stringify(cases.map((c) => attrText(c, {}))));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(got) != len(attrTextCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(attrTextCases))
	}

	s := &Server{}
	for i, v := range attrTextCases {
		want := html.UnescapeString(s.attrText(segsFor(v), nil))
		if got[i] != want {
			t.Errorf("value %q: the client would put %q in the attribute, the "+
				"server's escaped HTML decodes to %q", v, got[i], want)
		}
	}
}

// The mirror above must be the code that ships, or the parity it proves is a
// coincidence — the same failure this test exists to catch, one level up.
func TestClientAttrTextMirrorIsTheShippedSource(t *testing.T) {
	b, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	raw := string(b)

	if !strings.Contains(raw, `function attrText(segs, sc) { return segsToStr(segs, sc); }`) {
		t.Error("assets/facet.js no longer defines attrText as the flattened segments — " +
			"the mirror checked by TestClientAttrTextMatchesWhatTheServerEscaped has drifted")
	}

	// Every attribute this pass made interpolatable must reach the DOM through
	// attrText, and through a property or setAttribute — never through innerHTML,
	// which is the one way this side could start needing an escaper of its own.
	for _, want := range []string{
		`i.placeholder = attrText(node.placeholder, sc);`,
		`i.setAttribute("data-fa-icon", attrText(node.segs, sc));`,
		`emit(o.value, segsToStr(o.label, sc));`,
		`opt.value = value; opt.textContent = label;`,
		`submit.textContent = attrText(node.label, sc);`,
		`label.appendChild(document.createTextNode(attrText(node.label, sc)));`,
		`b.textContent = attrText(t.label, sc);`,
		`a.textContent = attrText(node.label, sc);`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("assets/facet.js no longer contains %q — an interpolated "+
				"attribute has stopped going through the client half of the rule", want)
		}
	}
}
