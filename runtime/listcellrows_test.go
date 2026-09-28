package runtime

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A page lists the rows a session's `[int]` cell names, in the cell's order
// (`for p in Post where p.id in hits by indexOf(hits, p.id, 0)`) — the cell
// set by an action from another action's reply — and a float cell filters
// as a float. (The row filter folds what does not read the row into a
// literal first; a list folded to the text "3,1" and a float to an int.)
func TestRowsNamedByAListCell(t *testing.T) {
	g, err := compile.String(`app H:
    entity Post:
        id: int
        title: text
        score: float
    state cutoff: float = 0.5
    type Res:
        ids: [int]
    state hits: [int] = []
    action seed():
        add Post { title: "one", score: 0.2 }
        add Post { title: "two", score: 0.7 }
        add Post { title: "three", score: 0.4 }
    action pick() -> Res @internal:
        return Res{ids: [3, 1]}
    action go():
        let r = run pick()
        hits = r.ids
    view Home at "/":
        for p in Post where p.id in hits by indexOf(hits, p.id, 0):
            text "[{p.title}]"
        for p in Post where p.score > cutoff:
            text "<{p.title}>"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.RunValue("ada", "member", true, "seed", nil); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := jarClient(t)
	if _, code := apiCall(t, c, ts.URL+"/api/go", "{}"); code != 200 {
		t.Fatalf("go: %d", code)
	}
	res, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	page := string(b)
	three, one := strings.Index(page, "[three]"), strings.Index(page, "[one]")
	if !strings.Contains(page, "&lt;two&gt;") || strings.Contains(page, "&lt;three&gt;") || strings.Contains(page, "&lt;one&gt;") {
		t.Errorf("a float cell must filter as a float: only two scores above 0.5\n%s", page)
	}
	if three < 0 || one < 0 || three > one || strings.Contains(page, "[two]") {
		t.Errorf("want [three] then [one] and not [two]: three %d one %d\n%s", three, one, page)
	}
}
