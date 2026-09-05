// Package source turns raw .fct text into an indentation tree. FDL is an
// offside-rule language: a header line owns the consecutive lines indented
// deeper than it. We resolve that nesting once, here, so the parser works on a
// tree of nodes and never has to think about whitespace again.
package source

import (
	"fmt"
	"strings"
)

// Line is one significant (non-blank, non-comment) source line.
type Line struct {
	Indent int    // leading-space count
	Text   string // trimmed content
	No     int    // 1-based line number
}

// Node is a Line plus the lines nested under it (strictly greater indent).
type Node struct {
	Line     Line
	Children []*Node
}

// Comments returns the full-line comments Parse discards, with their line
// numbers.
//
// Dropping comments is right for every FDL construct and wrong for exactly one:
// a `css:` block's body is a stylesheet, not FDL, and `#` opens a comment here
// while it names an id there. So `#fa-root .fa-box { border: none }` is not a
// rule the compiler rejects, it is a rule that never existed — the stylesheet
// compiles, the page renders, and four of its declarations are simply absent.
//
// The scanner cannot tell the two apart, because it has no idea which block it
// is inside. The parser does, so it asks — see `checkCSSComments`. Re-scanning
// the text is cheap next to compiling it, and keeps `Parse` returning the two
// values every caller wants.
func Comments(src string) []Line {
	var out []Line

	for i, raw := range strings.Split(src, "\n") {
		text := strings.TrimSpace(raw)
		if strings.HasPrefix(text, "#") {
			out = append(out, Line{
				Indent: len(raw) - len(strings.TrimLeft(raw, " ")),
				Text:   text,
				No:     i + 1,
			})
		}
	}

	return out
}

// Parse reads source text into a forest of indentation nodes (the indent-0
// lines, each carrying its nested children). Tabs are rejected: indentation
// must be spaces so columns are unambiguous.
func Parse(src string) ([]*Node, error) {
	var lines []Line
	for i, raw := range strings.Split(src, "\n") {
		if strings.ContainsRune(raw, '\t') && strings.TrimSpace(raw) != "" {
			lead := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]
			if strings.ContainsRune(lead, '\t') {
				return nil, fmt.Errorf("line %d: tabs are not allowed for indentation; use spaces", i+1)
			}
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue // blank or full-line comment
		}
		lines = append(lines, Line{Indent: indent, Text: text, No: i + 1})
	}
	roots, _ := build(lines, 0, -1)
	return roots, nil
}

// build consumes lines[pos:] whose indent is greater than parentIndent,
// returning the sibling nodes at the first such indent level and the next
// unconsumed position.
func build(lines []Line, pos, parentIndent int) ([]*Node, int) {
	var nodes []*Node
	for pos < len(lines) {
		ln := lines[pos]
		if ln.Indent <= parentIndent {
			break
		}
		// First child sets the level for this sibling group.
		level := ln.Indent
		if len(nodes) > 0 && ln.Indent != level {
			// shallower-than-first-sibling but still > parent: treat as end of group
		}
		n := &Node{Line: ln}
		pos++
		n.Children, pos = build(lines, pos, ln.Indent)
		nodes = append(nodes, n)
	}
	return nodes, pos
}
