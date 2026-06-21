package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const ssrMetaApp = `app Site:
    theme:
        bg "#fff"
    theme dark:
        bg "#000"
    state n: int = 0
    view Home at "/":
        meta title "Home — Site"
        meta description "the landing page"
        box:
            text "{n}"
`

func TestSSRHeadMetadata(t *testing.T) {
	g, err := compile.String(ssrMetaApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	html := string(httpGetBytes(t, ts.URL+"/"))

	for _, want := range []string{
		"<title>Home — Site</title>",
		`<meta property="og:title" content="Home — Site">`,
		`<meta name="description" content="the landing page">`,
		`<meta property="og:description" content="the landing page">`,
		// dark-mode tokens override under the media query
		"@media(prefers-color-scheme:dark){:root{--fa-bg:#000;}}",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func TestThemeCSSVariants(t *testing.T) {
	// light only
	if got := themeCSS(map[string]string{"bg": "#fff"}, nil, nil); got != ":root{--fa-bg:#fff;}" {
		t.Errorf("light-only: %q", got)
	}
	// dark only: emits both the OS media query and a forceable [data-theme=dark] block
	if got := themeCSS(nil, map[string]string{"bg": "#000"}, nil); !strings.Contains(got, "@media(prefers-color-scheme:dark)") || !strings.Contains(got, `[data-theme="dark"]{`) {
		t.Errorf("dark-only should emit a media query and a forceable selector: %q", got)
	}
	// a named palette emits a [data-theme="<name>"] block selected at runtime
	if got := themeCSS(nil, nil, map[string]map[string]string{"pride": {"accent": "#ff0080"}}); got != `[data-theme="pride"]{--fa-accent:#ff0080;}` {
		t.Errorf("named theme: %q", got)
	}
	// neither
	if got := themeCSS(nil, nil, nil); got != "" {
		t.Errorf("no theme should be empty: %q", got)
	}
}
