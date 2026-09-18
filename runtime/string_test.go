package runtime

// ── String decomposition primitives: split/slice/charAt, live over HTTP ────
//
// Before this milestone, `.fct`'s only way to decompose a `text` value was
// `take` (prefix-only), `contains`, `trim`, `upper`/`lower`, and `len` — no
// general substring, no split-on-separator, no character-at-index. That made
// `strings.Split(src, "\n")`, the literal first line of internal/source.go's
// real lexing algorithm, impossible to express in a proc: this is what
// selfhost/source.fct's port of it hit. split/slice/charAt below close that
// gap.
//
// Byte-vs-rune decision: `take`/`len` (runtime/eval.go's callBuiltin, "take"/
// "len" cases) already operate on runes — `take` converts its input to
// []rune before slicing, and `len` uses utf8.RuneCountInString for a text
// argument — not raw bytes. split/slice/charAt below are deliberately
// consistent with that: slice/charAt convert to []rune before indexing, and
// split, while implemented as a direct call to Go's strings.Split, is
// rune-safe for the same reason strings.Contains/Index already are — UTF-8
// is self-synchronizing, so a byte-for-byte search for a valid UTF-8
// separator can only ever match at rune boundaries. TestSplitOnMultiByteRunes
// and TestSliceIsRuneIndexedNotByteIndexed below prove this explicitly with
// non-ASCII input.
//
// Out-of-range decision: slice/charAt never raise a runtime error — they
// clamp, exactly like take's existing convention for an n longer than the
// string (runtime/eval.go's "take" case: n is clamped into [0, len(r)]).
// slice clamps both start and end into [0, len(r)] independently, and if
// start ends up past end after clamping, the result is "" rather than an
// error. charAt(s, i) is defined as exactly slice(s, i, i+1), so an
// out-of-range i (negative, or >= len) also yields "" rather than erroring.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"facet/internal/compile"
)

// stringOpsApp exercises split/slice/charAt individually (via one action per
// builtin, each binding a proc's result into its own state cell) and
// composed together in lexerShape — a real lexer-shaped mini-algorithm: split
// a small multi-line source into lines, then for each line (via a loop)
// accumulate its rune length and check whether it starts with a space, the
// exact two operations internal/source.go's real indentation-tree lexer needs
// per line. The combined result is encoded as totalLen*1000 + indentCount so
// one int round-trips both numbers over the wire.
const stringOpsApp = `app A:
    proc doSplit(s: text, sep: text) -> [text]:
        return split(s, sep)
    proc doSlice(s: text, start: int, end: int) -> text:
        return slice(s, start, end)
    proc doCharAt(s: text, i: int) -> text:
        return charAt(s, i)
    proc lexerShape(src: text) -> int:
        let lines = split(src, "\n")
        let mut totalLen = 0
        let mut indentCount = 0
        let mut i = 0
        loop i < len(lines):
            let line = lines[i]
            totalLen = totalLen + len(line)
            if len(line) > 0:
                if charAt(line, 0) == " ":
                    indentCount = indentCount + 1
            i = i + 1
        return totalLen * 1000 + indentCount
    state parts: [text] = []
    state sliced: text = ""
    state ch: text = ""
    state lexResult: int = 0
    action runSplit(s: text, sep: text):
        let r = do doSplit(s, sep)
        parts = r
    action runSlice(s: text, start: int, end: int):
        let r = do doSlice(s, start, end)
        sliced = r
    action runCharAt(s: text, i: int):
        let r = do doCharAt(s, i)
        ch = r
    action runLexer(src: text):
        let r = do lexerShape(src)
        lexResult = r
    view Home at "/":
        box:
            text "{len(parts)}"
`

func newStringOpsServer(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.String(stringOpsApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// toStrSlice converts a wire-decoded `[]any` of strings (JSON numbers decode
// to float64, JSON strings to string — every element here is always a
// string) into a plain []string for comparison against strings.Split.
func toStrSlice(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a []any, got %T (%v)", v, v)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("element %d is %T (%v), want string", i, e, e)
		}
		out[i] = s
	}
	return out
}

func argsJSON(t *testing.T, args ...any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSplitMatchesGoStringsSplitExactly cross-checks split(s, sep) against
// Go's own strings.Split, live over HTTP, for every edge case Go's own docs
// call out: a normal multi-field split, consecutive separators (empty-string
// elements), no separator present (a single-element result), and a
// leading/trailing separator (empty leading/trailing elements) — plus the
// exact motivating case: splitting a small multi-line `.fct`-source-shaped
// string on "\n", which is literally internal/source.go's first line
// (strings.Split(src, "\n")) that a proc could not express before this.
func TestSplitMatchesGoStringsSplitExactly(t *testing.T) {
	ts := newStringOpsServer(t)
	cases := []struct {
		name, s, sep string
	}{
		{"normal multi-field", "a,b,c,d", ","},
		{"consecutive separators", "a,,b,,,c", ","},
		{"separator not present", "abcdef", ","},
		{"leading and trailing separator", ",a,b,", ","},
		{"multi-char separator", "one::two::three", "::"},
		{
			"a small .fct-source-shaped string on newline, exactly source.go's own strings.Split(src, \"\\n\")",
			"app A:\n    state count: int = 0\n\n    view Home at \"/\":\n        text \"{count}\"\n",
			"\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := postJSON(t, ts, "runSplit", argsJSON(t, c.s, c.sep))
			got := toStrSlice(t, d["parts"])
			want := strings.Split(c.s, c.sep)
			if len(got) != len(want) {
				t.Fatalf("split(%q, %q) = %#v (%d parts), want %#v (%d parts)", c.s, c.sep, got, len(got), want, len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("split(%q, %q)[%d] = %q, want %q (Go: %#v)", c.s, c.sep, i, got[i], want[i], want)
				}
			}
		})
	}
}

// TestSplitOnMultiByteRunes proves split is safe on non-ASCII input: an empty
// separator splits after every UTF-8 sequence (a multi-byte rune stays one
// element, never cut in half), matching Go's strings.Split(s, "") exactly —
// the sharpest test of "byte-based would silently corrupt this."
func TestSplitOnMultiByteRunes(t *testing.T) {
	ts := newStringOpsServer(t)
	s := "héllo wörld"
	d := postJSON(t, ts, "runSplit", argsJSON(t, s, ""))
	got := toStrSlice(t, d["parts"])
	want := strings.Split(s, "")
	if len(got) != len(want) {
		t.Fatalf("split(%q, \"\") = %#v (%d runes), want %#v (%d runes)", s, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("split(%q, \"\")[%d] = %q, want %q", s, i, got[i], want[i])
		}
	}
}

// TestSliceMatchesGoSubstringing cross-checks slice(s, start, end) against
// Go's own s[start:end] for in-range inputs, and proves the documented
// clamp-never-error convention for out-of-range start/end — mirroring take's
// existing "clamp an n longer than the string" behavior instead of raising a
// new, inconsistent error path.
func TestSliceMatchesGoSubstringing(t *testing.T) {
	ts := newStringOpsServer(t)
	cases := []struct {
		name       string
		s          string
		start, end int
		want       string
	}{
		{"normal in-range substring", "hello world", 0, 5, "hello world"[0:5]},
		{"a middle slice", "hello world", 6, 11, "hello world"[6:11]},
		{"empty range at a valid position", "hello", 2, 2, ""},
		{"negative start clamps to 0", "abc", -5, 2, "ab"},
		{"end beyond length clamps to len", "abc", 1, 100, "bc"},
		{"both out of range clamp to the full string", "abc", -5, 100, "abc"},
		{"start left past end after clamping yields empty, not an error", "abc", 5, 2, ""},
		{"start equal to len yields empty", "abc", 3, 3, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := postJSON(t, ts, "runSlice", argsJSON(t, c.s, c.start, c.end))
			got, _ := d["sliced"].(string)
			if got != c.want {
				t.Errorf("slice(%q, %d, %d) = %q, want %q", c.s, c.start, c.end, got, c.want)
			}
		})
	}
}

// TestSliceIsRuneIndexedNotByteIndexed proves slice indexes by rune, not
// byte, matching take/len's existing rune convention: a naive byte-index
// slice on this input would cut the multi-byte 'é'/'ö' in half and either
// panic or produce invalid UTF-8. Cross-checked against []rune(s)[start:end]
// — the same rune-slicing Go code would need to write by hand for
// rune-correct behavior.
func TestSliceIsRuneIndexedNotByteIndexed(t *testing.T) {
	ts := newStringOpsServer(t)
	s := "héllo wörld"
	r := []rune(s)
	start, end := 1, 4
	d := postJSON(t, ts, "runSlice", argsJSON(t, s, start, end))
	got, _ := d["sliced"].(string)
	want := string(r[start:end])
	if got != want {
		t.Fatalf("slice(%q, %d, %d) = %q, want %q (rune-indexed, matching []rune(s)[%d:%d])", s, start, end, got, want, start, end)
	}
	if got != "éll" {
		t.Fatalf("slice(%q, 1, 4) = %q, want \"éll\" — a byte-indexed implementation would have produced invalid UTF-8 or a different, wrong slice here", s, got)
	}
}

// TestCharAt cross-checks charAt(s, i) — a length-1 string, this language's
// stand-in for a dedicated character/rune type — for a normal in-range
// index, a multi-byte rune at that index (proving rune-, not byte-, indexing,
// same as slice), and the out-of-range clamp convention (negative or >= len
// yields "" rather than an error, since charAt is defined as exactly
// slice(s, i, i+1)).
func TestCharAt(t *testing.T) {
	ts := newStringOpsServer(t)
	cases := []struct {
		name string
		s    string
		i    int
		want string
	}{
		{"first character", "hello", 0, "h"},
		{"a middle character", "hello", 4, "o"},
		{"a multi-byte rune at the index, not its first byte", "héllo", 1, "é"},
		{"negative index clamps to empty", "abc", -1, ""},
		{"index equal to len clamps to empty", "abc", 3, ""},
		{"index far past len clamps to empty", "abc", 999, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := postJSON(t, ts, "runCharAt", argsJSON(t, c.s, c.i))
			got, _ := d["ch"].(string)
			if got != c.want {
				t.Errorf("charAt(%q, %d) = %q, want %q", c.s, c.i, got, c.want)
			}
		})
	}
}

// TestLexerShapedAlgorithmComposesSplitSliceLen is the combined proof the
// task asks for: split + charAt + len used together in a `loop`, doing
// exactly what a real lexer's per-line pass needs — split a small multi-line
// source into lines, then for each line compute its rune length and check
// its first character — the same two questions internal/source.go's real
// indentation-tree lexer asks of every line it scans. The proc encodes both
// running totals into one int (totalLen*1000 + indentCount); this test
// decodes it and cross-checks both halves against an independent computation
// in Go over the same source string, using the same rune-based len() the
// runtime itself uses (utf8.RuneCountInString), so a bug in either total
// would surface as a mismatch here.
func TestLexerShapedAlgorithmComposesSplitSliceLen(t *testing.T) {
	ts := newStringOpsServer(t)
	src := "app A:\n    state héllo: int = 0\n\n        text \"hi\"\n"

	lines := strings.Split(src, "\n")
	wantTotalLen, wantIndentCount := 0, 0
	for _, line := range lines {
		wantTotalLen += utf8.RuneCountInString(line)
		if len(line) > 0 {
			first := []rune(line)[0]
			if first == ' ' {
				wantIndentCount++
			}
		}
	}
	want := wantTotalLen*1000 + wantIndentCount

	d := postJSON(t, ts, "runLexer", argsJSON(t, src))
	got := toInt(d["lexResult"])
	if got != want {
		t.Fatalf("lexerShape(src) = %d (totalLen=%d indentCount=%d encoded), want %d (totalLen=%d indentCount=%d) — split/charAt/len did not compose correctly",
			got, got/1000, got%1000, want, wantTotalLen, wantIndentCount)
	}
	// Sanity: this source has 5 lines ("app A:", the state line, an empty
	// line, the text line, and the trailing empty line strings.Split
	// produces after the final "\n"), two of which are indented (start with
	// a space).
	if len(lines) != 5 {
		t.Fatalf("test source has %d lines after split, want 5 — fix the fixture", len(lines))
	}
	if wantIndentCount != 2 {
		t.Fatalf("test source has %d indented lines, want 2 — fix the fixture", wantIndentCount)
	}
}
