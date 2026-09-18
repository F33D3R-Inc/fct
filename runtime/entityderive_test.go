package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"facet/internal/compile"
)

// ── an entity-carried `derive` is computed on read, never stored ───────────

const cartApp = `app Cart:
    entity CartLine:
        id: int
        qty: int
        unitPrice: int
        derive lineTotal: int = qty * unitPrice
    action addLine(q: int, p: int):
        add CartLine { qty: q, unitPrice: p }
    action setQty(id: int, q: int):
        set CartLine(id).qty = q
    view Home at "/":
        box:
            for l in CartLine by id:
                text "{l.lineTotal}"
`

// A `derive` written inside an `entity` block (`derive lineTotal: int = qty *
// unitPrice`) is a computed-on-read value, never a stored column: creating a
// row with real qty/unitPrice values and reading it back over the actual JSON
// API must answer with lineTotal already computed, and changing qty with a
// real `set` — going through the store, not just the in-memory copy — must
// change what the next read computes, because nothing about it is cached.
func TestEntityDeriveComputedOnRead(t *testing.T) {
	g, err := compile.String(cartApp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(apiReadEnv, "CartLine")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) int {
		t.Helper()
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	rows := func() []any {
		t.Helper()
		out, _ := getJSON(t, ts.URL+"/api/CartLine")["rows"].([]any)
		return out
	}

	if code := post("/api/addLine", `{"args":[3,150]}`); code != 200 {
		t.Fatalf("addLine should succeed, got %d", code)
	}

	r := rows()
	if len(r) != 1 {
		t.Fatalf("want 1 row, got %d", len(r))
	}
	row := r[0].(map[string]any)
	if got := toInt(row["qty"]); got != 3 {
		t.Fatalf("qty = %v, want 3", row["qty"])
	}
	if got := toInt(row["unitPrice"]); got != 150 {
		t.Fatalf("unitPrice = %v, want 150", row["unitPrice"])
	}
	if got := toInt(row["lineTotal"]); got != 450 {
		t.Fatalf("lineTotal = %v, want 450 (qty * unitPrice, computed on read)", row["lineTotal"])
	}

	// A real `set` — through the store, not a shortcut into the in-memory row —
	// changes an input the derive reads. The NEXT read must reflect it: if the
	// value were computed once at write time and cached, this would still read
	// 450.
	id := toInt(row["id"])
	if code := post("/api/setQty", `{"args":[`+strconv.Itoa(id)+`,5]}`); code != 200 {
		t.Fatalf("setQty should succeed, got %d", code)
	}
	r = rows()
	if len(r) != 1 {
		t.Fatalf("want 1 row after set, got %d", len(r))
	}
	row = r[0].(map[string]any)
	if got := toInt(row["qty"]); got != 5 {
		t.Fatalf("qty after set = %v, want 5", row["qty"])
	}
	if got := toInt(row["lineTotal"]); got != 750 {
		t.Fatalf("lineTotal after set = %v, want 750 (5 * 150) — a derive must recompute on every read, never go stale", row["lineTotal"])
	}
}

// ── the same derive, read through a keyed `Entity(id).field` lookup ────────

const cartLookupApp = `app Cart2:
    entity CartLine:
        id: int
        qty: int
        unitPrice: int
        derive lineTotal: int = qty * unitPrice
    action addLine(q: int, p: int):
        add CartLine { qty: q, unitPrice: p }
    view Home at "/":
        box:
            text "{CartLine(1).lineTotal}"
`

// `Entity(key).field` is a second, independent way to read a row's fields
// (distinct from a `for` region's rows, which go through
// listRows/visibleRows/applyDerives before a view ever touches them): this
// checks a keyed lookup resolves an entity-carried derive too, not just a
// stored column.
func TestEntityDeriveViaKeyedLookup(t *testing.T) {
	g, err := compile.String(cartLookupApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/addLine", "application/json", strings.NewReader(`{"args":[4,25]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("addLine returned %d", resp.StatusCode)
	}

	page, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	html := string(body)
	if !strings.Contains(html, "100") {
		t.Errorf("rendered page should show CartLine(1).lineTotal = 4*25 = 100, got:\n%s", html)
	}
}
