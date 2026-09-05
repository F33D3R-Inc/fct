package runtime

import (
	"os"
	"strings"
	"testing"
)

// The controls' two renderers, pinned to each other at the source.
//
// integration/control_test.go is the real check: it boots the shipped facet.js
// over a page this server actually rendered and compares the controls the two
// sides produced, down to whether a box is ticked. But it skips without node,
// and the divergence it catches is exactly the kind someone introduces while
// editing one side — so this asserts, from Go alone, that both sides still
// contain the arm the other one has.
//
// It is deliberately about the pieces that differ *by construction*: the server
// writes markup and the client assigns DOM properties, and nothing but both
// files being written to the same rule makes `checked`, `name`, `value` and
// `type` come out the same.
func TestBothRenderersStillCarryEveryControl(t *testing.T) {
	client, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	server, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading the server renderer: %v", err)
	}

	for _, c := range []struct {
		control        string
		inServer, inJS string
		why            string
	}{
		{
			control:  "textarea",
			inServer: `<textarea%s data-fa-input="%s" placeholder="%s">%s</textarea>`,
			inJS:     `const t = el("textarea", "fa-textarea");`,
			why:      "a textarea's value is its content on one side and .value on the other",
		},
		{
			control:  "checkbox",
			inServer: `<label%s><input type="checkbox" data-fa-input="%s"%s%s><span>%s</span></label>`,
			inJS:     `i.setAttribute("type", "checkbox");`,
			why:      "the server writes `checked` as an attribute and the client assigns the property",
		},
		{
			control:  "toggle (a checkbox variant, not a node kind)",
			inServer: `if n.Value == "switch" {`,
			inJS:     `node.value === "switch" ? "fa-toggle" : "fa-checkbox"`,
			why:      "`toggle` is Node.Value == \"switch\"; if one side stops reading it, one side stops being a toggle",
		},
		{
			control:  "radio",
			inServer: `<input type="radio" name="%s" value="%s" data-fa-input="%s"%s>`,
			inJS:     `i.setAttribute("name", node.bind);`,
			why:      "the group is the bound cell's name; without it a browser lets two be selected at once",
		},
		{
			control:  "password/newpassword (an input variant, not a node kind)",
			inServer: `secret = fmt.Sprintf(` + "`" + ` type="password" autocomplete="%s"` + "`" + `, html.EscapeString(n.Value))`,
			inJS:     `if (node.value) { i.setAttribute("type", "password"); i.setAttribute("autocomplete", node.value); }`,
			why: "the mask is `type=\"password\"` and nothing else; a side that stops writing it hydrates the " +
				"secret into a plain text box, which is what the CSS-masked field this replaced did on every render",
		},
	} {
		t.Run(c.control, func(t *testing.T) {
			if !strings.Contains(string(server), c.inServer) {
				t.Errorf("runtime/server.go no longer renders %s as %q — %s",
					c.control, c.inServer, c.why)
			}
			if !strings.Contains(string(client), c.inJS) {
				t.Errorf("runtime/assets/facet.js no longer renders %s (%q) — %s",
					c.control, c.inJS, c.why)
			}
		})
	}

	// One list decides what counts as a two-way control on the client. Three
	// places used to ask that question separately, and a control added to two of
	// them worked until its cell changed underneath it.
	for _, want := range []string{
		`const CONTROL_KINDS = { input: 1, select: 1, upload: 1, typeahead: 1, textarea: 1, checkbox: 1, radio: 1 };`,
		`if (CONTROL_KINDS[n.kind] && n.id) inputById[n.id] = n;`,
		`function syncControl(node) {`,
		`root.addEventListener("change", function (e) {`,
	} {
		if !strings.Contains(string(client), want) {
			t.Errorf("assets/facet.js no longer contains %q — a control can no longer be "+
				"written by the actor, or no longer follows its cell", want)
		}
	}
}

// Choices drawn from data: one walk, written twice.
//
// integration/dataoptions_test.go is the real check — it boots the shipped
// facet.js over a page this server rendered and compares the choices the two
// sides actually offered, down to which one is selected. This asserts, from Go
// alone, that the pieces which agree only *by construction* are still present on
// both sides, because those are the ones someone editing one file removes without
// anything failing:
//
//   - one walk turns a fixed list and a repeating one into a single sequence of
//     choices. Two walks would be two answers to "what does this control offer";
//   - a row's expressions are evaluated at the address the other side recomputes,
//     or a per-row aggregate in a label resolves to another row's value;
//   - the control is marked as a region exactly when its choices come from data,
//     or the client has nothing to re-fill and the dropdown silently goes stale.
func TestBothRenderersDrawChoicesFromDataByTheSameRule(t *testing.T) {
	client, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	server, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading the server renderer: %v", err)
	}

	for _, c := range []struct {
		rule           string
		inServer, inJS string
		why            string
	}{
		{
			rule:     "one walk over both shapes of a choice list",
			inServer: `func (rd *renderer) eachOption(n ir.Node, scope map[string]any, path string, yield func(value, label string)) {`,
			inJS:     `function eachOption(node, sc, path, emit) {`,
			why:      "a select and a radio group ask one function what they offer; a second walk is a second answer",
		},
		{
			rule:     "the fixed list is still the fixed list",
			inServer: `for _, o := range n.Options {`,
			inJS:     `for (const o of list(node.options)) emit(o.value, segsToStr(o.label, sc));`,
			why:      "a control whose choices are all literal must render from ir.Node.Options exactly as it always did",
		},
		{
			rule:     "a repeating entry yields one choice per row",
			inServer: `if c.Kind != "options" {`,
			inJS:     `if (c.kind !== "options") { emit(optionValue(c, sc), segsToStr(c.label, sc)); continue; }`,
			why:      "the `option`/`options` children are the dynamic list; skipping either kind drops choices on one side only",
		},
		{
			rule:     "a row's expressions are addressed identically",
			inServer: `rd.mat.path = s.listRowPath(c, cpath, j, r)`,
			inJS:     `curPath = rowPathFor(c, cpath, j++, row);`,
			why:      "the server records an aggregate at the address the client looks it up at; one side moving is a wrong value, not an error",
		},
		{
			rule:     "the stored value may be computed",
			inServer: `func (rd *renderer) optionValue(n ir.Node, scope map[string]any) string {`,
			inJS:     `function optionValue(node, sc) {`,
			why:      "ir.Node.Val beside ir.Node.Value is the whole of what a data-driven choice adds; one side reading only Value stores the wrong identity",
		},
		{
			rule:     "the control is a region exactly when its choices come from data",
			inServer: `func optionRegionID(n ir.Node) string {`,
			inJS:     `function optionsRegionId(node) {`,
			why:      "the server writes data-fa-region and the client re-fills by it; without the pair the options paint once and then lie",
		},
	} {
		t.Run(c.rule, func(t *testing.T) {
			if !strings.Contains(string(server), c.inServer) {
				t.Errorf("runtime/server.go no longer contains %q — %s", c.inServer, c.why)
			}
			if !strings.Contains(string(client), c.inJS) {
				t.Errorf("runtime/assets/facet.js no longer contains %q — %s", c.inJS, c.why)
			}
		})
	}

	// The client half has two more obligations with nothing on the server to
	// mirror them, because they are about refreshing a page the server rendered
	// once: a choice list is re-filled like any other region, and a control that is
	// BOTH a region and a two-way input has both answered rather than the first.
	for _, want := range []string{
		`: kind === "select" || kind === "radio" ? fillOptions : fillIf;`,
		`if (regionById[id]) {`,
		`if (inputById[id]) syncControl(inputById[id]);`,
	} {
		if !strings.Contains(string(client), want) {
			t.Errorf("assets/facet.js no longer contains %q — a control whose choices come from "+
				"data no longer refreshes on both of its edges", want)
		}
	}
}
