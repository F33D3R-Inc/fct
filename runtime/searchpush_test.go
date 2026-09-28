package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"facet/internal/compile"
)

const searchPushApp = `app SP:
    entity Post:
        id: int
        body: text
        n: int
    action seed(body: text, n: int):
        add Post { body: body, n: n }
    action search(q: text) -> text:
        return join(list(p.id in Post where contains(lower(p.body), lower(q)) && p.n != 3 by id limit 40), ",") + "|" + count(p in Post where contains(lower(p.body), lower(q))) + "|" + exists(p in Post where contains(lower(p.body), lower(q)) && p.n == 2)
    action addAndSearch(q: text) -> int:
        add Post { body: "fresh " + q, n: 9 }
        return count(p in Post where contains(lower(p.body), lower(q)))
    view Home at "/":
        text "sp"
`

// A case-insensitive search over a field the app searches is answered by the
// database's folded text index, which the boot migration declared — with the
// scan's answers, over text whose lowercase is not ASCII folding — and never
// after the request has written the entity (the database does not have that
// write yet).
func TestSearchPushdownAnswersAsTheScan(t *testing.T) {
	requireFacetQL(t)
	t.Setenv("FACET_FOLDED_INDEXES", "1")
	g, err := compile.String(searchPushApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := New(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown() })

	// the migration declared the folded index the search reads through
	url := strings.Replace(os.Getenv("FACET_DATABASE_URL"), "facetql://tok@", "http://", 1)
	req, _ := http.NewRequest("GET", url+"/admin/indexes", nil)
	req.Header.Set("x-api-key", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var listed []map[string]any
	_ = json.Unmarshal(body, &listed)
	folded := false
	for _, ix := range listed {
		if ix["kind"] == "Post" && ix["field"] == "body" && ix["mode"] == "folded" {
			folded = true
		}
	}
	if !folded {
		t.Fatalf("no folded index on Post.body was declared: %s", body)
	}

	words := []string{"İstanbul", "ÉCOLE", "école", "Kelvin", "Kelvin", "Hello", "HELLO", "world", "ΟΔΟΣ", "plain"}
	for i := 0; i < 300; i++ {
		b := fmt.Sprintf("%s post %d %s", words[i%len(words)], i, words[(i*7)%len(words)])
		if _, err := srv.Run("ada", "member", true, "seed", []any{b, i % 5}); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{"istanbul", "İSTANBUL", "école", "ÉCOLE", "kelvin", "hello", "HELLO WORLD", "οδοσ", "post 1", "st 29", "zz", "nothing-here"}
	run := func(push bool) []string {
		rowIndexing = push
		defer func() { rowIndexing = true }()
		var out []string
		for _, q := range queries {
			v, err := srv.RunValue("ada", "member", true, "search", []any{q})
			if err != nil {
				t.Fatalf("search(%q): %v", q, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		return out
	}
	scan := run(false)
	before := searchPushdown.Load()
	pushed := run(true)
	if n := searchPushdown.Load() - before; n < 10 {
		t.Fatalf("the database answered only %d searches", n)
	}
	for i := range scan {
		if scan[i] != pushed[i] {
			t.Fatalf("search(%q): scan %s, pushed %s", queries[i], scan[i], pushed[i])
		}
	}
	t.Logf("e.g. %q -> %s", queries[0], pushed[0])

	// a search after this request's own write reads the working set
	for _, q := range []string{"İstanbul", "brand new words"} {
		want, _ := srv.RunValue("ada", "member", true, "search", []any{q})
		wantCount := strings.Split(fmt.Sprint(want), "|")[1]
		got, err := srv.RunValue("ada", "member", true, "addAndSearch", []any{q})
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(toInt(wantCount)+1) {
			t.Fatalf("addAndSearch(%q) = %v, want %s + the row it added", q, got, wantCount)
		}
	}
}
