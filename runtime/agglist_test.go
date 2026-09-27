package runtime

import (
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// An aggregate over a `[T]` list cell counts, tests and lists its elements
// with the item variable bound to each value — what a parsed search query's
// required words need: "every word is in the body".
func TestAggregateOverListStateEvaluates(t *testing.T) {
	g, err := compile.String(`
app Q:
    state words: [text] = [] @server
    action addWord(w: text):
        words = words + [w]
    derive hit(body: text): bool = count(w in words where !contains(body, w)) == 0
    derive has(w0: text): bool = exists(w in words where w == w0)
    view Home at "/":
        text "{hit(\"x\")}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, w := range []string{"cat", "dog"} {
		if _, err := srv.Run("ada", "member", true, "addWord", []any{w}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(src string, want any) {
		e, err := ir.CompileExpr(g, src)
		if err != nil {
			t.Fatal(err)
		}
		if got := srv.EvalExpr(e, "ada", "member", true); !equal(got, want) {
			t.Errorf("%s = %v, want %v", src, got, want)
		}
	}
	check(`len(words)`, 2)
	check(`count(words)`, 2)
	check(`count(w in words where true)`, 2)
	check(`count(w in words where w != "")`, 2)
	check(`hit("the cat and the dog")`, true)
	check(`hit("only a cat")`, false)
	check(`has("dog")`, true)
	check(`has("cow")`, false)
	check(`len(list(w in words where contains(w, "o")))`, 1)
}
