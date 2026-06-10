// Package lexer turns FDL source into a flat token stream that encodes block
// structure via INDENT / DEDENT tokens (the "offside rule", as in Python).
//
// It does NOT tokenize line *content* into identifiers, strings, etc. — that is
// the parser's job, and it differs per block (a `render:` body is HTML, a `data:`
// body is field declarations). The lexer's single responsibility is structure:
// strip comments and blank lines, validate indentation, and emit one LINE token
// per significant source line bracketed by INDENT/DEDENT.
//
// See README.md (the language reference) and DECISIONS.md (ADR-0002, ADR-0004).
package lexer

import (
	"fmt"
	"strings"
)

// Kind enumerates the structural token kinds.
type Kind int

const (
	// LINE is one significant source line (comments/blanks already removed).
	// Text holds the line with leading indentation stripped and trailing
	// whitespace trimmed.
	LINE Kind = iota
	// INDENT marks entry into a deeper block (one level = 4 spaces).
	INDENT
	// DEDENT marks exit from a block. A run of DEDENTs may close several levels.
	DEDENT
)

func (k Kind) String() string {
	switch k {
	case LINE:
		return "LINE"
	case INDENT:
		return "INDENT"
	case DEDENT:
		return "DEDENT"
	default:
		return "?"
	}
}

// Token is one structural token.
type Token struct {
	Kind Kind
	Text string // LINE only: the trimmed content
	Line int    // 1-based source line number
	Col  int    // 1-based column where content begins (indent + 1)
}

func (t Token) String() string {
	if t.Kind == LINE {
		return fmt.Sprintf("%d:%d LINE %q", t.Line, t.Col, t.Text)
	}
	return fmt.Sprintf("%d:%d %s", t.Line, t.Col, t.Kind)
}

// Error is a lexing error carrying a source position.
type Error struct {
	Line int
	Col  int
	Msg  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg)
}

// Lex tokenizes src. On the first structural problem it returns a *Error with a
// source position; otherwise it returns the full token stream (INDENT/DEDENT
// balanced, every block closed at EOF).
func Lex(src string) ([]Token, error) {
	var toks []Token
	indents := []int{0} // stack of open indentation columns; always starts with 0

	lines := splitLines(src)
	inBlockComment := false

	for i, raw := range lines {
		lineNo := i + 1

		// Strip a trailing \r so CRLF files behave like LF files.
		line := strings.TrimRight(raw, "\r")

		// --- block comments: #| ... |# on their own lines ---
		trimmed := strings.TrimSpace(line)
		if inBlockComment {
			if strings.HasSuffix(trimmed, "|#") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "#|") {
			// Single-line #| ... |# also allowed.
			if !strings.HasSuffix(trimmed, "|#") || trimmed == "#|" {
				inBlockComment = true
			}
			continue
		}

		// --- blank lines and full-line comments are structurally invisible ---
		// (ADR-0004: `#` only starts a comment as the first non-ws char.)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// --- measure indentation ---
		col, err := measureIndent(line, lineNo)
		if err != nil {
			return nil, err
		}

		content := line[col:]
		content = strings.TrimRight(content, " \t")

		top := indents[len(indents)-1]
		switch {
		case col > top:
			indents = append(indents, col)
			toks = append(toks, Token{Kind: INDENT, Line: lineNo, Col: col + 1})
		case col < top:
			// Pop until we match an open level exactly.
			for len(indents) > 0 && col < indents[len(indents)-1] {
				indents = indents[:len(indents)-1]
				toks = append(toks, Token{Kind: DEDENT, Line: lineNo, Col: col + 1})
			}
			if indents[len(indents)-1] != col {
				return nil, &Error{Line: lineNo, Col: col + 1,
					Msg: "dedent does not match any open indentation level"}
			}
		}

		toks = append(toks, Token{Kind: LINE, Text: content, Line: lineNo, Col: col + 1})
	}

	if inBlockComment {
		return nil, &Error{Line: len(lines), Col: 1, Msg: "unterminated #| block comment"}
	}

	// Close every still-open block at EOF.
	eof := len(lines) + 1
	for len(indents) > 1 {
		indents = indents[:len(indents)-1]
		toks = append(toks, Token{Kind: DEDENT, Line: eof, Col: 1})
	}

	return toks, nil
}

// measureIndent returns the number of leading-space columns. Tabs in
// indentation are an error. Nesting is determined by RELATIVE indentation
// (see Lex), so the width is not required to be a multiple of any fixed size —
// 4 spaces per level is convention, but wrapped HTML continuation lines inside
// a `looks:` body may align to any column. A genuinely broken dedent (one that
// lands between two open levels) is still caught by the DEDENT-match check.
func measureIndent(line string, lineNo int) (int, error) {
	col := 0
	for col < len(line) {
		switch line[col] {
		case ' ':
			col++
		case '\t':
			return 0, &Error{Line: lineNo, Col: col + 1,
				Msg: "tab in indentation; use spaces"}
		default:
			return col, nil
		}
	}
	return col, nil
}

// splitLines splits on \n, preserving content; a trailing newline does not
// produce a spurious empty final line.
func splitLines(src string) []string {
	if src == "" {
		return nil
	}
	lines := strings.Split(src, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
