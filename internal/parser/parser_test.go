package parser

import (
	"os"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

func parseOne(t *testing.T, src string) *ast.Facet {
	t.Helper()
	facets, err := Parse(src)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(facets) != 1 {
		t.Fatalf("expected 1 facet, got %d", len(facets))
	}
	return facets[0]
}

func TestExampleFacet(t *testing.T) {
	src, err := os.ReadFile("../../examples/like_button.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	f := parseOne(t, string(src))

	if f.Name != "LikeButton" {
		t.Errorf("name = %q, want LikeButton", f.Name)
	}

	// what
	wantFields := []ast.Field{
		{Name: "post", Type: "Post"},
		{Name: "count", Type: "int"},
		{Name: "liked", Type: "bool"},
	}
	if len(f.Fields) != len(wantFields) {
		t.Fatalf("fields = %d, want %d", len(f.Fields), len(wantFields))
	}
	for i, w := range wantFields {
		if f.Fields[i].Name != w.Name || f.Fields[i].Type != w.Type {
			t.Errorf("field %d = %+v, want %+v", i, f.Fields[i], w)
		}
	}

	// facet-id derivation: first custom-typed field is `post: Post`
	if got := f.DerivedFacetID(); got != "LikeButton:post:{post.id}" {
		t.Errorf("facet-id = %q, want LikeButton:post:{post.id}", got)
	}

	// when
	if len(f.Whens) != 1 {
		t.Fatalf("whens = %d, want 1", len(f.Whens))
	}
	w := f.Whens[0]
	if len(w.Events) != 1 || w.Events[0] != "post.like_toggled" {
		t.Errorf("events = %v", w.Events)
	}
	if len(w.Mutations) != 1 {
		t.Fatalf("mutations = %d, want 1", len(w.Mutations))
	}
	m := w.Mutations[0]
	if m.Op != "replace" || m.Target != "LikeButton" || m.With != "event.payload" {
		t.Errorf("mutation = %+v", m)
	}

	// looks: literal HTML must survive, and the control/interp holes must be present
	lit := ast.LooksText(f.Looks)
	for _, want := range []string{"<button", "</button>", "<svg", `class="btn-like__count"`} {
		if !strings.Contains(lit, want) {
			t.Errorf("looks literal missing %q", want)
		}
	}

	// Count control markers: inline `if liked`/`end`, inline `if liked`/`else`/`end`,
	// and the block `if count` (synthesized end) → 3 `if`, 1 `else`, 3 `end`.
	var ifs, elses, ends, interps int
	for _, n := range f.Looks {
		switch c := n.(type) {
		case ast.Ctrl:
			switch c.Op {
			case "if":
				ifs++
			case "else":
				elses++
			case "end":
				ends++
			}
		case ast.Interp:
			interps++
		}
	}
	if ifs != 3 || elses != 1 || ends != 3 {
		t.Errorf("control counts: if=%d else=%d end=%d, want 3/1/3", ifs, elses, ends)
	}
	if interps < 2 {
		t.Errorf("interps = %d, want >= 2 ({post.id}, {count})", interps)
	}
}

func TestBlockIfElse(t *testing.T) {
	src := "" +
		"facet T:\n" +
		"    looks:\n" +
		"        <ul>\n" +
		"        if items:\n" +
		"            <li>has</li>\n" +
		"        else:\n" +
		"            <li>none</li>\n" +
		"        </ul>\n"
	f := parseOne(t, src)
	var seq []string
	for _, n := range f.Looks {
		if c, ok := n.(ast.Ctrl); ok {
			seq = append(seq, c.Op)
		}
	}
	if got := strings.Join(seq, ","); got != "if,else,end" {
		t.Fatalf("control sequence = %q, want if,else,end", got)
	}
}

func TestForLoop(t *testing.T) {
	src := "" +
		"facet T:\n" +
		"    looks:\n" +
		"        for u in users:\n" +
		"            <span>{u.name}</span>\n"
	f := parseOne(t, src)
	var c ast.Ctrl
	for _, n := range f.Looks {
		if cc, ok := n.(ast.Ctrl); ok && cc.Op == "for" {
			c = cc
		}
	}
	if c.Var != "u" || c.Iter != "users" {
		t.Fatalf("for parsed as var=%q iter=%q, want u/users", c.Var, c.Iter)
	}
}

func TestComputedFieldRejected(t *testing.T) {
	src := "facet T:\n    what:\n        x = fetch()\n"
	_, err := Parse(src)
	if err == nil || !strings.Contains(err.Error(), "computed") {
		t.Fatalf("expected computed-field rejection, got %v", err)
	}
}

func TestRenamedBlocksTeach(t *testing.T) {
	cases := []struct{ src, want string }{
		{"facet T:\n    data:\n        x: int\n", "what"},
		{"facet T:\n    render:\n        <p>hi</p>\n", "looks"},
		{"facet T:\n    subscribe:\n        \"x\"\n", "when"},
		{"facet T:\n    update on x:\n        replace T\n", "when"},
		{"facet T:\n    auth:\n        require: x\n", "who"},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("source %q: expected error mentioning %q, got %v", c.src, c.want, err)
		}
	}
}

func TestSlotAndBlockChild(t *testing.T) {
	src := "" +
		"facet Card:\n" +
		"    looks:\n" +
		"        <div>\n" +
		"            slot:\n" +
		"                <p>default</p>\n" +
		"        </div>\n" +
		"facet Page:\n" +
		"    looks:\n" +
		"        <Card>\n" +
		"            <span>hi</span>\n" +
		"        </Card>\n"
	facets, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	hasSlot := false
	for _, n := range facets[0].Looks {
		if _, ok := n.(ast.Slot); ok {
			hasSlot = true
		}
	}
	if !hasSlot {
		t.Error("Card should contain a Slot node")
	}
	var child ast.Child
	found := false
	for _, n := range facets[1].Looks {
		if ch, ok := n.(ast.Child); ok {
			child, found = ch, true
		}
	}
	if !found || child.Name != "Card" || child.Children == nil {
		t.Fatalf("Page should have a block-form <Card> with children: found=%v %+v", found, child)
	}
}

func TestWhoBlock(t *testing.T) {
	src := "" +
		"facet AdminPanel:\n" +
		"    who:\n" +
		"        require: is_admin\n" +
		"        redact author_ip always\n" +
		"        redact internal_flags unless is_moderator\n" +
		"    what:\n" +
		"        post: Post\n" +
		"    looks:\n" +
		"        <div>secret</div>\n"
	f := parseOne(t, src)
	if !f.HasWho() {
		t.Fatal("expected who block")
	}
	if len(f.Who.Require) != 1 || f.Who.Require[0] != "is_admin" {
		t.Errorf("require = %v", f.Who.Require)
	}
	if len(f.Who.Redactions) != 2 {
		t.Fatalf("redactions = %d, want 2", len(f.Who.Redactions))
	}
	if f.Who.Redactions[0].Field != "author_ip" || f.Who.Redactions[0].UnlessPolicy != "" {
		t.Errorf("redaction 0 = %+v", f.Who.Redactions[0])
	}
	if f.Who.Redactions[1].Field != "internal_flags" || f.Who.Redactions[1].UnlessPolicy != "is_moderator" {
		t.Errorf("redaction 1 = %+v", f.Who.Redactions[1])
	}
}

func TestMultipleFacets(t *testing.T) {
	src := "" +
		"facet A:\n    what:\n        x: int\n" +
		"facet B:\n    what:\n        y: str\n"
	facets, err := Parse(src)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(facets) != 2 || facets[0].Name != "A" || facets[1].Name != "B" {
		t.Fatalf("got %d facets: %+v", len(facets), facets)
	}
}

func TestSingletonFacetID(t *testing.T) {
	src := "facet Nav:\n    what:\n        count: int\n"
	f := parseOne(t, src)
	if got := f.DerivedFacetID(); got != "Nav" {
		t.Fatalf("singleton facet-id = %q, want Nav", got)
	}
}
