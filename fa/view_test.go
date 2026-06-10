package fa

import (
	"encoding/json"
	"strings"
	"testing"
)

// A facet rendered to a neutral tree carries the same structure a native runtime
// needs: element kinds, the facet id (for surgical updates), and the action (for
// taps) — with no HTML/DOM dependency.
func TestRenderTreeButton(t *testing.T) {
	c, err := Compile(`
facet Button:
    what:
        label: str
        action: str
    looks:
        <button class="fa-btn fa-btn--primary" data-action="{action}" data-facet-id="Button">
            <span>{label}</span>
        </button>
`)
	if err != nil {
		t.Fatal(err)
	}
	root, err := c.RenderTree("Button", map[string]any{"Label": "Go live", "Action": "stream.start"})
	if err != nil {
		t.Fatal(err)
	}
	if root.Kind != "button" {
		t.Errorf("root kind = %q, want button", root.Kind)
	}
	if root.Action != "stream.start" {
		t.Errorf("action = %q, want stream.start (a native tap maps to this)", root.Action)
	}
	if len(root.Children) != 1 || root.Children[0].Kind != "text" || root.Children[0].Text != "Go live" {
		t.Errorf("expected one text child 'Go live', got %+v", root.Children)
	}
	// It must serialize to JSON a native client can consume.
	b, _ := json.Marshal(root)
	if !strings.Contains(string(b), `"kind":"button"`) || !strings.Contains(string(b), `"text":"Go live"`) {
		t.Errorf("neutral JSON wrong: %s", b)
	}
}

// Composition produces a nested neutral tree with each facet's id preserved — so
// surgical sub-facet updates work on native exactly as on web.
func TestRenderTreeComposition(t *testing.T) {
	c, err := Compile(`
facet Avatar:
    what:
        src: str
    looks:
        <img class="fa-avatar" src="{src}" data-facet-id="Avatar" />

facet Row:
    what:
        src: str
        name: str
    looks:
        <div class="row" data-facet-id="Row">
            <Avatar src="{src}" />
            <span>{name}</span>
        </div>
`)
	if err != nil {
		t.Fatal(err)
	}
	root, err := c.RenderTree("Row", map[string]any{"Src": "/a.png", "Name": "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if root.Kind != "box" || root.FacetID != "Row" {
		t.Errorf("root = kind %q id %q, want box/Row", root.Kind, root.FacetID)
	}
	var img, text *ViewNode
	for _, ch := range root.Children {
		switch ch.Kind {
		case "image":
			img = ch
		case "text":
			text = ch
		}
	}
	if img == nil || img.FacetID != "Avatar" || img.Attrs["src"] != "/a.png" {
		t.Errorf("nested Avatar image wrong: %+v", img)
	}
	if text == nil || text.Text != "Ada" {
		t.Errorf("name text wrong: %+v", text)
	}
}

// The neutral parser must survive the real stdlib-shaped markup (icons/SVG, void
// elements, nested boxes) without losing structure.
func TestParseViewHandlesRealMarkup(t *testing.T) {
	frag := `<div class="fa-vcard"><div class="fa-vcard__thumb">` +
		`<img src="/t.jpg" alt=""/><span class="fa-vcard__dur">2:14</span></div>` +
		`<button data-action="play"><svg viewBox="0 0 24 24"><path d="M3 3"/></svg>Play</button></div>`
	root, err := ParseView(frag)
	if err != nil {
		t.Fatal(err)
	}
	if root.Kind != "box" {
		t.Fatalf("root kind = %q", root.Kind)
	}
	// Find the button and confirm its action + that the svg collapsed to one icon.
	var btn *ViewNode
	var walk func(n *ViewNode)
	walk = func(n *ViewNode) {
		if n.Kind == "button" {
			btn = n
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	if btn == nil || btn.Action != "play" {
		t.Fatalf("button/action not found: %+v", btn)
	}
	var icons, texts int
	for _, c := range btn.Children {
		if c.Kind == "icon" {
			icons++
		}
		if c.Kind == "text" && c.Text == "Play" {
			texts++
		}
	}
	if icons != 1 || texts != 1 {
		t.Errorf("button children: want 1 icon + 1 'Play' text, got icons=%d texts=%d (%+v)", icons, texts, btn.Children)
	}
}
