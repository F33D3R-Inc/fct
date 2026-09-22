package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// TestActionMatchDispatch proves an action `match` statement (desugared at
// parse time into a nested if/else chain — internal/parser/parser.go) really
// dispatches: each `case` fires its own branch and an unmatched value falls
// to `else`, over the live server, not just at compile time.
func TestActionMatchDispatch(t *testing.T) {
	src := `app A:
    state result: text = ""
    action dispatch(kind: text):
        match kind:
            case "insert_node":
                result = "insert"
            case "delete_node":
                result = "delete"
            else:
                result = "unknown"
    view Home at "/":
        box:
            text "{result}"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	cases := map[string]string{"insert_node": "insert", "delete_node": "delete", "nope": "unknown"}
	for kind, want := range cases {
		body, _ := json.Marshal(map[string]any{"args": []any{kind}})
		resp, err := http.Post(ts.URL+"/api/dispatch", "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		deltas, _ := out["deltas"].(map[string]any)
		if got := deltas["result"]; got != want {
			t.Errorf("dispatch(%q) result = %v, want %v", kind, got, want)
		}
	}
}

// TestActionMatchRequiresElse proves an action match without an `else` is a
// compile error — a message's variants are not an enum the compiler can
// prove exhaustive coverage against, so the default arm is mandatory.
func TestActionMatchRequiresElse(t *testing.T) {
	src := `app A:
    state result: text = ""
    action dispatch(kind: text):
        match kind:
            case "a":
                result = "x"
    view Home at "/":
        box:
            text "{result}"
`
	_, err := compile.String(src)
	if err == nil || !strings.Contains(err.Error(), "must be exhaustive") {
		t.Fatalf("want an exhaustiveness error, got %v", err)
	}
}
