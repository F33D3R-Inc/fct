package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// `for … by <expr>`: a view list and an action loop ordered by a key computed
// per row (here a count), as `list(… by <expr>)` already is; `limit` caps
// after the ordering.
func TestForOrderedByExpression(t *testing.T) {
	g, err := compile.String(`app O:
    entity Post:
        id: int
        title: text
    entity Like:
        id: int
        post: int
    entity Pick:
        id: int
        title: text
    action seedRows:
        add Post { title: "quiet" }
        add Post { title: "loved" }
        add Post { title: "liked" }
        add Like { post: 2 }
        add Like { post: 2 }
        add Like { post: 3 }
    action pickTop:
        for p in Post where true by count(l in Like where l.post == p.id) desc limit 1:
            add Pick { title: p.title }
    state skipN: int = 2
    component Ranked(skip: cell int):
        for p in Post where true by count(l in Like where l.post == p.id && l.id > skip) desc limit 1:
            text "<{p.title}>"
    view Home at "/":
        for p in Post where true by count(l in Like where l.post == p.id) desc limit 2:
            text "[{p.title}]"
        use Ranked(skipN)
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, a := range []string{"seedRows", "pickTop"} {
		if _, err := srv.RunValue("ada", "member", true, a, nil); err != nil {
			t.Fatal(err)
		}
	}
	if rows := srv.EntityRows("Pick"); len(rows) != 1 || rows[0].(map[string]any)["title"] != "loved" {
		t.Errorf("the action loop picked %v, want loved", rows)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	page := string(httpGetBytes(t, ts.URL+"/"))
	loved, liked := strings.Index(page, "[loved]"), strings.Index(page, "[liked]")
	if loved < 0 || liked < 0 || loved > liked || strings.Contains(page, "[quiet]") {
		t.Errorf("the list is not ordered by likes and capped at 2: loved %d liked %d quiet %v", loved, liked, strings.Contains(page, "[quiet]"))
	}
	// A component's reference parameter inside its `by <expr>` is substituted
	// with the caller's cell, as everywhere else in the body:
	// skipping the first two likes leaves "liked" ahead of "loved".
	if !strings.Contains(page, "&lt;liked&gt;") || strings.Contains(page, "&lt;loved&gt;") {
		t.Errorf("the component's ordering did not see its argument: %s", page)
	}
	// A loop over a list value has no rows to key; the ordering is refused
	// rather than silently ignored.
	if _, err := compile.String("app L:\n    entity P:\n        id: int\n        t: text\n    action a:\n        let names = [\"b\", \"a\"]\n        for n in names by n + \"x\":\n            add P { t: n }\n"); err == nil || !strings.Contains(err.Error(), "walks a list value") {
		t.Errorf("`for … by <expr>` over a list local must be refused, got %v", err)
	}
	if _, err := compile.String("app B:\n    entity P:\n        id: int\n    view V at \"/\":\n        for p in P where true by nosuch(p.id) desc:\n            text \"x\"\n"); err == nil {
		t.Error("an ordering over an unknown function must not compile")
	}
}
