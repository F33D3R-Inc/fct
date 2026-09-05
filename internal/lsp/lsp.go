// Package lsp is a Language Server for Facet, spoken over stdio (the `facet lsp`
// subcommand). It gives any LSP-capable editor live diagnostics (the compiler's
// own errors, on every keystroke), completion (keywords, types, builtins, and the
// app's declared names), go-to-definition and hover for top-level declarations,
// and a document symbol outline. It depends only on the standard library and the
// Facet compiler front-end, so it ships inside the one `facet` binary.
package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"facet/internal/ast"
	"facet/internal/ir"
	"facet/internal/parser"
)

// Serve runs the language server, reading LSP messages from in and writing
// responses to out until the stream closes (or an `exit` notification arrives).
func Serve(in io.Reader, out io.Writer) error {
	s := &server{
		out:  out,
		docs: map[string]string{},
		w:    bufio.NewWriter(out),
	}
	r := bufio.NewReader(in)
	for {
		body, err := readMessage(r)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var msg rpcMessage
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		if s.handle(msg) {
			return nil // exit
		}
	}
}

type server struct {
	out  io.Writer
	w    *bufio.Writer
	docs map[string]string // uri -> text
}

// ── JSON-RPC framing ─────────────────────────────────────────────────────────

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if n, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			length, _ = strconv.Atoi(strings.TrimSpace(n))
		}
	}
	if length == 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *server) write(v any) {
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "Content-Length: %d\r\n\r\n", len(body))
	s.w.Write(body)
	s.w.Flush()
}

func (s *server) reply(id json.RawMessage, result any) {
	if id == nil {
		return
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// ── dispatch ─────────────────────────────────────────────────────────────────

// handle processes one message; it returns true when the server should exit.
func (s *server) handle(msg rpcMessage) bool {
	switch msg.Method {
	case "initialize":
		s.reply(msg.ID, map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":       1, // full document sync
				"completionProvider":     map[string]any{"triggerCharacters": []string{" ", "."}},
				"definitionProvider":     true,
				"hoverProvider":          true,
				"documentSymbolProvider": true,
			},
			"serverInfo": map[string]any{"name": "facet-lsp", "version": "1"},
		})
	case "initialized", "$/setTrace":
		// no-op
	case "shutdown":
		s.reply(msg.ID, nil)
	case "exit":
		return true
	case "textDocument/didOpen":
		var p didOpenParams
		json.Unmarshal(msg.Params, &p)
		s.docs[p.TextDocument.URI] = p.TextDocument.Text
		s.publishDiagnostics(p.TextDocument.URI)
	case "textDocument/didChange":
		var p didChangeParams
		json.Unmarshal(msg.Params, &p)
		if len(p.ContentChanges) > 0 {
			s.docs[p.TextDocument.URI] = p.ContentChanges[len(p.ContentChanges)-1].Text
			s.publishDiagnostics(p.TextDocument.URI)
		}
	case "textDocument/didClose":
		var p didCloseParams
		json.Unmarshal(msg.Params, &p)
		delete(s.docs, p.TextDocument.URI)
	case "textDocument/completion":
		var p positionParams
		json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.completion(p.TextDocument.URI))
	case "textDocument/definition":
		var p positionParams
		json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.definition(p))
	case "textDocument/hover":
		var p positionParams
		json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.hover(p))
	case "textDocument/documentSymbol":
		var p positionParams
		json.Unmarshal(msg.Params, &p)
		s.reply(msg.ID, s.documentSymbols(p.TextDocument.URI))
	default:
		// Unknown request: reply with null so the editor is not left waiting.
		if msg.ID != nil {
			s.reply(msg.ID, nil)
		}
	}
	return false
}

// ── diagnostics ───────────────────────────────────────────────────────────────

func (s *server) publishDiagnostics(uri string) {
	text := s.docs[uri]
	diags := diagnose(text)
	if diags == nil {
		diags = []diagnostic{}
	}
	s.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": diags})
}

// diagnose compiles the document and returns the compiler's error (if any) as a
// single diagnostic on its reported line — the same error `facet build` prints —
// plus the advice the file earns whether or not it builds.
//
// THE ADVICE COMES FIRST, AND FROM THE PARSE ALONE. It is the only warning
// channel this toolchain has: ir.Build returns one error or none, so there is
// nowhere else for a rule that should not stop a build to be heard, and a
// missing `alt` is exactly such a rule — making it fatal would stop four
// repositories compiling on the day the attribute was introduced (see
// ast.Advise). Deriving it from the parse rather than from a successful build
// matters as much: most of a component library is fragments that do not compile
// on their own, and fragments are where the undescribed pictures live.
func diagnose(text string) []diagnostic {
	app, err := parser.Parse(text)
	if err != nil {
		return []diagnostic{errToDiagnostic(text, err)}
	}
	diags := adviceDiagnostics(text, ast.Advise(app))
	if _, err := ir.Build(app); err != nil {
		diags = append(diags, errToDiagnostic(text, err))
	}
	return diags
}

// adviceDiagnostics renders advice as Warning-severity diagnostics on their own
// lines, underlined the way an error is.
func adviceDiagnostics(text string, advice []ast.Advice) []diagnostic {
	if len(advice) == 0 {
		return nil
	}
	lines := strings.Split(text, "\n")
	out := make([]diagnostic, 0, len(advice))
	for _, a := range advice {
		idx := a.Line - 1
		if idx < 0 {
			idx = 0
		}
		lineText := ""
		if idx < len(lines) {
			lineText = lines[idx]
		}
		out = append(out, diagnostic{
			Range: lspRange{
				Start: position{Line: idx, Character: 0},
				End:   position{Line: idx, Character: len(lineText)},
			},
			Severity: 2, // Warning
			Source:   "facet",
			Message:  a.Msg,
		})
	}
	return out
}

func errToDiagnostic(text string, err error) diagnostic {
	line, msg := errLine(err)
	if line < 1 {
		line = 1
	}
	idx := line - 1
	// underline the whole source line.
	lineText := ""
	if lines := strings.Split(text, "\n"); idx < len(lines) {
		lineText = lines[idx]
	}
	return diagnostic{
		Range: lspRange{
			Start: position{Line: idx, Character: 0},
			End:   position{Line: idx, Character: len(lineText)},
		},
		Severity: 1, // Error
		Source:   "facet",
		Message:  msg,
	}
}

// errLine extracts the line number and bare message from a compiler error.
func errLine(err error) (int, string) {
	switch e := err.(type) {
	case *parser.Error:
		return e.Line, e.Msg
	case *ir.BuildError:
		return e.Line, e.Msg
	}
	// fall back: strip a leading "line N: " prefix the error string may carry.
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if colon := strings.IndexByte(rest, ':'); colon > 0 {
			if n, e := strconv.Atoi(strings.TrimSpace(rest[:colon])); e == nil {
				return n, strings.TrimSpace(rest[colon+1:])
			}
		}
	}
	return 1, msg
}

// ── language features ─────────────────────────────────────────────────────────

func (s *server) completion(uri string) any {
	var items []completionItem
	add := func(label string, kind int, detail string) {
		items = append(items, completionItem{Label: label, Kind: kind, Detail: detail})
	}
	for _, k := range keywords {
		add(k, 14, "keyword") // 14 = Keyword
	}
	for _, t := range typeNames {
		add(t, 7, "type") // 7 = Class
	}
	for _, b := range builtins {
		add(b, 3, "builtin") // 3 = Function
	}
	if app, err := parser.Parse(s.docs[uri]); err == nil {
		for name, sym := range symbols(app) {
			add(name, sym.completionKind, sym.detail)
		}
	}
	return map[string]any{"isIncomplete": false, "items": items}
}

func (s *server) definition(p positionParams) any {
	text := s.docs[p.TextDocument.URI]
	word := wordAt(text, p.Position.Line, p.Position.Character)
	if word == "" {
		return nil
	}
	app, err := parser.Parse(text)
	if err != nil {
		return nil
	}
	sym, ok := symbols(app)[word]
	if !ok {
		return nil
	}
	idx := sym.line - 1
	return location{
		URI: p.TextDocument.URI,
		Range: lspRange{
			Start: position{Line: idx, Character: 0},
			End:   position{Line: idx, Character: len(lineAt(text, idx))},
		},
	}
}

func (s *server) hover(p positionParams) any {
	text := s.docs[p.TextDocument.URI]
	word := wordAt(text, p.Position.Line, p.Position.Character)
	if word == "" {
		return nil
	}
	if app, err := parser.Parse(text); err == nil {
		if sym, ok := symbols(app)[word]; ok {
			return map[string]any{
				"contents": map[string]any{
					"kind":  "markdown",
					"value": "```facet\n" + sym.detail + "\n```",
				},
			}
		}
	}
	if d := builtinDoc(word); d != "" {
		return map[string]any{"contents": map[string]any{"kind": "markdown", "value": d}}
	}
	return nil
}

func (s *server) documentSymbols(uri string) any {
	app, err := parser.Parse(s.docs[uri])
	if err != nil {
		return []any{}
	}
	var out []documentSymbol
	for name, sym := range symbols(app) {
		idx := sym.line - 1
		out = append(out, documentSymbol{
			Name:           name,
			Kind:           sym.symbolKind,
			Detail:         sym.detail,
			Range:          lspRange{Start: position{Line: idx, Character: 0}, End: position{Line: idx, Character: 0}},
			SelectionRange: lspRange{Start: position{Line: idx, Character: 0}, End: position{Line: idx, Character: 0}},
		})
	}
	return out
}

// ── symbol table ─────────────────────────────────────────────────────────────

type symbol struct {
	line           int
	detail         string
	completionKind int // LSP CompletionItemKind
	symbolKind     int // LSP SymbolKind
}

// symbols collects every top-level declaration's name, declaration line, and a
// one-line signature, for go-to-definition, hover, completion and the outline.
func symbols(app *ast.App) map[string]symbol {
	out := map[string]symbol{}
	for _, e := range app.Entities {
		out[e.Name] = symbol{e.Line, "entity " + e.Name, 7, 23} // Class / Struct
	}
	for _, e := range app.Enums {
		out[e.Name] = symbol{e.Line, "enum " + e.Name + ": " + strings.Join(e.Values, ", "), 13, 10} // Enum
	}
	for _, st := range app.States {
		out[st.Name] = symbol{st.Line, "state " + st.Name + ": " + st.Type, 6, 13} // Variable
	}
	for _, d := range app.Derives {
		out[d.Name] = symbol{d.Line, "derive " + d.Name + ": " + d.Type, 6, 13}
	}
	for _, p := range app.Policies {
		out[p.Name] = symbol{p.Line, "policy " + p.Name + paramList(p.Params), 3, 12} // Function
	}
	for _, a := range app.Actions {
		out[a.Name] = symbol{a.Line, "action " + a.Name + paramList(a.Params), 3, 12}
	}
	for _, c := range app.Components {
		out[c.Name] = symbol{c.Line, "component " + c.Name + paramList(c.Params), 3, 12}
	}
	for _, l := range app.Layouts {
		out[l.Name] = symbol{l.Line, "layout " + l.Name, 7, 23}
	}
	for _, v := range app.Views {
		out[v.Name] = symbol{v.Line, "view " + v.Name, 7, 23}
	}
	return out
}

func paramList(ps []ast.Param) string {
	if len(ps) == 0 {
		return ""
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.Name + ": " + p.Type
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ── text helpers ──────────────────────────────────────────────────────────────

func lineAt(text string, idx int) string {
	lines := strings.Split(text, "\n")
	if idx < 0 || idx >= len(lines) {
		return ""
	}
	return lines[idx]
}

func wordAt(text string, line, char int) string {
	s := lineAt(text, line)
	if char > len(s) {
		char = len(s)
	}
	start := char
	for start > 0 && isIdentByte(s[start-1]) {
		start--
	}
	end := char
	for end < len(s) && isIdentByte(s[end]) {
		end++
	}
	if start >= end {
		return ""
	}
	return s[start:end]
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// ── static vocabulary ─────────────────────────────────────────────────────────

var keywords = []string{
	"app", "auth", "entity", "enum", "state", "derive", "policy", "action", "job",
	"component", "layout", "theme", "view", "for", "in", "where", "by", "limit",
	"if", "box", "text", "button", "input", "textarea", "checkbox", "toggle", "radio",
	"select", "option", "form", "upload",
	"use", "slot", "link", "add", "set", "remove", "clear", "requires", "check",
	"at", "desc", "asc", "on", "start", "every",
}

var typeNames = []string{"int", "text", "bool", "money", "date"}

var builtins = []string{
	"now", "rand", "count", "sum", "abs", "min", "max", "floor", "round", "money",
	"len", "upper", "lower", "trim", "year", "month", "day", "actor", "role", "verified",
	"route",
}

func builtinDoc(name string) string {
	docs := map[string]string{
		"now":   "`now()` → int — the server clock (unix seconds). Effectful: pins its action to the authority.",
		"rand":  "`rand(n)` → int — a server random in [0, n). Effectful.",
		"count": "`count(Entity)` → int — number of rows.",
		"sum":   "`sum(Entity.field)` → int — total of a numeric field.",
		"abs":   "`abs(n)` → int — absolute value.",
		"min":   "`min(a, b)` → int — the smaller of two values.",
		"max":   "`max(a, b)` → int — the larger of two values.",
		"len":   "`len(x)` → int — length of a string or list.",
		"upper": "`upper(s)` → text — uppercased.",
		"lower": "`lower(s)` → text — lowercased.",
		"trim":  "`trim(s)` → text — whitespace-trimmed.",
		"money": "`money(cents)` → text — format integer minor units as a 2-decimal string.",
		"year":  "`year(t)` → int — UTC year of a date (unix seconds).",
		"month": "`month(t)` → int — UTC month (1–12).",
		"day":   "`day(t)` → int — UTC day of month.",
		"actor": "`actor` — the signed-in username (or `\"guest\"`).",
		"role":  "`role` — the actor's role (admin | member | guest).",
		"route": "`route` — the path being rendered (e.g. `/post/7`). Readable in a view only; a nav compares it to a destination to mark itself active.",
	}
	return docs[name]
}

// ── LSP wire types ────────────────────────────────────────────────────────────

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}
type location struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}
type diagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity"`
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}
type textDocumentItem struct {
	URI  string `json:"uri"`
	Text string `json:"text"`
}
type textDocumentID struct {
	URI string `json:"uri"`
}
type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}
type contentChange struct {
	Text string `json:"text"`
}
type didChangeParams struct {
	TextDocument   textDocumentID  `json:"textDocument"`
	ContentChanges []contentChange `json:"contentChanges"`
}
type didCloseParams struct {
	TextDocument textDocumentID `json:"textDocument"`
}
type positionParams struct {
	TextDocument textDocumentID `json:"textDocument"`
	Position     position       `json:"position"`
}
type completionItem struct {
	Label  string `json:"label"`
	Kind   int    `json:"kind"`
	Detail string `json:"detail,omitempty"`
}
type documentSymbol struct {
	Name           string   `json:"name"`
	Detail         string   `json:"detail,omitempty"`
	Kind           int      `json:"kind"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}
