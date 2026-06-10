package lexer

import (
	"os"
	"strings"
	"testing"
)

// compact renders a token stream as a terse string for assertions:
// LINE → "L:<text>", INDENT → ">", DEDENT → "<".
func compact(toks []Token) string {
	var b strings.Builder
	for i, t := range toks {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch t.Kind {
		case INDENT:
			b.WriteByte('>')
		case DEDENT:
			b.WriteByte('<')
		case LINE:
			b.WriteString("L:" + t.Text)
		}
	}
	return b.String()
}

func TestNesting(t *testing.T) {
	src := "" +
		"facet A:\n" +
		"    data:\n" +
		"        x: int\n" +
		"    render:\n" +
		"        <p>hi</p>\n"

	toks, err := Lex(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := compact(toks)
	want := "L:facet A: > L:data: > L:x: int < L:render: > L:<p>hi</p> < <"
	if got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

func TestCommentsAndBlanksIgnored(t *testing.T) {
	src := "" +
		"# top comment\n" +
		"\n" +
		"facet A:\n" +
		"    # inner full-line comment\n" +
		"    render:\n" +
		"\n" +
		"        <p>hi</p>\n"

	toks, err := Lex(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := compact(toks)
	want := "L:facet A: > L:render: > L:<p>hi</p> < <"
	if got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
}

// The crucial ADR-0004 case: a `#` that is not the first non-ws char is literal,
// so HTML/CSS colours and anchors survive intact.
func TestHashInContentIsLiteral(t *testing.T) {
	src := "" +
		"facet A:\n" +
		"    render:\n" +
		`        <a href="#" style="color:#1d9bf0">x</a>` + "\n"

	toks, err := Lex(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	last := toks[len(toks)-3] // LINE, then two DEDENTs
	if last.Kind != LINE || !strings.Contains(last.Text, `href="#"`) || !strings.Contains(last.Text, "#1d9bf0") {
		t.Fatalf("hash in content was mangled: %q", last.Text)
	}
}

func TestTabIndentRejected(t *testing.T) {
	src := "facet A:\n\tdata:\n"
	_, err := Lex(src)
	if err == nil {
		t.Fatal("expected error for tab indentation")
	}
	if !strings.Contains(err.Error(), "tab") {
		t.Fatalf("expected tab error, got: %v", err)
	}
}

func TestArbitraryIndentWidthAccepted(t *testing.T) {
	// Nesting is by relative indentation; a consistent non-4 width is fine.
	src := "facet A:\n  what:\n    x: int\n"
	if _, err := Lex(src); err != nil {
		t.Fatalf("consistent 2-space indent should lex: %v", err)
	}
}

// The case that broke the scaffold: an HTML line wrapped onto a more-indented
// continuation (not a multiple of 4) must lex without error.
func TestWrappedContinuationLineAccepted(t *testing.T) {
	src := "" +
		"facet A:\n" +
		"    looks:\n" +
		"        <p>line one\n" +
		"             continues here</p>\n" + // 13 spaces
		"        <span>after</span>\n"
	toks, err := Lex(src)
	if err != nil {
		t.Fatalf("wrapped HTML continuation should lex: %v", err)
	}
	// the continuation must survive as a LINE
	var found bool
	for _, tk := range toks {
		if tk.Kind == LINE && strings.Contains(tk.Text, "continues here") {
			found = true
		}
	}
	if !found {
		t.Fatal("continuation line text lost")
	}
}

func TestMisalignedDedentRejected(t *testing.T) {
	// Closes from 8 spaces back to 4? No — dedents to a level (2 invalid anyway).
	// Use 8 -> 4 valid, but 12 -> 8 -> back to a non-open 4? Build an explicit case:
	src := "" +
		"facet A:\n" +
		"        x: int\n" + // 8 spaces (INDENT to 8)
		"    y: int\n" // 4 spaces: never an open level (only 0 and 8 are)
	_, err := Lex(src)
	if err == nil {
		t.Fatal("expected misaligned-dedent error")
	}
	if !strings.Contains(err.Error(), "dedent") {
		t.Fatalf("expected dedent error, got: %v", err)
	}
}

// The real example file must lex without error and be balanced.
func TestExampleFileLexes(t *testing.T) {
	src, err := os.ReadFile("../../examples/like_button.fct")
	if err != nil {
		t.Skipf("example not found: %v", err)
	}
	toks, err := Lex(string(src))
	if err != nil {
		t.Fatalf("example failed to lex: %v", err)
	}
	depth := 0
	for _, tk := range toks {
		switch tk.Kind {
		case INDENT:
			depth++
		case DEDENT:
			depth--
		}
		if depth < 0 {
			t.Fatal("unbalanced DEDENT")
		}
	}
	if depth != 0 {
		t.Fatalf("blocks not balanced at EOF: depth=%d", depth)
	}
}
