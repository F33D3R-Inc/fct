package runtime

import (
	"testing"

	"facet/internal/compile"
)

// `==` and `!=` over two lists compare them element by element (and a list
// is never equal to a scalar). Every list used to compare through toInt as
// 0, so ["a"] == ["b"] held and an action's `if words != typed:` never ran.
func TestListsCompareByElement(t *testing.T) {
	g, err := compile.String(`app E:
    proc same(a: [text], b: [text]) -> bool:
        return a == b
    action check(x: text, y: text) -> text:
        let a = split(x, " ")
        let b = split(y, " ")
        if a != b:
            return "differ"
        if same(a, b):
            return "same"
        return "?"
    action nested() -> bool:
        return [[1, 2], [3]] == [[1, 2], [3]] && [[1, 2]] != [[1, 3]] && [1] != [1, 1]
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, c := range []struct{ x, y, want string }{
		{"a b", "a b", "same"}, {"a b", "a c", "differ"}, {"a", "a b", "differ"}, {"elephant sanctuary", "elefant sanctuary", "differ"},
	} {
		got, err := srv.RunValue("ada", "member", true, "check", []any{c.x, c.y})
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("check(%q, %q) = %v, want %s", c.x, c.y, got, c.want)
		}
	}
	got, err := srv.RunValue("ada", "member", true, "nested", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != true {
		t.Errorf("nested list comparison = %v, want true", got)
	}
}
