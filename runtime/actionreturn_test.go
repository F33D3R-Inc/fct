package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// An action may declare `-> Type` and end in `return expr`: the value is the
// reply's `value` over /event and /api/<action>, and RunValue for the tooling.
// A `return` inside a `for`/`if` ends the body there.
const returnApp = `app R:
    type Summary:
        total: int
        top: text
    entity Item:
        id: int
        name: text
        n: int
    state hits: int = 0
    action seed():
        add Item { name: "a", n: 1 }
        add Item { name: "b", n: 5 }
        add Item { name: "c", n: 3 }
    action total() -> int:
        hits = hits + 1
        return sum(Item.n)
    action first(min: int) -> Item:
        for i in Item where i.n >= min by n desc:
            return i
    action bigger(min: int) -> text:
        for i in Item by n desc:
            if i.n > min:
                return i.name
        return "none"
    action noValue():
        hits = hits + 10
        return
    view Home at "/":
        text "{hits}"
`

func TestActionReturnValues(t *testing.T) {
	g, err := compile.String(returnApp)
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
	v, err := srv.RunValue("ada", "member", true, "total", nil)
	if err != nil || toInt(v) != 9 {
		t.Fatalf("total: %v, %v", v, err)
	}
	if got := srv.StateValue("hits"); toInt(got) != 1 {
		t.Fatalf("a returning action still applies its writes: hits = %v", got)
	}
	row, err := srv.RunValue("ada", "member", true, "first", []any{2})
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := row.(record); !ok || r["name"] != "b" {
		t.Fatalf("first(2) should return the row of b, got %#v", row)
	}
	for min, want := range map[int]string{4: "b", 10: "none"} {
		v, err := srv.RunValue("ada", "member", true, "bigger", []any{min})
		if err != nil || v != want {
			t.Fatalf("bigger(%d) = %v (%v), want %q", min, v, err, want)
		}
	}
	if _, err := srv.Run("ada", "member", true, "noValue", nil); err != nil {
		t.Fatal(err)
	}
	if got := srv.StateValue("hits"); toInt(got) != 11 {
		t.Fatalf("a bare return still commits earlier writes: hits = %v", got)
	}

	// Over the wire: the JSON reply carries `value` only for a returning action.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body, _ := json.Marshal(map[string]any{"args": []any{}})
	resp, err := http.Post(ts.URL+"/api/total", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if toInt(out["value"]) != 9 {
		t.Fatalf("/api/total reply = %v, want value 9", out)
	}
	resp, _ = http.Post(ts.URL+"/api/seed", "application/json", strings.NewReader(string(body)))
	out = map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if _, has := out["value"]; has {
		t.Fatalf("a no-return action must not carry a value key: %v", out)
	}
}

func TestActionReturnGates(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"value without a return type", "    action f():\n        return 1\n", "declares no return type"},
		{"bare return with a return type", "    action f() -> int:\n        return\n", "needs a value"},
		{"proc-only return type", "    struct S:\n        x: int\n    action f() -> S:\n        return 1\n", "proc-only type"},
		{"unknown return type", "    action f() -> Nope:\n        return 1\n", "unknown type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String("app G:\n" + c.src + "    view Home at \"/\":\n        text \"x\"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
