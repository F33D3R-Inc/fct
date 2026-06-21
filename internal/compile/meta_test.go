package compile

import (
	"strings"
	"testing"
)

// `meta title` / `meta description` are page metadata, interpolated and lowered to
// server-evaluated segments (not reactive client binds). `theme dark:` lowers to a
// separate token map for the dark color scheme.
const metaApp = `app Site:
    theme:
        bg "#fff"
    theme dark:
        bg "#000"
    entity Post:
        id: int
        title: text
    view Home at "/":
        meta title "Site — home"
        meta description "welcome"
        box:
            text "hi"
    view Read at "/post/:id":
        meta title "{Post(id).title}"
        box:
            text "static body"
`

func TestMetaAndDarkThemeLower(t *testing.T) {
	g, err := String(metaApp)
	if err != nil {
		t.Fatalf("meta + dark theme should compile, got: %v", err)
	}
	if g.Theme["bg"] != "#fff" || g.ThemeDark["bg"] != "#000" {
		t.Fatalf("theme/dark not lowered: light=%v dark=%v", g.Theme, g.ThemeDark)
	}
	for i := range g.Pages {
		p := g.Pages[i]
		switch p.Name {
		case "Home":
			if len(p.Title) == 0 || len(p.Desc) == 0 {
				t.Fatalf("Home should have title+desc, got title=%v desc=%v", p.Title, p.Desc)
			}
			if p.Title[0].Lit != "Site — home" {
				t.Fatalf("Home title literal wrong: %+v", p.Title)
			}
		case "Read":
			// dynamic title interpolates an entity lookup → a server-evaluated Expr seg
			if len(p.Title) == 0 || p.Title[0].Expr == nil {
				t.Fatalf("Read title should be an Expr seg, got %+v", p.Title)
			}
			// a metadata seg must NOT create a reactive client binding
			if len(p.Bindings) != 0 {
				t.Fatalf("page metadata must not add client bindings, got %d", len(p.Bindings))
			}
		}
	}
}

func TestMetaBadDirective(t *testing.T) {
	_, err := String(`app A:
    view H at "/":
        meta foo "x"
        box:
            text "y"`)
	if err == nil || !strings.Contains(err.Error(), "meta takes title or description") {
		t.Fatalf("want meta directive error, got: %v", err)
	}
}
