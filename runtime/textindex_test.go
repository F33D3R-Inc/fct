package runtime

import (
	"math/rand"
	"strings"
	"testing"

	"facet/internal/compile"
)

// runeSlice/runeLen replaced the `[]rune(s)` implementations of slice, take,
// charAt and len. They must agree with that reference on every input — ASCII
// and not, short and long enough to hit the cache, every clamp case.
func TestRuneSliceMatchesRuneConversion(t *testing.T) {
	ref := func(s string, start, end int) string {
		r := []rune(s)
		if start < 0 {
			start = 0
		}
		if start > len(r) {
			start = len(r)
		}
		if end < 0 {
			end = 0
		}
		if end > len(r) {
			end = len(r)
		}
		if end < start {
			end = start
		}
		return string(r[start:end])
	}
	rng := rand.New(rand.NewSource(7))
	alphabet := []rune("ab{}\":,ü日本😀é\n")
	for trial := 0; trial < 400; trial++ {
		n := rng.Intn(300)
		if trial%3 == 0 {
			n += 200 // long enough for the ASCII cache
		}
		var b strings.Builder
		ascii := trial%2 == 0
		for i := 0; i < n; i++ {
			if ascii {
				b.WriteRune(alphabet[rng.Intn(7)])
			} else {
				b.WriteRune(alphabet[rng.Intn(len(alphabet))])
			}
		}
		s := b.String()
		rl := len([]rune(s))
		if got := runeLen(s); got != rl {
			t.Fatalf("runeLen(%q) = %d, want %d", s, got, rl)
		}
		for k := 0; k < 20; k++ {
			start, end := rng.Intn(rl+4)-2, rng.Intn(rl+4)-2
			if got, want := runeSlice(s, start, end), ref(s, start, end); got != want {
				t.Fatalf("runeSlice(%q, %d, %d) = %q, want %q", s, start, end, got, want)
			}
		}
		for i := -1; i <= rl; i++ {
			if got, want := callBuiltin("charAt", []any{s, i}), ref(s, i, i+1); got != want {
				t.Fatalf("charAt(%q, %d) = %q, want %q", s, i, got, want)
			}
		}
	}
}

// The `x = x + e` fast path must be invisible: a proc that grows a string by
// self-concatenation, reassigns it outright part-way (which must reset the
// builder), and reads it inside the loop, produces exactly what plain
// concatenation would.
func TestAppendTextFastPathIsInvisible(t *testing.T) {
	src := `app A:
    proc build(n: int) -> text:
        let mut out = ""
        let mut i = 0
        loop i < n:
            out = out + "<" + i + ">"
            if i == 3:
                out = "reset:" + out
            if len(out) > 1000:
                out = "x"
            i = i + 1
        let mut other = out
        other = other + "!"
        return out + "|" + other
    state got: text = ""
    action go(n: int):
        let r = do build(n)
        got = r
    view Home at "/":
        text "{got}"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	want := func(n int) string {
		out := ""
		for i := 0; i < n; i++ {
			out = out + "<" + itoa(i) + ">"
			if i == 3 {
				out = "reset:" + out
			}
			if len(out) > 1000 {
				out = "x"
			}
		}
		other := out + "!"
		return out + "|" + other
	}
	for _, n := range []int{0, 1, 4, 7, 300} {
		if _, err := srv.Run("ada", "member", true, "go", []any{n}); err != nil {
			t.Fatal(err)
		}
		if got := srv.StateValue("got"); got != want(n) {
			t.Fatalf("n=%d: got %q, want %q", n, got, want(n))
		}
	}
}
