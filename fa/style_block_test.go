package fa

import (
	"strings"
	"testing"
)

const styledFDL = `
facet Panel:
    what:
        title: str
    style:
        direction: column
        gap: 2
        pad: 4
        bg: surface
        radius: md
        align: center
    looks:
        <div class="panel">
            <h3>{title}</h3>
        </div>
`

// TestStyleBlockWeb: the style: tokens resolve to concrete inline CSS on the
// facet's root element (the channel the browser renders directly).
func TestStyleBlockWeb(t *testing.T) {
	c, err := Compile(styledFDL)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := c.Render("Panel", map[string]any{"Title": "Hi"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		"display:flex",
		"flex-direction:column",
		"gap:8px",            // 2 * 4px
		"padding:16px",       // 4 * 4px
		"background:#ffffff", // surface token
		"border-radius:12px", // md token
		"align-items:center",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing resolved style %q in:\n%s", want, got)
		}
	}
	// It must land on the ROOT element (same tag as data-facet-id), not elsewhere.
	if i, j := strings.Index(got, "data-facet-id"), strings.Index(got, "display:flex"); i < 0 || j < 0 || j-i > 80 {
		t.Errorf("style not on the root element:\n%s", got)
	}
}

// TestStyleBlockNativeParity: the SAME facet rendered to the neutral tree
// resolves to the equivalent native Style — one declaration, three renderers.
func TestStyleBlockNativeParity(t *testing.T) {
	c, err := Compile(styledFDL)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	node, err := c.RenderTree("Panel", map[string]any{"Title": "Hi"})
	if err != nil {
		t.Fatalf("render tree: %v", err)
	}
	if node.Style == nil {
		t.Fatalf("root node has no resolved Style:\n%+v", node)
	}
	s := node.Style
	if s.Direction != "column" {
		t.Errorf("Direction = %q, want column", s.Direction)
	}
	if s.Gap != 8 {
		t.Errorf("Gap = %d, want 8", s.Gap)
	}
	if s.PadT != 16 || s.PadL != 16 {
		t.Errorf("padding = %d/%d, want 16 uniform", s.PadT, s.PadL)
	}
	if s.BG != "#ffffff" {
		t.Errorf("BG = %q, want #ffffff", s.BG)
	}
	if s.Radius != 12 {
		t.Errorf("Radius = %d, want 12", s.Radius)
	}
	if s.Align != "center" {
		t.Errorf("Align = %q, want center", s.Align)
	}
}

// TestStyleGrowParity: `grow: true` reaches native as Style.Grow.
func TestStyleGrowParity(t *testing.T) {
	const src = `
facet G:
    style:
        grow: true
    looks:
        <div class="g">x</div>
`
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if h, _ := c.Render("G", nil); !strings.Contains(string(h), "flex:1 1 0%") {
		t.Errorf("grow not emitted on web:\n%s", h)
	}
	node, err := c.RenderTree("G", nil)
	if err != nil {
		t.Fatalf("render tree: %v", err)
	}
	if node.Style == nil || !node.Style.Grow {
		t.Errorf("grow did not reach the neutral Style: %+v", node.Style)
	}
}

// TestStyleUnknownTokenIsCompileError: a typo'd token/property fails the build.
func TestStyleUnknownTokenIsCompileError(t *testing.T) {
	cases := []struct{ src, want string }{
		{"facet T:\n    style:\n        bg: surfaze\n    looks:\n        <p>x</p>\n", "color token"},
		{"facet T:\n    style:\n        radius: medium\n    looks:\n        <p>x</p>\n", "radius"},
		{"facet T:\n    style:\n        gpa: 2\n    looks:\n        <p>x</p>\n", "unknown style property"},
		{"facet T:\n    style:\n        gap: huge\n    looks:\n        <p>x</p>\n", "spacing step"},
	}
	for _, tc := range cases {
		_, err := Compile(tc.src)
		if err == nil {
			t.Errorf("expected compile error for %q", tc.src)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q should mention %q", err.Error(), tc.want)
		}
	}
}

// TestStyleBlockRejectedOnClientKind: style: needs a server-rendered root.
func TestStyleBlockRejectedOnClientKind(t *testing.T) {
	const src = "vault Secret:\n    style:\n        gap: 2\n    decrypt:\n        {plaintext}\n"
	if _, err := Compile(src); err == nil || !strings.Contains(err.Error(), "style:") {
		t.Fatalf("expected style: rejection on vault, got %v", err)
	}
}
