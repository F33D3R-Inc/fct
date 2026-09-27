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
