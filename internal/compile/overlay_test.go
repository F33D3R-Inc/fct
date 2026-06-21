package compile

import (
	"strings"
	"testing"
)

const overlayApp = `app UI:
    state menuOpen: bool = false @client
    state q: text = "" @client
    entity Tag:
        id: int
        name: text
    action open():
        menuOpen = true
    view Home at "/":
        box:
            button "Menu" -> open()
            typeahead bind q from Tag.name placeholder "find a tag"
            overlay bind menuOpen:
                box:
                    text "menu contents"
`

func TestOverlayAndTypeaheadLower(t *testing.T) {
	g, err := String(overlayApp)
	if err != nil {
		t.Fatalf("overlay + typeahead should compile, got: %v", err)
	}
	kids := g.Pages[0].View[0].Children
	var sawOverlay, sawTypeahead bool
	for _, n := range kids {
		switch n.Kind {
		case "overlay":
			sawOverlay = true
			if n.Bind != "menuOpen" || len(n.Children) == 0 {
				t.Fatalf("overlay lowered wrong: %+v", n)
			}
		case "typeahead":
			sawTypeahead = true
			if n.Bind != "q" || n.Coll != "Tag" || n.Value != "name" {
				t.Fatalf("typeahead lowered wrong: %+v", n)
			}
		}
	}
	if !sawOverlay || !sawTypeahead {
		t.Fatalf("missing nodes: overlay=%v typeahead=%v", sawOverlay, sawTypeahead)
	}
}

func TestOverlayTypeaheadErrors(t *testing.T) {
	base := "app A:\n    state s: text = \"\" @client\n    state b: bool = false @client\n    entity E:\n        id: int\n        name: text\n"
	view := func(node string) string {
		return base + "    view H at \"/\":\n        box:\n            " + node + "\n"
	}
	cases := []struct{ name, src, want string }{
		{"overlay non-bool", view("overlay bind s:\n                text \"x\""), "not a bool"},
		{"typeahead unknown entity", view("typeahead bind s from Nope.name"), "unknown entity"},
		{"typeahead unknown field", view("typeahead bind s from E.nope"), "no field"},
		{"typeahead non-text", view("typeahead bind b from E.name"), "not text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := String(tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}
