package runtime

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

// A list of wire-typed values as an action parameter: a JSON body's array of
// objects, each shaped to the type (fields coerced, absent ones zero), and a
// malformed element refusing the request.
func TestListOfWireTypeParameter(t *testing.T) {
	g, err := compile.String(`app W:
    type Item:
        name: text
        qty: int
        price: float
        note: text?
    entity Line:
        id: int
        name: text
        qty: int
        price: float
        note: text
    action addItems(items: [Item]) -> int:
        for it in items:
            check it.qty > 0 "qty must be positive" status 422
            add Line { name: it.name, qty: it.qty, price: it.price, note: it.note }
        return count(Line)
    api POST "/api/items" -> addItems
    view V at "/":
        text "x"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post := func(body string) (int, string) {
		resp, err := http.Post(ts.URL+"/api/items", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, body := post(`{"items":[{"name":"tea","qty":"2","price":3},{"name":"cake","qty":1,"price":4.5,"note":"birthday"}]}`); code != 200 || body != "2" {
		t.Fatalf("addItems = %d %s", code, body)
	}
	rows := srv.EntityRows("Line")
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["qty"] != 2 || first["price"] != 3.0 || first["note"] != "" || second["note"] != "birthday" || second["price"] != 4.5 {
		t.Errorf("rows = %v", rows)
	}
	for _, bad := range []string{`{"items":[1,2]}`, `{"items":[{"name":"x","qty":"lots"}]}`, `{"items":{"name":"x"}}`} {
		if code, body := post(bad); code != 400 {
			t.Errorf("%s = %d %s, want 400", bad, code, body)
		}
	}
	if code, _ := post(`{"items":[{"name":"ok","qty":1},{"name":"zero","qty":0}]}`); code != 422 {
		t.Errorf("a failing check = %d, want 422", code)
	}
	if n := len(srv.EntityRows("Line")); n != 2 {
		t.Errorf("%d lines after the refused and rolled-back calls, want 2", n)
	}
}
