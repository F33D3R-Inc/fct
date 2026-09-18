// This file verifies expr.fct's tokenizer + parser procs against the real,
// current Go implementation they are a subset-port of
// (internal/parser.ParseExpr / internal/parser's unexported tokenize +
// exprParser), by compiling and running expr.fct through the same
// in-memory server source_test.go already uses, and comparing the parsed
// tree SHAPE, not just a flat value, against the real Go parser's own
// ast.Expr for the same input. It imports internal/parser and internal/ast
// read-only — no .go file anywhere in the module is modified by this
// package.
package selfhost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"facet/internal/ast"
	"facet/internal/compile"
	"facet/internal/parser"
	"facet/runtime"
)

// loadExprApp compiles and boots selfhost/expr.fct through the same
// in-memory server source_test.go's loadApp already uses for source.fct —
// a separate helper (not a shared one) only because loadApp there is
// hardcoded to "source.fct"; the pattern is otherwise identical. expr.fct is
// now a driver that imports expr_tokens.fct/expr_tree.fct (the STAGE 0-2f
// split), so it must go through compile.File — the import-aware entry
// point — rather than compile.String, which rejects any source declaring
// imports.
func loadExprApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("expr.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/expr.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// postExprJSON mirrors source_test.go's postJSON exactly (same request/
// response shape), named separately only to avoid colliding with that
// file's own postJSON in this shared package.
func postExprJSON(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/"+action, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s returned %d: %s", action, resp.StatusCode, b)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("%s did not report ok: %+v", action, out)
	}
	return out.Deltas
}

// treeShape is a Go-side, pointer-tree restatement of ast.Expr, used only
// as a common comparison target: one built by walking the REAL Go parser's
// ast.Expr (viaShapeFromGoExpr), the other by walking expr.fct's own flat
// arena result back out (shapeFromArena) — structurally compared field by
// field, not by any encoding detail either side happens to use internally.
//
// "un" (round 2 addition, alongside comparisons/&&/||/bitwise as new "bin"
// operators, which need no new treeShape kind since ast.Bin/arena kind 2
// already generalize over any operator STRING) is the one shape that does
// need a new kind: ast.Un{Op, X} is a genuinely different node shape (one
// child, not two) from ast.Bin, and arena kind 3 (see expr.fct's STAGE 2b
// doc) mirrors that — `right` stays nil/unused for "un", exactly as the
// arena's own right slot stays -1 for a Un node.
//
// Round 3 adds "get" (ast.Get{Obj, Field} / arena kind 6, `.field`) and
// "index" (ast.Index{Obj, Idx} / arena kind 7, `[i]`), plus two more `litKind`
// values ("bool"/"text") alongside "lit"'s existing (now explicit) "int" —
// ast.Lit already generalizes over any Kind string, exactly like ast.Bin
// already generalized over any Op string, so no new top-level `kind` was
// needed for the new literal flavors, only a sub-discriminator.
//
// Round 4 adds "call" (ast.Call{Name, Args} / arena kind 9, `name(args...)`,
// builtin calls only — see expr.fct's ROUND 4 SCOPE NOTE) and "list"
// (ast.ListLit{Elems} / arena kind 10, `[elems...]`) — the first two shapes
// in this file with a variable-length child list rather than a fixed
// left/right pair, so `args`/`elems` are `[]*treeShape` rather than reusing
// left/right.
//
// Round 5 adds a third `litKind` ("float", alongside "int"/"bool"/"text" —
// ast.Lit already generalizes over any Kind string, so again no new
// top-level `kind` was needed), plus two more top-level kinds: "entity"
// (ast.EntityGet{Entity, Key, Field} / arena kind 12, `Entity(key).field` or
// bare `Entity(key)` — `field` is "" when the real Field is absent, since a
// real field name is never empty) and "map" (ast.MapLit{Keys, Vals} / arena
// kind 14, `{k: v, ...}`) — the third variable-length-child shape, this one
// with TWO parallel children lists (`mapKeys`/`mapVals`) rather than one,
// since each entry is a key/value PAIR.
type treeShape struct {
	kind        string  // "lit" | "ref" | "bin" | "un" | "get" | "index" | "call" | "list" | "entity" | "map"
	litKind     string  // kind == "lit": "int" | "bool" | "text" | "float"
	intVal      int     // kind == "lit" && litKind == "int"
	boolVal     bool    // kind == "lit" && litKind == "bool"
	textVal     string  // kind == "lit" && litKind == "text"
	floatVal    float64 // kind == "lit" && litKind == "float"
	name        string  // kind == "ref" | "call" | "entity" (entity's builtin/entity name)
	op          string  // kind == "bin" | "un"
	field       string  // kind == "get" | "entity" (entity: "" means no field)
	left, right *treeShape
	args        []*treeShape // kind == "call"
	elems       []*treeShape // kind == "list"
	mapKeys     []*treeShape // kind == "map"
	mapVals     []*treeShape // kind == "map" (parallel to mapKeys)
}

func shapeFromGoExpr(t *testing.T, e ast.Expr) *treeShape {
	t.Helper()
	switch x := e.(type) {
	case ast.Lit:
		switch x.Kind {
		case "int":
			return &treeShape{kind: "lit", litKind: "int", intVal: x.Val.(int)}
		case "bool":
			return &treeShape{kind: "lit", litKind: "bool", boolVal: x.Val.(bool)}
		case "text":
			return &treeShape{kind: "lit", litKind: "text", textVal: x.Val.(string)}
		case "float":
			return &treeShape{kind: "lit", litKind: "float", floatVal: x.Val.(float64)}
		default:
			t.Fatalf("shapeFromGoExpr: unexpected literal kind %q (this subset covers int/bool/text literals)", x.Kind)
			return nil
		}
	case ast.Ref:
		return &treeShape{kind: "ref", name: x.Name}
	case ast.Bin:
		return &treeShape{
			kind:  "bin",
			op:    x.Op,
			left:  shapeFromGoExpr(t, x.L),
			right: shapeFromGoExpr(t, x.R),
		}
	case ast.Un:
		return &treeShape{
			kind: "un",
			op:   x.Op,
			left: shapeFromGoExpr(t, x.X),
		}
	case ast.Get:
		return &treeShape{
			kind:  "get",
			field: x.Field,
			left:  shapeFromGoExpr(t, x.Obj),
		}
	case ast.Index:
		return &treeShape{
			kind:  "index",
			left:  shapeFromGoExpr(t, x.Obj),
			right: shapeFromGoExpr(t, x.Idx),
		}
	case ast.Call:
		var args []*treeShape
		for _, a := range x.Args {
			args = append(args, shapeFromGoExpr(t, a))
		}
		return &treeShape{kind: "call", name: x.Name, args: args}
	case ast.ListLit:
		var elems []*treeShape
		for _, el := range x.Elems {
			elems = append(elems, shapeFromGoExpr(t, el))
		}
		return &treeShape{kind: "list", elems: elems}
	case ast.EntityGet:
		return &treeShape{
			kind:  "entity",
			name:  x.Entity,
			field: x.Field,
			left:  shapeFromGoExpr(t, x.Key),
		}
	case ast.MapLit:
		var keys, vals []*treeShape
		for _, k := range x.Keys {
			keys = append(keys, shapeFromGoExpr(t, k))
		}
		for _, v := range x.Vals {
			vals = append(vals, shapeFromGoExpr(t, v))
		}
		return &treeShape{kind: "map", mapKeys: keys, mapVals: vals}
	default:
		t.Fatalf("shapeFromGoExpr: unexpected ast.Expr node %T (this subset only covers Lit(int/bool/text/float)/Ref/Bin/Un/Get/Index/Call/ListLit/EntityGet/MapLit)", e)
		return nil
	}
}

// shapeFromArena decodes expr.fct's own flat [kind,valTok,opTok,left,right]
// arena (5 ints per node, see expr.fct's STAGE 2b doc for the exact layout)
// back into the same treeShape, given the tokenTexts array the arena's
// valTok/opTok fields index into. nodeIdx is a NODE index (row number), not
// a raw array offset.
//
// Round 3 kinds: 4 (Lit(bool)) decodes valTok's token text directly
// ("true"/"false", the only two IDENT spellings factorNodes ever tags this
// way). 5 (Lit(text)) decodes valTok's token text with strconv.Unquote — the
// EXACT function internal/parser/expr.go's own parseAtom tStr case calls
// (see expr.go line ~657) — since expr.fct's tokenizer deliberately keeps a
// string token's text RAW (quotes and backslash escapes intact; see this
// file's own header FINDING 7), leaving the actual escape decoding to
// whoever consumes the arena, exactly as expr.go's own tokenize/parseAtom
// split (tokenize captures the raw span, parseAtom decodes it) already does.
// 6 (Get) and 7 (Index) recurse into their object/index children the same
// way Bin/Un already do.
//
// 9 (Call) and 10 (ListLit) are the first variable-arity kinds: `left` is
// the NODE index of the first of N contiguous ArgSlot rows (arena kind 8,
// or -1 when `right` (the count N) is 0), never a second child directly —
// see expr.fct's header FINDING 8 for why. Each argument/element is
// recovered by reading ArgSlot row `left+i`'s OWN valTok (its only real
// field), which names that argument's true root node index, then
// recursing into shapeFromArena on THAT index — one extra indirection hop
// this decoder mirrors exactly, argument by argument, in order.
func shapeFromArena(t *testing.T, arena []int, tokTexts []string, nodeIdx int) *treeShape {
	t.Helper()
	if nodeIdx < 0 || nodeIdx*5+4 >= len(arena) {
		t.Fatalf("shapeFromArena: node index %d out of range (arena has %d nodes)", nodeIdx, len(arena)/5)
	}
	kind := arena[nodeIdx*5+0]
	valTok := arena[nodeIdx*5+1]
	opTok := arena[nodeIdx*5+2]
	left := arena[nodeIdx*5+3]
	right := arena[nodeIdx*5+4]
	switch kind {
	case 0: // Lit(int)
		n := 0
		for i, c := range tokTexts[valTok] {
			_ = i
			if c < '0' || c > '9' {
				t.Fatalf("shapeFromArena: leaf token %q at tok %d is not all digits", tokTexts[valTok], valTok)
			}
		}
		for _, c := range tokTexts[valTok] {
			n = n*10 + int(c-'0')
		}
		return &treeShape{kind: "lit", litKind: "int", intVal: n}
	case 1: // Ref
		return &treeShape{kind: "ref", name: tokTexts[valTok]}
	case 2: // Bin
		return &treeShape{
			kind:  "bin",
			op:    tokTexts[opTok],
			left:  shapeFromArena(t, arena, tokTexts, left),
			right: shapeFromArena(t, arena, tokTexts, right),
		}
	case 3: // Un
		return &treeShape{
			kind: "un",
			op:   tokTexts[opTok],
			left: shapeFromArena(t, arena, tokTexts, left),
		}
	case 4: // Lit(bool)
		raw := tokTexts[valTok]
		if raw != "true" && raw != "false" {
			t.Fatalf("shapeFromArena: bool literal token %q at tok %d is neither \"true\" nor \"false\"", raw, valTok)
		}
		return &treeShape{kind: "lit", litKind: "bool", boolVal: raw == "true"}
	case 5: // Lit(text)
		raw := tokTexts[valTok]
		dec, err := strconv.Unquote(raw)
		if err != nil {
			t.Fatalf("shapeFromArena: bad string literal token %q at tok %d: %v", raw, valTok, err)
		}
		return &treeShape{kind: "lit", litKind: "text", textVal: dec}
	case 6: // Get
		return &treeShape{
			kind:  "get",
			field: tokTexts[opTok],
			left:  shapeFromArena(t, arena, tokTexts, left),
		}
	case 7: // Index
		return &treeShape{
			kind:  "index",
			left:  shapeFromArena(t, arena, tokTexts, left),
			right: shapeFromArena(t, arena, tokTexts, right),
		}
	case 9: // Call: opTok = name token, left = first ArgSlot node index, right = argCount
		var args []*treeShape
		for i := 0; i < right; i++ {
			slotIdx := left + i
			if slotIdx < 0 || slotIdx*5+4 >= len(arena) {
				t.Fatalf("shapeFromArena: Call arg slot index %d out of range (node %d)", slotIdx, nodeIdx)
			}
			argRoot := arena[slotIdx*5+1] // ArgSlot's valTok: the real argument's root node index
			args = append(args, shapeFromArena(t, arena, tokTexts, argRoot))
		}
		return &treeShape{kind: "call", name: tokTexts[opTok], args: args}
	case 10: // ListLit: left = first ArgSlot node index, right = element count
		var elems []*treeShape
		for i := 0; i < right; i++ {
			slotIdx := left + i
			if slotIdx < 0 || slotIdx*5+4 >= len(arena) {
				t.Fatalf("shapeFromArena: ListLit elem slot index %d out of range (node %d)", slotIdx, nodeIdx)
			}
			elemRoot := arena[slotIdx*5+1]
			elems = append(elems, shapeFromArena(t, arena, tokTexts, elemRoot))
		}
		return &treeShape{kind: "list", elems: elems}
	case 11: // Lit(float): valTok names the same NUM token an int would (int
		// and float share one token kind — see expr.fct's ROUND 5 SCOPE
		// NOTE); decoded with strconv.ParseFloat, the exact function
		// internal/parser/expr.go's own parseAtom tNum-with-a-dot case calls.
		raw := tokTexts[valTok]
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("shapeFromArena: bad float literal token %q at tok %d: %v", raw, valTok, err)
		}
		return &treeShape{kind: "lit", litKind: "float", floatVal: f}
	case 12: // EntityGet: valTok = field name token (-1 if absent), opTok =
		// entity name token, left = key expression's root node index.
		field := ""
		if valTok != -1 {
			field = tokTexts[valTok]
		}
		return &treeShape{
			kind:  "entity",
			name:  tokTexts[opTok],
			field: field,
			left:  shapeFromArena(t, arena, tokTexts, left),
		}
	case 14: // MapLit: left = first MapPairSlot node index, right = pair
		// count. Each MapPairSlot (kind 13) stores its OWN key/value root
		// node indices directly in ITS left/right (not through a valTok
		// indirection the way ArgSlot does for Call/ListLit) — see
		// expr.fct's FINDING 10.
		var keys, vals []*treeShape
		for i := 0; i < right; i++ {
			slotIdx := left + i
			if slotIdx < 0 || slotIdx*5+4 >= len(arena) {
				t.Fatalf("shapeFromArena: MapLit pair slot index %d out of range (node %d)", slotIdx, nodeIdx)
			}
			keyRoot := arena[slotIdx*5+3]
			valRoot := arena[slotIdx*5+4]
			keys = append(keys, shapeFromArena(t, arena, tokTexts, keyRoot))
			vals = append(vals, shapeFromArena(t, arena, tokTexts, valRoot))
		}
		return &treeShape{kind: "map", mapKeys: keys, mapVals: vals}
	default:
		t.Fatalf("shapeFromArena: unknown node kind %d at node %d", kind, nodeIdx)
		return nil
	}
}

func shapesEqual(a, b *treeShape) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case "lit":
		if a.litKind != b.litKind {
			return false
		}
		switch a.litKind {
		case "bool":
			return a.boolVal == b.boolVal
		case "text":
			return a.textVal == b.textVal
		case "float":
			return a.floatVal == b.floatVal
		default: // "int"
			return a.intVal == b.intVal
		}
	case "ref":
		return a.name == b.name
	case "bin":
		return a.op == b.op && shapesEqual(a.left, b.left) && shapesEqual(a.right, b.right)
	case "un":
		return a.op == b.op && shapesEqual(a.left, b.left)
	case "get":
		return a.field == b.field && shapesEqual(a.left, b.left)
	case "index":
		return shapesEqual(a.left, b.left) && shapesEqual(a.right, b.right)
	case "call":
		if a.name != b.name || len(a.args) != len(b.args) {
			return false
		}
		for i := range a.args {
			if !shapesEqual(a.args[i], b.args[i]) {
				return false
			}
		}
		return true
	case "list":
		if len(a.elems) != len(b.elems) {
			return false
		}
		for i := range a.elems {
			if !shapesEqual(a.elems[i], b.elems[i]) {
				return false
			}
		}
		return true
	case "entity":
		return a.name == b.name && a.field == b.field && shapesEqual(a.left, b.left)
	case "map":
		if len(a.mapKeys) != len(b.mapKeys) || len(a.mapVals) != len(b.mapVals) {
			return false
		}
		for i := range a.mapKeys {
			if !shapesEqual(a.mapKeys[i], b.mapKeys[i]) {
				return false
			}
		}
		for i := range a.mapVals {
			if !shapesEqual(a.mapVals[i], b.mapVals[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func shapeString(s *treeShape) string {
	if s == nil {
		return "<nil>"
	}
	switch s.kind {
	case "lit":
		switch s.litKind {
		case "bool":
			return "Lit(" + strconv.FormatBool(s.boolVal) + ")"
		case "text":
			return "Lit(" + strconv.Quote(s.textVal) + ")"
		case "float":
			return "Lit(" + strconv.FormatFloat(s.floatVal, 'g', -1, 64) + ")"
		default: // "int"
			return "Lit(" + itoa(s.intVal) + ")"
		}
	case "ref":
		return "Ref(" + s.name + ")"
	case "bin":
		return "(" + shapeString(s.left) + " " + s.op + " " + shapeString(s.right) + ")"
	case "un":
		return "(" + s.op + shapeString(s.left) + ")"
	case "get":
		return shapeString(s.left) + "." + s.field
	case "index":
		return shapeString(s.left) + "[" + shapeString(s.right) + "]"
	case "call":
		out := s.name + "("
		for i, a := range s.args {
			if i > 0 {
				out += ", "
			}
			out += shapeString(a)
		}
		return out + ")"
	case "list":
		out := "["
		for i, el := range s.elems {
			if i > 0 {
				out += ", "
			}
			out += shapeString(el)
		}
		return out + "]"
	case "entity":
		out := s.name + "(" + shapeString(s.left) + ")"
		if s.field != "" {
			out += "." + s.field
		}
		return out
	case "map":
		out := "{"
		for i := range s.mapKeys {
			if i > 0 {
				out += ", "
			}
			out += shapeString(s.mapKeys[i]) + ": " + shapeString(s.mapVals[i])
		}
		return out + "}"
	}
	return "?"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// exprCases are real expressions over this subset's grammar (int literals,
// identifiers, `+ - * /`, parens) — chosen to exercise precedence
// (`*`/`/` binding tighter than `+`/`-`), left-associativity of both
// levels, nested parens, and a deeper mixed tree, not just single-operator
// shapes.
var exprCases = []string{
	"42",
	"x",
	"3+4",
	"3 + 4",
	"a - b",
	"2*3+4",
	"2+3*4",
	"10-2-3",
	"10-2+3",
	"2*3*4",
	"20/4/5",
	"(2+3)*4",
	"2*(3+4)",
	"(2+3)*(4+5)",
	"2*(3+4)-5",
	"a+b*c-d/e",
	"((a))",
	"((a+b))*c",
	"1+2-3+4-5",
	"1*2/2*3",
	"x*(y+1)-(z-2)*w",
}

// TestExprArenaMatchesGoParser is this port's headline verification: for
// each real expression string, expr.fct's own tokenizer+parser (invoked
// live over HTTP) must build a tree whose SHAPE — literal values, ref
// names, operators, and left/right structure, recursively — matches
// internal/parser.ParseExpr's real ast.Expr for the exact same input.
func TestExprArenaMatchesGoParser(t *testing.T) {
	ts := loadExprApp(t)
	for _, src := range exprCases {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}

			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true (a fully valid expression in this subset)", src)
			}
		})
	}
}

// TestTokenizeMatchesGoTokenCount is a lighter-weight companion check: for
// each case, the NUMBER of tokens expr.fct's tokenizer produces must match
// how many tokens the real Go tokenizer would need to represent the same
// text (inferred indirectly: parsing succeeds and consumes every token, so
// the token count this test can directly observe — len(tokenTextsResult) —
// combined with TestExprArenaMatchesGoParser's shape check together prove
// the tokenizer isn't merging or splitting tokens differently from the real
// one while still happening to build a matching tree).
func TestTokenizeMatchesGoTokenCount(t *testing.T) {
	ts := loadExprApp(t)
	cases := map[string]int{
		"42":        1,
		"3+4":       3,
		"3 + 4":     3,
		"(a+b)*c":   7,
		"a+b*c-d/e": 9,
	}
	for src, want := range cases {
		d := postExprJSON(t, ts, "runTokenTexts", src)
		raw, ok := d["tokenTextsResult"].([]any)
		if !ok {
			t.Fatalf("%q: tokenTextsResult = %#v, want a list", src, d["tokenTextsResult"])
		}
		if len(raw) != want {
			t.Errorf("%q: got %d tokens %v, want %d", src, len(raw), raw, want)
		}
	}
}

// TestParseExprConsumedAllDetectsTrailingGarbage checks the honest status
// signal's negative case: input that is NOT one complete expression in this
// subset's grammar (trailing tokens after a complete expression) must
// report consumedAll = false, mirroring what would make the real
// parseExpr's `p.pos != len(p.toks)` check fail.
func TestParseExprConsumedAllDetectsTrailingGarbage(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want bool
	}{
		{"3+4", true},
		{"3+4)", false}, // unmatched trailing `)`: factorEnd/exprEnd never
		// consume a bare `)` at all (it's neither NUMBER/IDENT nor `(`), so
		// exprEnd(kinds,texts,0) stops at index 2 (right after "4"), one
		// short of len(kinds)==3 — correctly reported as not fully consumed.
		{"(3+4", false}, // unmatched `(`: factorEnd's LPAREN case
		// unconditionally does `return inner + 1` to skip the closing `)` —
		// even though there IS no `)` left to skip — so the computed end
		// position (5) overshoots past len(kinds)==4 entirely. This subset
		// has no error path (see expr.fct's STAGE 2a doc: a proc can't
		// return expr.go's *Error), so an unmatched `(` doesn't fail
		// cleanly the way the real Go parser's "missing `)` in expression"
		// error would — it produces a nonsensical (but never
		// false-positive) end position. Recorded here as the TRUE, observed
		// behavior of this honest partial port, not a hand-guessed
		// expectation; TestExprArenaMatchesGoParser's own exprCases are all
		// well-formed, so this malformed-input edge case is deliberately
		// exercised only here, separately, and is not claimed to match the
		// real Go parser's own (real-error) behavior for the same input.
		{"3 4", false}, // two atoms, no operator between them
		{"", true},     // empty input: 0 tokens: exprEnd(kinds,texts,0) never
		// advances past position 0 (there is nothing to consume), and
		// len(kinds)==0, so "0 == 0" is vacuously true — this subset's
		// parseExprConsumedAll reports an empty string as "fully consumed"
		// (there is no leftover token), not as "a valid expression" (it
		// isn't one; this proc only ever answers the leftover-tokens
		// question, exactly mirroring parseExpr's own `p.pos !=
		// len(p.toks)` check, which is likewise blind to whether p.pos==0
		// resulted from a real atom or from nothing at all).
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runParseExprConsumedAll", c.src)
		got, _ := d["consumedAllResult"].(bool)
		if got != c.want {
			t.Errorf("parseExprConsumedAll(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

// ============================================================
// Round 2: comparisons (== != < <= > >=), `&&`/`||`, prefix unary (- ! ~),
// and bitwise (& | ^) — six new real precedence levels plus unary, verified
// the exact same way round 1's exprCases were: real expression strings, fed
// to both the real, unmodified internal/parser.ParseExpr and expr.fct's own
// tokenizer+parser (over HTTP), compared as the same treeShape.
//
// exprCasesRound2 covers each new operator individually (2-3 expressions
// each, per operator) AND, more importantly, precedence INTERACTIONS
// between old and new operators and between the new operators themselves —
// precedence bugs only ever show up when two different levels combine in
// one expression, never in a single-operator case, so those are the cases
// that matter most here:
//
//   - "a + b == c * d"        arithmetic (8/9) binds tighter than comparison (6)
//   - "a == b && c == d"      comparison (6) binds tighter than && (2)
//   - "-a + b"                unary binds tighter than everything binary
//   - "a < b && c < d || e < f"  three-level chain: comparison < && < ||
//   - "a & b == c"            comparison (6) binds TIGHTER than `&` (5) —
//     the one interaction easy to get backwards, since in most C-family
//     languages `&` binds tighter than `==`; this table's `binPrec` says
//     otherwise (== is 6, & is 5), so this must parse as `a & (b == c)`,
//     not `(a & b) == c`
//   - "a | b & c"             `&` (5) binds tighter than `|` (3)
//   - "a ^ b | c"             `^` (4) binds tighter than `|` (3)
//   - "!a || b && c"          unary tightest, then && (2) tighter than || (1)
var exprCasesRound2 = []string{
	// comparisons, individually
	"a == b",
	"3 == 3",
	"a != b",
	"a < b",
	"a <= b",
	"a > b",
	"a >= b",
	"a < b + 1",
	// && / ||, individually
	"a && b",
	"a || b",
	"a && b && c",
	"a || b || c",
	// prefix unary, individually
	"-a",
	"!a",
	"~a",
	"--a",
	"!!a",
	"-(a+b)",
	// bitwise, individually
	"a & b",
	"a | b",
	"a ^ b",
	"a & b & c",
	"a | b | c",
	// precedence interactions: old vs. new
	"a + b == c * d",
	"a == b && c == d",
	"-a + b",
	"a < b && c < d || e < f",
	"a & b == c",
	"a == b & c",
	"a | b & c",
	"a ^ b | c",
	"!a || b && c",
	"a * b + c == d - e / f",
	"a < b + c && c > d - e",
	"(a == b) && (c == d)",
	"a == b + c * d - e",
	"a & b | c ^ d",
	"!a == b",
	"-a == -b",
	"a || b && c || d",
}

// TestExprArenaMatchesGoParserRound2 is TestExprArenaMatchesGoParser's exact
// twin, run over exprCasesRound2 instead of exprCases — same real-Go-parser
// cross-check, same shape comparison, same consumedAll assertion. Kept as a
// separate test (rather than folded into exprCases) so a round-2 regression
// is reported distinctly from a round-1 one, and so round 1's own exprCases
// stay completely untouched, per this file's own convention of never
// editing an already-verified case list.
func TestExprArenaMatchesGoParserRound2(t *testing.T) {
	ts := loadExprApp(t)
	for _, src := range exprCasesRound2 {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}

			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true (a fully valid expression in this subset)", src)
			}
		})
	}
}

// TestTokenizeRound2MultiCharOps checks the longest-match tokenization this
// round's operators need directly (not just indirectly via a tree shape
// matching): a two-character operator must be ONE token, not two, and a
// single `<`/`>`/`&`/`|` next to something that would form a real two-char
// operator elsewhere must NOT accidentally merge when it shouldn't.
func TestTokenizeRound2MultiCharOps(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want []string
	}{
		{"a == b", []string{"a", "==", "b"}},
		{"a<=b", []string{"a", "<=", "b"}},
		{"a>=b", []string{"a", ">=", "b"}},
		{"a!=b", []string{"a", "!=", "b"}},
		{"a&&b", []string{"a", "&&", "b"}},
		{"a||b", []string{"a", "||", "b"}},
		{"a<b", []string{"a", "<", "b"}},
		{"a>b", []string{"a", ">", "b"}},
		{"a&b", []string{"a", "&", "b"}},
		{"a|b", []string{"a", "|", "b"}},
		{"a^b", []string{"a", "^", "b"}},
		{"!a", []string{"!", "a"}},
		{"~a", []string{"~", "a"}},
		{"a&b&&c", []string{"a", "&", "b", "&&", "c"}}, // `&` then `&&`, not `&&` then `&`
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runTokenTexts", c.src)
		raw, ok := d["tokenTextsResult"].([]any)
		if !ok {
			t.Fatalf("%q: tokenTextsResult = %#v, want a list", c.src, d["tokenTextsResult"])
		}
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i] = v.(string)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %d tokens %v, want %d tokens %v", c.src, len(got), got, len(c.want), c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: token %d = %q, want %q (full: got %v, want %v)", c.src, i, got[i], c.want[i], got, c.want)
			}
		}
	}
}

// ============================================================
// Round 3: member access (`.field`, chained), indexing (`[i]`, chained and
// combined with member access), bool literals (`true`/`false`), and text/
// string literals (including escape sequences) — verified the exact same
// way rounds 1/2 were: real expression strings, fed to both the real,
// unmodified internal/parser.ParseExpr and expr.fct's own tokenizer+parser
// (over HTTP), compared as the same treeShape.
//
// exprCasesRound3 covers each new feature individually AND its interaction
// with the operators/levels rounds 1/2 already verified — precedence/
// binding bugs between a POSTFIX (`.field`/`[i]`) and a binary/unary
// operator only show up when both appear in one expression:
//
//   - "a.field + b.other"        member access binds tighter than `+`
//   - "a[i] == b[j]"             indexing binds tighter than `==`
//   - "!flag && a.field > 0"     unary/&&/comparison all interact with `.field`
//   - "-a[i]"                    unary binds LOOSER than postfix: `-(a[i])`,
//     not `(-a)[i]` — exactly parseUnary calling parsePostfix, never the
//     other way around
//   - "s == \"hello\""           a text literal used as a comparison operand
//   - "a.b + c.d * e.f"          member access composing with `+`/`*` precedence
//   - "(a+b).field"              a parenthesized sub-expression as a postfix's
//     OBJECT — parsePostfix always runs after parseAtom, regardless of what
//     parseAtom produced (a paren group counts too)
var exprCasesRound3 = []string{
	// member access, individually (including chained)
	"a.field",
	"a.field.other",
	"a.b.c.d",
	// indexing, individually (including chained)
	"a[i]",
	"a[0]",
	"a[i][j]",
	"a[i + 1]",
	// combined member access + indexing
	"a.field[i]",
	"a[i].field",
	"a[i].field[j]",
	"(a+b).field",
	// bool literals, individually and combined
	"true",
	"false",
	"true && false",
	"!true",
	"true || false",
	// text literals, individually (including escapes)
	`"hello"`,
	`""`,
	`"say \"hi\""`,
	`"line1\nline2"`,
	`"back\\slash"`,
	// precedence interactions: postfix vs. existing operators
	"a.field + b.other",
	"a[i] == b[j]",
	"!flag && a.field > 0",
	"-a[i]",
	`s == "hello"`,
	"a.field * 2 + b[i]",
	"a.field == true",
	"a[i] && b[j]",
	"a.b + c.d * e.f",
	"(a.field)",
	"a[i + j] - b[k]",
	"a.field != false",
}

// TestExprArenaMatchesGoParserRound3 is TestExprArenaMatchesGoParser's exact
// twin, run over exprCasesRound3 instead of exprCases — same real-Go-parser
// cross-check, same shape comparison, same consumedAll assertion. Kept as a
// separate test (rather than folded into exprCases/exprCasesRound2) so a
// round-3 regression is reported distinctly, per this file's own convention
// of never editing an already-verified case list.
func TestExprArenaMatchesGoParserRound3(t *testing.T) {
	ts := loadExprApp(t)
	for _, src := range exprCasesRound3 {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}

			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true (a fully valid expression in this subset)", src)
			}
		})
	}
}

// TestTokenizeRound3StringAndBracketTokens checks round 3's tokenization
// directly: a string literal must be ONE token spanning quote-to-quote (an
// escaped `\"` inside it must NOT end the token early), and `[`/`]`/`.` must
// each tokenize as their own single-character token (this file's header
// FINDING 6: they need no new tokenizer kind, just to be consumed by the new
// postfix level — this test proves the TOKENIZING side of that claim
// directly, independent of the parser/arena side TestExprArenaMatchesGoParserRound3
// already covers).
func TestTokenizeRound3StringAndBracketTokens(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want []string
	}{
		{`"hello"`, []string{`"hello"`}},
		{`""`, []string{`""`}},
		{`"say \"hi\""`, []string{`"say \"hi\""`}},
		{`"line1\nline2"`, []string{`"line1\nline2"`}},
		{"a.field", []string{"a", ".", "field"}},
		{"a[i]", []string{"a", "[", "i", "]"}},
		{"a[i].field", []string{"a", "[", "i", "]", ".", "field"}},
		{`x == "hi"`, []string{"x", "==", `"hi"`}},
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runTokenTexts", c.src)
		raw, ok := d["tokenTextsResult"].([]any)
		if !ok {
			t.Fatalf("%q: tokenTextsResult = %#v, want a list", c.src, d["tokenTextsResult"])
		}
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i] = v.(string)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %d tokens %v, want %d tokens %v", c.src, len(got), got, len(c.want), c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: token %d = %q, want %q (full: got %v, want %v)", c.src, i, got[i], c.want[i], got, c.want)
			}
		}
	}
}

// ============================================================
// Round 4: builtin function calls (`name(args...)`) and list literals
// (`[elems...]`) — verified the exact same way rounds 1/2/3 were: real
// expression strings, fed to both the real, unmodified
// internal/parser.ParseExpr and expr.fct's own tokenizer+parser (over
// HTTP), compared as the same treeShape.
//
// Two real grammar findings shaped this round's case list (see expr.fct's
// own ROUND 4 SCOPE NOTE for the full reasoning):
//
//   - This language has no user-defined functions in expression position —
//     `name(args)` only ever means a call to one of expr.go's fixed builtin
//     names (isBuiltinCall). An arbitrary identifier followed by `(` that is
//     NOT one of those names is `Entity(key).field` lookup syntax in the
//     real grammar (a different AST shape, ast.EntityGet) — genuinely out of
//     scope this round, not merely untested. exprCasesRound4 therefore only
//     ever calls real builtin names (len/abs/trim/contains/upper/slice/
//     charAt/append/...), the same allowlist expr.fct's own isCallNameTok
//     uses (minus min/max — see below).
//   - `min`/`max` are excluded from this round's call-name allowlist
//     entirely, because they are genuinely ambiguous with the (also out of
//     scope) aggregate grammar in the real parser (`min(x)` alone is an
//     aggregate; `min(a, b)` is a call) — see
//     TestCallDisambiguationAndOutOfScopeNames below for this exact
//     divergence, checked directly against this port's own honest
//     behavior rather than against the real Go parser (which would produce
//     a wholly different AST shape for some of these inputs).
//
// exprCasesRound4 covers, per the task's own priority list: calls with 0,
// 1, 2, and 3+ arguments; nested calls; calls with complex argument
// expressions; list literals with 0, 1, and several elements; nested list
// literals (confirmed in scope — expr.go's own parseListLit parses each
// element via parseBinary(0), which can itself bottom out in another `[`);
// lists of complex expressions; and the disambiguation cases that matter
// most (`a[0]` index vs. `[0]` list literal; `len` bare ref vs. `len(x)`
// call; a list literal immediately indexed, `[1, 2][0]`); plus precedence
// interactions between calls/lists and every earlier round's operators.
var exprCasesRound4 = []string{
	// calls: 0, 1, 2, 3+ arguments
	"len()",
	"len(x)",
	"abs(x)",
	"trim(a)",
	"contains(a, b)",
	"slice(a, b, c)",
	"charAt(s, i)",
	"append(a, b, c)",
	// nested calls
	"contains(trim(a), upper(b))",
	"abs(len(x))",
	"len(trim(upper(x)))",
	// calls with complex argument expressions
	"abs(a + b)",
	"len(a.field)",
	"contains(a.field, b + 1)",
	"trim(a[i])",
	"contains(a == b, c != d)",
	// list literals: 0, 1, several elements
	"[]",
	"[1]",
	"[1, 2, 3]",
	"[a, b, c]",
	// nested list literals
	"[1, [2, 3], 4]",
	"[[1, 2], [3, 4]]",
	"[[]]",
	// lists of complex expressions
	"[a + b, c.field, len(x)]",
	"[a == b, c != d]",
	// disambiguation: index vs. list literal
	"a[0]",
	"[0]",
	"[1, 2][0]",
	"a[0] + [1, 2][0]",
	"[a, b][i]",
	// disambiguation: bare ref vs. call
	"len",
	"len(x)",
	"len + x",
	"abs",
	"abs(x) + abs(y)",
	// precedence interactions
	"2 + len(x)",
	"len(x) + 2",
	"-len(x)",
	"!contains(a, b) && c",
	"len(x) == 0",
	"len(x) > 0 && len(y) > 0",
}

// TestExprArenaMatchesGoParserRound4 is TestExprArenaMatchesGoParser's exact
// twin, run over exprCasesRound4 instead of exprCases — same real-Go-parser
// cross-check, same shape comparison, same consumedAll assertion. Kept as a
// separate test (rather than folded into exprCases/exprCasesRound2/
// exprCasesRound3) so a round-4 regression is reported distinctly, per this
// file's own convention of never editing an already-verified case list.
func TestExprArenaMatchesGoParserRound4(t *testing.T) {
	ts := loadExprApp(t)
	for _, src := range exprCasesRound4 {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}

			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true (a fully valid expression in this subset)", src)
			}
		})
	}
}

// TestCallDisambiguationAndOutOfScopeNames checks the real, documented
// divergences from the real Go parser directly (not via shapesEqual against
// parser.ParseExpr, which would produce a genuinely different AST —
// ast.Agg — for the min/max cases): a name immediately followed by `(` that
// is NOT in this port's builtin-call allowlist (isCallNameTok) and IS one of
// the reserved aggregate/action-state names (isEntityNameTok) is honestly
// left UNCONSUMED, exactly like any other out-of-scope construct in this
// file (see TestParseExprConsumedAllDetectsTrailingGarbage for the
// established pattern this mirrors) — never silently misparsed, never a
// crash.
//
//   - "foo(x)": UPDATED in round 5 — "foo" is not a builtin name, but round
//     5 now implements the real grammar's own fallback for exactly this case
//     (`Entity(key).field`-shaped syntax, ast.EntityGet — see expr.fct's
//     ROUND 5 SCOPE NOTE). This port's isEntityLookupStartAt now returns
//     true for "foo(x)" (not a builtin, not a reserved aggregate/action-
//     state name), so it IS now fully consumed, as a real EntityGet — the
//     opposite of round 4's documented behavior for this exact input, and a
//     deliberate, intentional change (not a regression): round 4's own
//     SCOPE NOTE named this precise gap as what a future round would close.
//     See TestEntityLookupDisambiguation, below, for the shape-level proof
//     (cross-checked against the real Go parser) that this is now a genuine,
//     correct ast.EntityGet, not a guess.
//   - "min(a, b)" / "max(x)": deliberately excluded from BOTH isCallNameTok
//     AND isEntityNameTok's entity-lookup candidacy (ROUND 4 SCOPE NOTE,
//     finding 2, and ROUND 5 SCOPE NOTE) because the real grammar's own
//     disambiguation for these two names (call vs. aggregate, decided by
//     whether the argument list has a top-level comma) is itself out of
//     scope — including them here (as a call OR an entity lookup) would risk
//     a WRONG tree shape, not just an incomplete one, so both keep degrading
//     the same honest "not consumed" way, regardless of arity.
//   - "count(x)" / "sum(Coll.field)" / "pending(action)": the OTHER reserved
//     aggregate/action-state names (isEntityNameTok) — still unconsumed,
//     confirming these did NOT accidentally start being treated as entity
//     lookups just because they aren't builtin call names either.
//   - "len" alone (no trailing `(`) and "len + x" both confirm the other
//     direction: a builtin name NOT immediately followed by `(` is still
//     just a bare Ref and IS fully consumed — isCallStartAt's lookahead
//     never fires without an immediately-adjacent `(`.
func TestCallDisambiguationAndOutOfScopeNames(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want bool
	}{
		{"foo(x)", true},     // round 5: non-builtin, non-reserved name + `(` is now a real EntityGet
		{"min(a, b)", false}, // min/max deliberately excluded (aggregate ambiguity) — unchanged
		{"max(x)", false},
		{"count(x)", false}, // reserved aggregate name: still out of scope, still unconsumed
		{"sum(Coll.field)", false},
		{"pending(action)", false}, // reserved action-state name: still out of scope, still unconsumed
		{"len", true},              // builtin name, no `(`: just a bare Ref, fully consumed
		{"len + x", true},          // ditto, mid-expression
		{"len(x)", true},           // builtin name + `(`: a real call, fully consumed
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runParseExprConsumedAll", c.src)
		got, _ := d["consumedAllResult"].(bool)
		if got != c.want {
			t.Errorf("parseExprConsumedAll(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

// TestIndexVsListLitDisambiguation is this round's headline new-grammar-
// boundary check: `a[0]` (postfix indexing — `[` appearing AFTER an atom
// already exists) and `[0]` (a list literal — `[` appearing where a fresh
// atom is expected) must never be confused, in either direction, including
// when they appear right next to each other in the same expression. Cross-
// checked against the real Go parser's own ast.Expr, exactly like
// TestExprArenaMatchesGoParserRound4 above (folded into that test's own
// case list too; kept as its own focused test so this specific boundary is
// never accidentally deleted or diluted by an unrelated future edit to
// exprCasesRound4).
func TestIndexVsListLitDisambiguation(t *testing.T) {
	ts := loadExprApp(t)
	cases := []string{
		"a[0]",             // index: `[` after an existing atom `a`
		"[0]",              // list literal: `[` starting a fresh atom
		"[1, 2][0]",        // a list literal immediately indexed
		"a[0] + [1, 2][0]", // both forms in one expression
		"[a, b][i]",        // list literal of refs, indexed by a ref
		"a[[0][0]]",        // a list literal used AS an index expression
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}
			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true", src)
			}
		})
	}
}

// ============================================================
// Round 5: float literals, `Entity(key).field` lookups (including bare
// `Entity(key)`, no field), and map literals (`{k: v, ...}`) — verified the
// exact same way rounds 1/2/3/4 were: real expression strings, fed to both
// the real, unmodified internal/parser.ParseExpr and expr.fct's own
// tokenizer+parser (over HTTP), compared as the same treeShape.
//
// exprCasesRound5 covers, per the task's own priority order: float literals
// individually and interacting with existing operators/postfixes/list
// literals; entity lookups bare, with a single field, with chained access
// afterward (`.field.other`, `[i]` on top of a bare lookup), with a complex
// key expression, and the headline disambiguation case the task called out
// by name — a non-builtin, non-reserved name followed by `(` (`foo(x)`,
// `Widget(42).price`) now parsing as a real EntityGet, not staying
// unconsumed the way round 4 left it; map literals empty/single/multi-entry,
// with text and expression keys/values, nested inside list literals and
// nested inside each other, and combined with postfix chaining
// (`{1: 2}[1]`, `{1: 2}.field` — syntactically legal even though
// semantically odd, exactly like round 4's own `trim(s)[0]` case already
// established this file tests the GRAMMAR boundary, not semantic sense).
var exprCasesRound5 = []string{
	// float literals, individually
	"3.14",
	"0.5",
	"1.0",
	"100.001",
	// float literals, interacting with existing operators/postfixes
	"3.14 + 2.5",
	"1.5 * 2.0",
	"-2.5",
	"a + 1.5",
	"1.5 == 1.5",
	"1.5 < 2.5 && 3.5 > 1.0",
	"a.field + 1.5",
	"(1.5 + 2.5) * 2.0",
	// float literals inside round-4 constructs
	"[1.5, 2.5, 3.5]",
	"len([1.5, 2.5])",
	"[1, 2.5, 3]",

	// entity lookups: bare, with field, chained
	"Post(id)",
	"Post(id).title",
	"Post(1).author.name",
	"Post(a + b)",
	"Post(id)[0]",
	"Post(id).items[0]",
	"Post(a).field + Post(b).other",
	"Widget(42).price",
	"Order(x).total == 0",
	"-Post(id).amount",
	"Post(id).amount + 1.5",
	// the headline disambiguation case: a non-builtin, non-reserved name
	// followed by `(` is a real entity lookup now (round 4 left this
	// unconsumed — see TestCallDisambiguationAndOutOfScopeNames's own
	// updated doc for the exact behavior change)
	"foo(x)",

	// map literals: empty, single, multi-entry
	"{}",
	"{1: 2}",
	"{1: 2, 3: 4}",
	`{"a": 1, "b": 2}`,
	"{a: b}",
	// map literals nested with round-4 constructs
	"{1: [1, 2, 3]}",
	"[{1: 2}, {3: 4}]",
	"{1: {2: 3}}",
	"{len(x): 1}",
	"{1: len(x)}",
	// map literal postfix interactions (grammar-legal, not semantically
	// meaningful — same spirit as round 4's own `trim(s)[0]`)
	"{1: 2}[1]",
	"{1: 2}.field",
	// precedence interactions inside map literal keys/values
	"{1: a + b}",
	"{a == b: 1}",
	"{1: a && b}",
}

// TestExprArenaMatchesGoParserRound5 is TestExprArenaMatchesGoParser's exact
// twin, run over exprCasesRound5 instead of exprCases — same real-Go-parser
// cross-check, same shape comparison, same consumedAll assertion. Kept as a
// separate test (rather than folded into exprCases/exprCasesRound2/
// exprCasesRound3/exprCasesRound4) so a round-5 regression is reported
// distinctly, per this file's own convention of never editing an
// already-verified case list.
func TestExprArenaMatchesGoParserRound5(t *testing.T) {
	ts := loadExprApp(t)
	for _, src := range exprCasesRound5 {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)

			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}

			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true (a fully valid expression in this subset)", src)
			}
		})
	}
}

// TestTokenizeRound5FloatAndBraceTokens checks round 5's tokenization
// directly: a float literal must be ONE token (`<digits>.<digits>`, not
// split at the `.`), a bare trailing dot with no digit after it must NOT be
// absorbed into the number (`3.` tokenizes as NUM("3") then its own `.`
// operator token, mirroring expr.go's own tokenize rule exactly — see
// expr.fct's tokenKinds/tokenTexts doc), and `{`/`}`/`:` must each tokenize
// as their own single-character token (this file's ROUND 5 SCOPE NOTE: they
// need no new tokenizer kind, the same "any operator character" fallthrough
// FINDING 6 already established for `[`/`]`/`.`) — this test proves the
// TOKENIZING side of both claims directly, independent of the parser/arena
// side TestExprArenaMatchesGoParserRound5 already covers.
func TestTokenizeRound5FloatAndBraceTokens(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want []string
	}{
		{"3.14", []string{"3.14"}},
		{"0.5", []string{"0.5"}},
		{"100.001", []string{"100.001"}},
		{"3.", []string{"3", "."}},             // trailing dot, no digit after: NOT a float
		{"3.14.5", []string{"3.14", ".", "5"}}, // greedy float, then leftover `.5`-shaped tokens
		{"a+3.14", []string{"a", "+", "3.14"}},
		{"3.14+2", []string{"3.14", "+", "2"}},
		{"{}", []string{"{", "}"}},
		{"{1: 2}", []string{"{", "1", ":", "2", "}"}},
		{"{1: 2, 3: 4}", []string{"{", "1", ":", "2", ",", "3", ":", "4", "}"}},
		{"Post(id).field", []string{"Post", "(", "id", ")", ".", "field"}},
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runTokenTexts", c.src)
		raw, ok := d["tokenTextsResult"].([]any)
		if !ok {
			t.Fatalf("%q: tokenTextsResult = %#v, want a list", c.src, d["tokenTextsResult"])
		}
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i] = v.(string)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %d tokens %v, want %d tokens %v", c.src, len(got), got, len(c.want), c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: token %d = %q, want %q (full: got %v, want %v)", c.src, i, got[i], c.want[i], got, c.want)
			}
		}
	}
}

// TestFloatTrailingDotConsumedAll checks "3." (trailing dot, no digit after
// it) directly against the honest consumedAll signal, WITHOUT going through
// shapeFromArena/parser.ParseExpr — real Go's ParseExpr actually ERRORS on
// this input (parseBinary consumes NUM("3"), then the top-level parseExpr
// check `p.pos != len(p.toks)` fails on the leftover `.` token, since `.` is
// not a binary operator in binPrec), so this can't be a shapesEqual
// cross-check case the way exprCasesRound5's well-formed inputs are — it
// belongs here instead, alongside TestParseExprConsumedAllDetectsTrailingGarbage's
// own established pattern for malformed input.
func TestFloatTrailingDotConsumedAll(t *testing.T) {
	ts := loadExprApp(t)
	cases := []struct {
		src  string
		want bool
	}{
		{"3.14", true}, // a real float, fully consumed
		{"3.", false},  // trailing dot with no digit after: NUM("3") then a
		// leftover `.` token this subset's postfix loop tries to read a
		// field name after (honestly degrading, not crashing — the same
		// "optimistic advance past whatever token is there" convention
		// factorEnd's unmatched `(` case and entityGetEnd already use),
		// overshooting past the end of the token stream; consumedAll is
		// false either way, since 2 real tokens exist and neither `*End`
		// path leaves position 2 exactly.
		{"3.14.5", true}, // SURPRISING but correct for this honest port: 3
		// tokens ("3.14", ".", "5"), and postfixEnd's `.` case unconditionally
		// advances 2 tokens for ANY `.` (this subset never validates that
		// what follows a `.` is really an IDENT — a pre-existing round-3
		// limitation of postfixEnd/postfixNodes' field-name handling,
		// unrelated to floats specifically, just newly reachable via a
		// float's own trailing-dot edge case): "3.14" then "." then "5" is
		// formally fully consumed as Get(Lit(3.14), field="5"), even though
		// "5" is a NUMBER token, not a real field-name IDENT. The real Go
		// parser's fieldName() DOES check this and returns a real error
		// ("expected a field name after `.`") — see this test's own doc for
		// why "3.14.5" is therefore never used in a shapesEqual cross-check
		// (parser.ParseExpr would fail with an error, not build a tree at
		// all), only tested here for the honest-but-surprising consumedAll
		// answer this port actually gives.
	}
	for _, c := range cases {
		d := postExprJSON(t, ts, "runParseExprConsumedAll", c.src)
		got, _ := d["consumedAllResult"].(bool)
		if got != c.want {
			t.Errorf("parseExprConsumedAll(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

// TestEntityLookupDisambiguation is this round's headline new-grammar-
// boundary check, mirroring TestIndexVsListLitDisambiguation's own role for
// round 4: a plain call-shaped name that is a builtin (`len(x)`), a reserved
// aggregate/action-state name (`count(x)`, `pending(a)` — still out of scope,
// still unconsumed), or neither (a real entity lookup, `Post(id)`) must never
// be confused, cross-checked against the real Go parser's own ast.Expr where
// that's meaningful (the reserved-name cases produce a DIFFERENT real AST —
// ast.Agg/ast.ActState — so those are checked via consumedAll only, mirroring
// TestCallDisambiguationAndOutOfScopeNames's own established split).
func TestEntityLookupDisambiguation(t *testing.T) {
	ts := loadExprApp(t)
	shapeCases := []string{
		"Post(id)",       // entity lookup: bare, no field
		"Post(id).title", // entity lookup: with field
		"len(x)",         // builtin call: unaffected by entity-lookup addition
		"foo(x)",         // non-builtin, non-reserved: now a real entity lookup
		"Widget(1).name",
		"Order(a + b).total",
	}
	for _, src := range shapeCases {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)
			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}
			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true", src)
			}
		})
	}

	reservedCases := []struct {
		src  string
		want bool
	}{
		{"count(x)", false},
		{"sum(Coll.field)", false},
		{"exists(x)", false},
		{"avg(Coll.field)", false},
		{"min(a, b)", false},
		{"max(x)", false},
		{"pending(action)", false},
		{"failed(action)", false},
		{"dirty(x)", false},
		{"touched(x)", false},
	}
	for _, c := range reservedCases {
		d := postExprJSON(t, ts, "runParseExprConsumedAll", c.src)
		got, _ := d["consumedAllResult"].(bool)
		if got != c.want {
			t.Errorf("parseExprConsumedAll(%q) = %v, want %v (reserved aggregate/action-state name — never an entity lookup)", c.src, got, c.want)
		}
	}
}

// TestMapLitDisambiguation is this round's other new-grammar-boundary check:
// `{` starting a fresh atom (a map literal) must never be confused with
// anything else — including an empty map (`{}`) vs. a map literal
// immediately used as a postfix base (`{1:2}[1]`, `{1:2}.field`), and a map
// literal nested inside a list literal or another map literal — cross-
// checked against the real Go parser's own ast.Expr, exactly like
// TestIndexVsListLitDisambiguation does for round 4's `[`/`]` boundary.
// Folded into exprCasesRound5 too; kept as its own focused test so this
// specific boundary is never accidentally diluted by an unrelated future
// edit to exprCasesRound5.
func TestMapLitDisambiguation(t *testing.T) {
	ts := loadExprApp(t)
	cases := []string{
		"{}",
		"{1: 2}",
		"{1: 2}[1]",
		"{1: 2}.field",
		"[{1: 2}, {3: 4}]",
		"{1: {2: 3}}",
		"{1: [1, 2], 3: [4, 5]}",
	}
	for _, src := range cases {
		t.Run(src, func(t *testing.T) {
			wantExpr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("real Go parser.ParseExpr(%q): %v", src, err)
			}
			want := shapeFromGoExpr(t, wantExpr)

			d := postExprJSON(t, ts, "runParseExpr", src)
			arenaRaw, ok := d["arenaResult"].([]any)
			if !ok {
				t.Fatalf("arenaResult = %#v, want a list", d["arenaResult"])
			}
			arena := make([]int, len(arenaRaw))
			for i, v := range arenaRaw {
				arena[i] = int(v.(float64))
			}
			textsRaw, ok := d["tokenTextsResult"].([]any)
			if !ok {
				t.Fatalf("tokenTextsResult = %#v, want a list", d["tokenTextsResult"])
			}
			texts := make([]string, len(textsRaw))
			for i, v := range textsRaw {
				texts[i] = v.(string)
			}
			if len(arena)%5 != 0 || len(arena) == 0 {
				t.Fatalf("arena length = %d, want a positive multiple of 5", len(arena))
			}
			rootIdx := len(arena)/5 - 1
			got := shapeFromArena(t, arena, texts, rootIdx)
			if !shapesEqual(got, want) {
				t.Errorf("%q:\n  got  %s\n  want %s (real Go parser.ParseExpr)", src, shapeString(got), shapeString(want))
			}
			if consumedOK, _ := d["consumedAllResult"].(bool); !consumedOK {
				t.Errorf("%q: parseExprConsumedAll = false, want true", src)
			}
		})
	}
}
