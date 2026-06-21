package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

func renderHome(t *testing.T, src string) string {
	t.Helper()
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	// seed a Tag so the typeahead has a suggestion
	if a := srv.byAction["addTag"]; a != nil {
		srv.runAction(systemSID, a, []any{"urgent"})
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	return string(httpGetBytes(t, ts.URL+"/"))
}

const overlayHidden = `app UI:
    state menuOpen: bool = false @client
    state q: text = "" @client
    entity Tag:
        id: int
        name: text
    action addTag(name: text):
        add Tag { name: name }
    view Home at "/":
        box:
            typeahead bind q from Tag.name placeholder "find"
            overlay bind menuOpen:
                box:
                    text "secret menu"
`

func TestOverlayHiddenAndTypeahead(t *testing.T) {
	html := renderHome(t, overlayHidden)
	// overlay is closed (menuOpen=false): no backdrop instance is rendered. (The
	// children still ship in the IR JSON so the client can open it — the rendered
	// `data-fa-close` attribute is the reliable "is it shown" signal.)
	if strings.Contains(html, `data-fa-close="menuOpen"`) {
		t.Error("a closed overlay must not render its backdrop instance")
	}
	// typeahead renders an input wired to a datalist of the entity's field values
	if !strings.Contains(html, `data-fa-input="q" list="ta-`) {
		t.Errorf("typeahead input/list missing:\n%s", html)
	}
	if !strings.Contains(html, `<option value="urgent">`) {
		t.Error("typeahead datalist should include the seeded tag value")
	}
}

const overlayOpen = `app UI:
    state menuOpen: bool = true @client
    view Home at "/":
        box:
            overlay bind menuOpen:
                box:
                    text "shown menu"
`

func TestOverlayShownWhenTrue(t *testing.T) {
	html := renderHome(t, overlayOpen)
	if !strings.Contains(html, `data-fa-close="menuOpen"`) {
		t.Error("an open overlay should render a backdrop tagged with its bound cell")
	}
	if !strings.Contains(html, "shown menu") {
		t.Error("an open overlay should render its contents")
	}
}
