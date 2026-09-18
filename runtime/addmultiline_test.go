package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A multi-line `add Entity { ... }` — fields on separate lines, the closing
// `}` indented under `add` the same way an `if`/`for` body is indented under
// its header — must run exactly like the single-line form: the row lands in
// the store with every field carrying the value the action gave it.
const multilineAddRuntimeApp = `app Catalog:
    entity Widget:
        id: int
        name: text
        qty: int
        price: int
    action make(name: text, qty: int, price: int):
        add Widget {
            name: name,
            qty: qty,
            price: price
            }
    view Home at "/":
        box:
            for w in Widget by id:
                text "{w.name}"
`

func TestMultilineAddRecordLandsInStore(t *testing.T) {
	g, err := compile.String(multilineAddRuntimeApp)
	if err != nil {
		t.Fatalf("a multi-line add record should compile, got: %v", err)
	}
	t.Setenv(apiReadEnv, "Widget")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	r, err := http.Post(ts.URL+"/api/make", "application/json",
		strings.NewReader(`{"args":["lantern",3,1200]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("make: want 200, got %d", r.StatusCode)
	}

	rows, _ := getJSON(t, ts.URL+"/api/Widget")["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %v", len(rows), rows)
	}
	row := rows[0].(map[string]any)
	if row["name"] != "lantern" {
		t.Errorf("name = %v, want %q", row["name"], "lantern")
	}
	if toInt(row["qty"]) != 3 {
		t.Errorf("qty = %v, want 3", row["qty"])
	}
	if toInt(row["price"]) != 1200 {
		t.Errorf("price = %v, want 1200", row["price"])
	}
}
