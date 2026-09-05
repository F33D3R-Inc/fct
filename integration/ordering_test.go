package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

const orderedApp = `app Ordered:
    auth
    entity Note:
        id: int
        body: text
    action write(body: text):
        add Note { body: body }
    view Home at "/":
        box:
            for n in Note by id desc limit 5:
                text "{n.body}"
`

// apiRows fetches an entity listing and returns the ids in the order served.
func apiRows(t *testing.T, a *app, query string) []int {
	t.Helper()

	code, body := a.get("/api/Note?" + query)
	if code != 200 {
		t.Fatalf("GET /api/Note?%s: %d %s", query, code, body)
	}

	var out struct {
		Rows []struct {
			ID int `json:"id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding the listing: %v", err)
	}

	ids := make([]int, len(out.Rows))
	for i, r := range out.Rows {
		ids[i] = r.ID
	}

	return ids
}

// Ordering by identity must be numeric, because identity is a number.
//
// A row's id maps onto the FacetQL node *address*, which is the store's primary
// key and its native ordering — and that ordering is over strings. Unpadded, the
// mapping did not preserve order: `Note:9` sorts after `Note:12`. So "the newest
// five" returned 9, 8, 7, 6, 5 out of twelve rows. Not an error and not an empty
// page — the wrong five, with a 200, which is the shape of bug this is worth a
// test for.
//
// Ten is the smallest count that exposes it (the first id whose text sorts below
// a shorter one), so this uses twelve.
func TestOrderingByIdIsNumericNotLexical(t *testing.T) {
	e := startEngine(t)
	// The JSON API refuses an entity the app has not published; this suite reads
	// Note listings, so it publishes Note (see fct/runtime/apiread.go).
	t.Setenv("FACET_API_READ", "Note")
	a := startApp(t, e, orderedApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}

	const rows = 12
	for i := 1; i <= rows; i++ {
		if code, body := a.action("write", "note"); code != 200 {
			t.Fatalf("write %d: %d %s", i, code, body)
		}
	}

	if got, want := apiRows(t, a, "by=id&desc=1&limit=5"), []int{12, 11, 10, 9, 8}; !equalInts(got, want) {
		t.Errorf("newest five by id = %v, want %v —\n"+
			"lexical address order puts `Note:9` after `Note:12`, so the rows\n"+
			"that are actually newest are the ones left out", got, want)
	}

	if got, want := apiRows(t, a, "by=id&limit=5"), []int{1, 2, 3, 4, 5}; !equalInts(got, want) {
		t.Errorf("oldest five by id = %v, want %v", got, want)
	}

	// The view orders the same way, and renders.
	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if !strings.Contains(html, "note") {
		t.Error("the ordered view rendered nothing")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
