package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"facet/internal/compile"
)

// Request→response: a server action binds a brain's typed answer with `let x =
// call …` and assigns it into authoritative state, which surfaces to the client
// as a delta. The whole point of the feature — fct as the edge that reads the mesh.
const meshApp = `app Mesh:
    service Brain at "http://placeholder.invalid":
        answer(q: int) -> int
    state result: int = 0
    action ask(q: int):
        let a = call Brain.answer(q)
        result = a
    view Home at "/":
        box:
            text "{result}"
`

func TestServiceRequestResponseBindsResult(t *testing.T) {
	// A fake brain that doubles whatever it's asked and answers with a {"result"}
	// envelope — exercising both the round-trip and the envelope-unwrap.
	var gotBody map[string]any
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		q, _ := gotBody["q"].(float64)
		json.NewEncoder(w).Encode(map[string]any{"result": int(q) * 2})
	}))
	defer brain.Close()

	g, err := compile.String(meshApp)
	if err != nil {
		t.Fatal(err)
	}
	g.Services[0].URL = brain.URL // point the declared brain at the fake

	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/ask", "application/json", strings.NewReader(`{"args":[21]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ask returned %d", resp.StatusCode)
	}
	var out struct {
		Deltas map[string]any `json:"deltas"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if got := toInt(out.Deltas["result"]); got != 42 {
		t.Fatalf("bound result = %v, want 42 (21*2 from the brain)", got)
	}
	// the argument reached the brain under its declared parameter name.
	if gotBody["q"] != float64(21) {
		t.Errorf("brain received %v for q, want 21", gotBody["q"])
	}
}

func TestServiceRequestResponseFailureAborts(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "brain down", http.StatusInternalServerError)
	}))
	defer brain.Close()

	g, err := compile.String(meshApp)
	if err != nil {
		t.Fatal(err)
	}
	g.Services[0].URL = brain.URL
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/ask", "application/json", strings.NewReader(`{"args":[1]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a failed brain call should abort the action with 502, got %d", resp.StatusCode)
	}
}

func TestCoerceRet(t *testing.T) {
	cases := []struct {
		v    any
		ret  string
		list bool
		want any
	}{
		{float64(42), "int", false, 42},
		{"tier2", "text", false, "tier2"},
		{[]any{float64(3), float64(1), float64(2)}, "int", true, []any{3, 1, 2}},
		{float64(7), "int", true, []any{7}}, // a lone value where a list was declared
		{nil, "int", true, []any{}},         // a null list
	}
	for i, c := range cases {
		if got := coerceRet(c.v, c.ret, c.list); !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: coerceRet(%v,%q,%v) = %#v, want %#v", i, c.v, c.ret, c.list, got, c.want)
		}
	}
}
