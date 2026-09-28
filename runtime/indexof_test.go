package runtime

// indexOf(s, sub, from) and the proc engine's native text-position paths
// (proccompile.go: charAt / slice / len / indexOf over native ints): the
// rune indexing slice and charAt use, on ASCII and non-ASCII text, at
// positions past the boxed-int cache (65536), with every clamp.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

func TestRuneIndexOf(t *testing.T) {
	cases := []struct {
		s, sub string
		from   int
		want   int
	}{
		{"hello world", "o", 0, 4},
		{"hello world", "o", 5, 7},
		{"hello world", "o", 8, -1},
		{"hello world", "", 3, 3},
		{"hello world", "x", 0, -1},
		{"hello", "l", -5, 2},
		{"hello", "l", 99, -1},
		{"héllo wörld", "w", 0, 6},
		{"héllo wörld", "ö", 0, 7},
		{"héllo wörld", "l", 3, 3},
		{"héllo wörld", "l", 4, 9},
		{"日本語テキスト", "テ", 0, 3},
		{"日本語テキスト", "語", 3, -1},
	}
	for _, c := range cases {
		if got := runeIndexOf(c.s, c.sub, c.from); got != c.want {
			t.Errorf("indexOf(%q, %q, %d) = %d, want %d", c.s, c.sub, c.from, got, c.want)
		}
	}
}

const textPositionsApp = `app A:
    proc scan(s: text, needle: text) -> text:
        let at = indexOf(s, needle, 0)
        let again = indexOf(s, needle, at + 1)
        let nowhere = indexOf(s, "zzz", 0)
        let c = charAt(s, at)
        let far = charAt(s, len(s) - 1)
        let neg = charAt(s, -1)
        let past = charAt(s, len(s) + 5)
        let mid = slice(s, at, at + len(needle))
        let clamped = slice(s, len(s) - 2, len(s) + 100)
        let empty = slice(s, 5, 2)
        return "" + at + "," + again + "," + nowhere + "," + c + "," + far + "," + neg + "|" + past + "|" + mid + "," + clamped + "," + empty + "|" + len(s)
    state result: text = ""
    action run(s: text, needle: text):
        let r = do scan(s, needle)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestTextPositionsPastBoxedRange(t *testing.T) {
	g, err := compile.String(textPositionsApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// 70000 ASCII bytes then the needle (positions past 65535), and the same
	// with a multi-byte prefix so rune and byte positions differ.
	long := strings.Repeat("a", 70000) + "needle" + strings.Repeat("b", 10) + "needle" + "xy"
	wide := strings.Repeat("é", 70000) + "needle" + strings.Repeat("ö", 10) + "needle" + "xy"
	cases := []struct{ s, want string }{
		{long, "70000,70016,-1,n,y,|" + "|needle,xy,|70024"},
		{wide, "70000,70016,-1,n,y,|" + "|needle,xy,|70024"},
	}
	for _, c := range cases {
		body := `{"args":[` + jsonString(c.s) + `,"needle"]}`
		deltas := postJSON(t, ts, "run", body)
		if got := deltas["result"]; got != c.want {
			t.Fatalf("result = %v, want %s", got, c.want)
		}
	}
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// indexOf over a list: the first element `==` to the value at or after
// from — equal's comparison, so a number matches its spelling as `==`
// has it — or -1.
func TestListIndexOf(t *testing.T) {
	xs := []any{"a", "b", "a", 7, "7", 2.5}
	cases := []struct {
		v    any
		from int
		want int
	}{
		{"a", 0, 0}, {"a", 1, 2}, {"a", 3, -1}, {"b", -4, 1}, {"zz", 0, -1},
		{7, 0, 3}, {"7", 0, 3}, {2.5, 0, 5}, {"a", 99, -1},
	}
	for _, c := range cases {
		if got := listIndexOf(xs, c.v, c.from); got != c.want {
			t.Errorf("indexOf(xs, %v, %d) = %d, want %d", c.v, c.from, got, c.want)
		}
		if got := toInt(callBuiltin("indexOf", []any{xs, c.v, c.from})); got != c.want {
			t.Errorf("builtin indexOf(xs, %v, %d) = %d, want %d", c.v, c.from, got, c.want)
		}
	}
}

const listIndexApp = `app A:
    proc find(names: [text], name: text) -> int:
        return indexOf(names, name, 0)
    proc run() -> text:
        let names = ["x", "y", "z", "y"]
        let at = indexOf(names, "y", 2) + 1
        return "" + find(names, "z") + "," + find(names, "q") + "," + at
    state result: text = ""
    action run:
        let r = do run()
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestListIndexOfInProcs(t *testing.T) {
	g, err := compile.String(listIndexApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if got := postJSON(t, ts, "run", `{"args":[]}`)["result"]; got != "2,-1,4" {
		t.Fatalf("result = %v, want 2,-1,4", got)
	}
}

// runeIndexOf agrees with a []rune reference on random text either side of
// the short-text bound (shortText: indexed into a stack buffer below it,
// through the cache from it up), ASCII and not, from every clamp.
func TestRuneIndexOfMatchesRuneReference(t *testing.T) {
	ref := func(s, sub string, from int) int {
		r := []rune(s)
		if from < 0 {
			from = 0
		}
		if from > len(r) {
			from = len(r)
		}
		k := strings.Index(string(r[from:]), sub)
		if k < 0 {
			return -1
		}
		return from + len([]rune(string(r[from:])[:k]))
	}
	alphabet := []rune("ab-é日😀")
	seed := uint32(11)
	next := func(n int) int {
		seed = seed*1664525 + 1013904223
		return int(seed>>8) % n
	}
	for trial := 0; trial < 600; trial++ {
		var b strings.Builder
		for b.Len() < shortText-8+trial%20 && (trial%3 != 0 || b.Len() < 300) {
			b.WriteRune(alphabet[next(len(alphabet))])
		}
		s := b.String()
		rl := len([]rune(s))
		sub := string([]rune(s)[next(rl+1):][:0])
		if rl > 2 {
			at := next(rl - 1)
			sub = string([]rune(s)[at : at+1+next(2)])
		}
		for k := 0; k < 6; k++ {
			from := next(rl+6) - 3
			if got, want := runeIndexOf(s, sub, from), ref(s, sub, from); got != want {
				t.Fatalf("indexOf(%q, %q, %d) = %d, want %d", s, sub, from, got, want)
			}
		}
		if got := runeLen(s); got != rl {
			t.Fatalf("runeLen(%q) = %d, want %d", s, got, rl)
		}
	}
}
