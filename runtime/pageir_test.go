package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"facet/internal/compile"
)

// pageIRApp: two pages, each dispatching its own action (one through a
// component), a guarded route, and `unused` actions and procs no page names.
func pageIRApp(unused int) string {
	var b strings.Builder
	b.WriteString(`app P:
    entity Note:
        id: int
        body: text
    state count: int = 0
    state shown: int = 2 @client
    state draft: text = "" @client
    policy member:
        actor != "guest"
    policy staff:
        role == "staff"
    action inc():
        count = count + 1
    action loadMore:
        shown = shown + 2
    action save(body: text):
        add Note { body: body }
    component Saver:
        form "Save" -> save(draft):
            input bind draft
`)
	for i := 0; i < unused; i++ {
		fmt.Fprintf(&b, "    action unused%d(x: int):\n        count = count + x * %d\n        add Note { body: \"unused %d\" }\n", i, i, i)
		fmt.Fprintf(&b, "    proc helper%d(x: int) -> int:\n        return x + %d\n", i, i)
	}
	b.WriteString(`    view Home at "/":
        text "{count}"
        button "+" -> inc()
        for n in Note by id limit shown more loadMore:
            text "{n.body}"
    view Write at "/write":
        use Saver
    view Admin at "/admin" requires member:
        text "members only"
`)
	return b.String()
}

var pageIRScript = regexp.MustCompile(`(?s)<script type="application/json" id="fa-ir">(.*?)</script>`)

func pageIRFor(t *testing.T, src, path string) (map[string]json.RawMessage, int) {
	t.Helper()
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := pageIRScript.FindSubmatch(body)
	if m == nil {
		t.Fatalf("%s: no #fa-ir (status %d): %.300s", path, resp.StatusCode, body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(m[1], &out); err != nil {
		t.Fatal(err)
	}
	return out, len(m[1])
}

func pageIRNames(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var items []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	sort.Strings(names)
	return names
}

// A page's `#fa-ir` is bounded by what the page uses: exactly the fields the
// client reads, the actions its view and components dispatch, the policies
// route guards name, and the components it reaches — so an app growing by
// actions and procs no page names grows no page by a byte.
func TestPageIRIsBoundedByWhatThePageUses(t *testing.T) {
	small, smallSize := pageIRFor(t, pageIRApp(1), "/")
	big, bigSize := pageIRFor(t, pageIRApp(60), "/")

	var keys []string
	for k := range big {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "actions,bindings,depGraph,policies,routes,states,view"; got != want {
		t.Errorf("fields shipped on /: %s, want %s (the ones facet.js reads)", got, want)
	}
	if got := pageIRNames(t, big["actions"]); strings.Join(got, ",") != "inc,loadMore" {
		t.Errorf("actions shipped on /: %v, want [inc loadMore] (the button's and the list's)", got)
	}
	if got := pageIRNames(t, big["policies"]); strings.Join(got, ",") != "member" {
		t.Errorf("policies shipped on /: %v, want [member] (the one a route guard names)", got)
	}
	if smallSize != bigSize || string(small["actions"]) != string(big["actions"]) {
		t.Errorf("59 more unused actions and procs grew / from %d to %d bytes of IR", smallSize, bigSize)
	}

	// A component's form reaches its action; the page ships the component.
	page, _ := pageIRFor(t, pageIRApp(3), "/write")
	if got := pageIRNames(t, page["actions"]); strings.Join(got, ",") != "save" {
		t.Errorf("actions shipped on /write: %v, want [save] (the component's form)", got)
	}
	if got := pageIRNames(t, page["components"]); strings.Join(got, ",") != "Saver" {
		t.Errorf("components shipped on /write: %v, want [Saver]", got)
	}
}
