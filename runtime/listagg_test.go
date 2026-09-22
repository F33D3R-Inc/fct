package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// `list(...)` yields rows (or a per-row value) as a list, ordered and capped;
// a wire `type` literal is a plain JSON object anywhere an expression goes.
// Together they let an action answer with the DTO list a typed endpoint needs.
const listAggApp = `app L:
    type ItemDTO:
        id: int
        label: text
        big: bool
    type PageDTO:
        total: int
        items: [ItemDTO]
    entity Item:
        id: int
        name: text
        n: int
    state cap: int = 2
    action seed():
        add Item { name: "a", n: 1 }
        add Item { name: "b", n: 5 }
        add Item { name: "c", n: 3 }
        add Item { name: "d", n: 4 }
    action page(min: int) -> PageDTO:
        return PageDTO{total: count(i in Item where i.n >= min), items: list(ItemDTO{id: i.id, label: i.name + "!", big: i.n > 3} in Item where i.n >= min by n desc limit cap)}
    action names() -> [text]:
        return list(i.name in Item by name)
    action rows() -> [Item]:
        return list(i in Item where i.n > 3 by id)
    derive top: text = list(i.name in Item by n desc limit 1) + ""
    view Home at "/":
        text "top={top} n={len(list(i in Item where i.n > 1))}"
`

func TestListAggregateAndWireLiterals(t *testing.T) {
	g, err := compile.String(listAggApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "seed", nil); err != nil {
		t.Fatal(err)
	}
	v, err := srv.RunValue("ada", "member", true, "page", []any{2})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v)
	want := `{"items":[{"big":true,"id":2,"label":"b!"},{"big":true,"id":4,"label":"d!"}],"total":3}`
	if string(got) != want {
		t.Fatalf("page(2) =\n  %s\nwant\n  %s", got, want)
	}
	v, _ = srv.RunValue("ada", "member", true, "names", nil)
	if got, _ := json.Marshal(v); string(got) != `["a","b","c","d"]` {
		t.Fatalf("names = %s", got)
	}
	v, _ = srv.RunValue("ada", "member", true, "rows", nil)
	if got, _ := json.Marshal(v); !strings.HasPrefix(string(got), `[{"id":2,"n":5,"name":"b"},{"id":4,`) {
		t.Fatalf("rows = %s", got)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := strings.ReplaceAll(string(page), "\n", "")
	for _, want := range []string{"top=", ">b<", "n=", ">3<"} {
		if !strings.Contains(text, want) {
			t.Errorf("the page should render %q via a list aggregate; got %s", want, text[:min(len(text), 400)])
		}
	}
}

func TestListAggregateGates(t *testing.T) {
	base := "app G:\n    type D:\n        a: int\n    entity Item:\n        id: int\n        n: int\n    derive x: int = EXPR\n    view Home at \"/\":\n        text \"{x}\"\n"
	cases := []struct{ name, expr, want string }{
		{"by on a reducer", "sum(i.n in Item by n)", "is a clause of list(...)"},
		{"unknown order field", "len(list(i in Item by nope))", "has no field \"nope\" to order list(...) by"},
		{"unknown DTO field", "len(list(D{b: 1} in Item))", "has no field \"b\""},
		{"DTO field twice", "len(list(D{a: 1, a: 2} in Item))", "set twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(base, "EXPR", c.expr, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
