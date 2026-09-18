package runtime

import (
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"facet/internal/ir"
)

// record is one entity row.
type record = map[string]any

// frame is one proc invocation's local scope: its parameters plus its
// `let`-bound locals, distinct from the flat action/session `map[string]any`
// scope runActionLocked reads — a proc's scratch variable must never alias or
// leak into session state or wire deltas, which a shared map could not guarantee
// (a proc-local named the same as a state cell or an action parameter would
// silently collide). It chains to a parent so a nested block can shadow an
// outer local without a flat namespace: runProcLocked builds the proc's own
// top-level frame (parent == nil), and execProcBlock (runtime/server.go) gives
// each `loop` iteration a fresh child frame chained to the frame it was called
// with, so a loop-local declared inside the body doesn't survive past that one
// pass while a `let mut` declared OUTSIDE the loop and reassigned inside it
// still resolves (via frame.set walking the chain) to the same outer slot every
// iteration — the whole point of an accumulator.
type frame struct {
	vars   map[string]any
	parent *frame
}

// get resolves a name by walking the frame chain from the innermost scope
// outward.
func (f *frame) get(name string) (any, bool) {
	for fr := f; fr != nil; fr = fr.parent {
		if v, ok := fr.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// set reassigns name in whichever frame of the chain already declares it (a
// `let mut` local written back to by a plain `name = expr`); the compiler
// guarantees the name was declared by a `let` first, so it is always found.
func (f *frame) set(name string, v any) {
	for fr := f; fr != nil; fr = fr.parent {
		if _, ok := fr.vars[name]; ok {
			fr.vars[name] = v
			return
		}
	}
	f.vars[name] = v
}

// evalInFrame evaluates an IR expression against a proc's frame chain instead of
// the flat scope map eval() reads — the resolver a proc's own scope needs (see
// frame, above). A proc's expressions are restricted at compile time
// (internal/ir/build.go, checkProcExpr) to literals, its own locals/params,
// arithmetic/comparison/boolean/bitwise operators, pure builtins, and array
// literals/index reads — never a state read, an entity/aggregate read, or
// client reactive state — so those are the only IR node kinds this needs to
// know about. Where the logic is identical to eval's (binary/unary operators,
// builtin calls), it delegates to the same helpers eval uses, so the two
// interpreters cannot silently drift apart.
//
// Unlike eval(), this returns an error as its second result: an array index
// read (case "index") is the one place a proc expression can fail at runtime
// in a way no compile-time check can rule out (bounds are data-dependent, not
// static — see ast.Index) — reading past an array's length, or indexing a
// value that isn't an array despite the compile-time check on the common
// `name[i]` shape (internal/ir/build.go's checkIndexTypes only catches
// that shape; anything it couldn't prove statically is checked here instead).
// That error threads back up through every caller in this function and in
// runtime/server.go's execProcBlock/execProcLoop, the same way a `do` call's
// own failure already does, and ends the request as a clean 5xx rather than a
// Go panic reaching the HTTP layer.
func evalInFrame(e *ir.Expr, fr *frame) (any, error) {
	if e == nil {
		return nil, nil
	}
	switch e.Kind {
	case "lit":
		return litValue(e), nil
	case "list":
		out := make([]any, len(e.Args))
		for i, el := range e.Args {
			v, err := evalInFrame(el, fr)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case "ref":
		v, _ := fr.get(e.Name)
		return v, nil
	case "index":
		obj, err := evalInFrame(e.Obj, fr)
		if err != nil {
			return nil, err
		}
		idxV, err := evalInFrame(e.Key, fr)
		if err != nil {
			return nil, err
		}
		arr, ok := obj.([]any)
		if !ok {
			return nil, fmt.Errorf("cannot index a non-array value")
		}
		idx := toInt(idxV)
		if idx < 0 || idx >= len(arr) {
			return nil, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(arr))
		}
		return arr[idx], nil
	case "un":
		x, err := evalInFrame(e.X, fr)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case "!":
			return !truthy(x), nil
		case "-":
			return -toInt(x), nil
		case "~":
			return ^toInt(x), nil
		}
	case "bin":
		l, err := evalInFrame(e.L, fr)
		if err != nil {
			return nil, err
		}
		r, err := evalInFrame(e.R, fr)
		if err != nil {
			return nil, err
		}
		return applyBin(e.Op, l, r), nil
	case "call":
		args := make([]any, len(e.Args))
		for i, a := range e.Args {
			v, err := evalInFrame(a, fr)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		return callBuiltin(e.Name, args), nil
	}
	return nil, nil
}

// cloneArrayValue returns v unchanged unless it is a []any (an array value),
// in which case it returns a fresh copy with its own backing array.
//
// This is what gives a proc-local array value semantics instead of Go's
// default slice-aliasing assignment: called at every point a value flows into
// a NEW binding — a `let`, a plain `name = expr` reassignment, and a proc
// parameter — so `let ys = xs` (or passing xs as an argument) never leaves ys
// and xs sharing a backing array. Mutating one afterward (via an index-write,
// runtime/server.go's "indexset") therefore cannot be observed through the
// other. This matches the rest of the language's copy-on-read value model
// (an action's state cells are never shared references either) rather than
// adding a new, inconsistent reference-semantics value kind; the cost is one
// copy per assignment, paid only for actual array values (everything else —
// the overwhelming majority of assignments — returns immediately unchanged).
func cloneArrayValue(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, len(arr))
	copy(out, arr)
	return out
}

// eval interprets an IR expression over a scope (state + entities + locals like
// action params, item vars, and `actor`). It is the server half of the one
// shared expression semantics (the client half is assets/facet.js); both must
// agree, which is why the language is deliberately small.
func eval(e *ir.Expr, scope map[string]any) any {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case "lit":
		return litValue(e)
	case "list":
		out := make([]any, len(e.Args))
		for i, el := range e.Args {
			out[i] = eval(el, scope)
		}
		return out
	case "ref":
		return scope[e.Name]
	case "get":
		obj := eval(e.Obj, scope)
		if m, ok := obj.(record); ok {
			return m[e.Field]
		}
		if m, ok := obj.(map[string]any); ok {
			return m[e.Field]
		}
		return nil
	case "eget", "agg":
		// A collection read. During a page render these are materialized: the
		// browser will re-evaluate this very expression with no collection to scan
		// (see runtime/region.go), so the render records what it computed, keyed by
		// where it computed it. The lookup doubles as memoization — the same
		// aggregate at the same render path is the same question — and everything
		// that evaluates without a collector (actions, policies, the API) is
		// unaffected.
		m := materializerOf(scope)
		if v, hit := m.lookup(e); hit {
			return v
		}
		// An aggregate ranges over a collection an entity's `read:` clause may
		// restrict; ee is what actually gets resolved/scanned below, e is what
		// still gets recorded — see withEntityReadPolicy on why those must stay
		// two different values. EntityGet ("eget") is deliberately left alone: a
		// row-read policy governs a list, not a lookup by id (see ast.Entity.Read).
		ee := e
		if e.Kind == "agg" {
			ee = withEntityReadPolicy(m, e)
		}
		// A count/exists the database can answer is asked of the database: that is
		// what stops the in-memory mirror from being the only thing that can answer
		// it. Anything else — a sum, an unpushable predicate, an aggregate a list
		// could not batch — is counted here, over the working set, as before.
		if v, ok := m.resolveAgg(ee, scope); ok {
			return m.record(e, v)
		}
		v := evalColl(ee, scope)
		if e.Kind == "eget" && e.Field == "" {
			// A whole row is about to be recorded for the client, which re-renders
			// this lookup from the recorded value. A field this actor may not read
			// (`@requires`) leaves here the way it leaves every other projection —
			// stripped by visibleRows — so the row a component is handed is the
			// same row on both sides, and nothing gated rides along in `@aggs`.
			v = m.gateRow(e.Name, v, scope)
		}
		return m.record(e, v)
	}
	return evalRest(e, scope)
}

// evalColl evaluates the two forms that read a whole collection: an
// `Entity(id).field` lookup and an aggregate over (optionally filtered) rows.
func evalColl(e *ir.Expr, scope map[string]any) any {
	switch e.Kind {
	case "eget":
		rows, _ := scope[e.Name].([]any)
		key := eval(e.Key, scope)
		for _, r := range rows {
			if m, ok := r.(record); ok && equal(m["id"], key) {
				if e.Field == "" {
					return m // `Post(id)`: the row itself
				}
				if v, ok := m[e.Field]; ok {
					return v
				}
				// Not a stored key: `Post(id).lineTotal` may be naming an
				// entity-carried derive (ast.Entity.Derives), which is never a
				// map entry — see entityDeriveValue.
				if v, ok := entityDeriveValue(scope, e.Name, e.Field, m); ok {
					return v
				}
				return nil
			}
		}
		return nil
	}
	{
		rows, _ := scope[e.Name].([]any)
		// Filtered form: keep only rows the predicate accepts, with the item
		// variable bound to each row. Mutate-and-restore keeps eval allocation-free.
		//
		// evalRowPredicate, not eval: this predicate is answered once per row while
		// the render path stands still, so a nested aggregate inside it has no
		// address of its own and must be computed for the row that is bound rather
		// than read back from the one the first row wrote.
		if e.Where != nil {
			prev, had := scope[e.Var]
			kept := make([]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(record); ok {
					scope[e.Var] = m
					if evalRowPredicate(e.Where, scope) {
						kept = append(kept, r)
					}
				}
			}
			if had {
				scope[e.Var] = prev
			} else {
				delete(scope, e.Var)
			}
			rows = kept
		}
		switch e.Op {
		case "exists":
			return len(rows) > 0
		case "count":
			return len(rows)
		}
		// sum/avg/min/max reduce a numeric value over the (filtered) rows: a
		// bare column, or an expression evaluated once per row.
		if e.Sel == nil {
			return reduceAgg(e.Op, rows, fieldValue(e.Field))
		}
		prev, had := scope[e.Var]
		defer func() {
			if had {
				scope[e.Var] = prev
			} else {
				delete(scope, e.Var)
			}
		}()
		return reduceAgg(e.Op, rows, func(r any) (int, bool) {
			m, ok := r.(record)
			if !ok {
				return 0, false
			}
			scope[e.Var] = m
			// evalPerRow, not eval, for the reason the filter uses it: this
			// value is computed once per row while the render path stands
			// still, so a nested aggregate or lookup inside it has no address
			// of its own and must be computed for the row that is bound rather
			// than read back from the one the first row wrote.
			return toInt(evalPerRow(e.Sel, scope)), true
		})
	}
}

// fieldValue reads one column off a row — the reduced value of the bare form,
// `sum(x.amount in …)`.
//
// A row that is not a record contributes nothing and is not counted, which is
// what keeps `avg`'s divisor honest when a collection holds something that is
// not a row at all.
func fieldValue(field string) func(row any) (int, bool) {
	return func(r any) (int, bool) {
		m, ok := r.(record)
		if !ok {
			return 0, false
		}
		return toInt(m[field]), true
	}
}

// reduceAgg folds rows to the value `sum`/`avg`/`min`/`max` reduces them to.
//
// This is **the** definition of what those four mean in this language, and it is
// a function rather than a block inside the interpreter because three separate
// things have to produce the same number for the same rows: the interpreter
// reading the in-memory working set, `memStore` answering the same question as a
// store, and — through the values the durable stores are made to return — a
// pushed-down `sum(...)` that never brings a row into this process at all. A
// rendered page can resolve one aggregate through the database and the next
// through the mirror, and a viewer must not be able to tell which.
//
// The rules that are easy to get wrong, stated once:
//
//   - **The empty reduction is 0, for every one of the four.** Not an error and
//     not a null: the language types these as the field's own numeric type, and
//     there is no hole in an `int`. `max(Order.amount)` over no orders is 0.
//   - **`avg` is integer division**, because the language has no float. It
//     divides by the rows that matched, not by the rows that carried a value.
//   - A row that holds nothing at the field contributes `toInt(nil)`, which is
//     0 — it is not skipped. Reachable only for a column added by a migration to
//     rows that predate it, since every declared field is written on every
//     insert.
//
// `value` is what one row contributes, and it is a function rather than a field
// name because the language has two forms: a bare column (`sum(x.amount in …)`,
// see [fieldValue]) and an expression reduced over each row
// (`sum(l.qty * l.unitPrice in …)`). Both fold identically once the number is in
// hand, which is the point of taking it this way — the store, which can only
// reduce a stored column, hands in the first and never has to know about the
// second. Returning false means the row contributes nothing AND is not counted,
// so it stays out of `avg`'s divisor.
func reduceAgg(op string, rows []any, value func(row any) (int, bool)) int {
	total, n := 0, 0
	var lo, hi int
	for _, r := range rows {
		v, ok := value(r)
		if !ok {
			continue
		}
		if n == 0 {
			lo, hi = v, v
		} else {
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
		}
		total += v
		n++
	}
	switch op {
	case "avg":
		if n == 0 {
			return 0
		}
		return total / n
	case "min":
		return lo // 0 over an empty range
	case "max":
		return hi
	default: // sum
		return total
	}
}

// evalRest is the remainder of the interpreter: everything that does not read a
// collection, split out so eval's collection cases stay legible.
func evalRest(e *ir.Expr, scope map[string]any) any {
	switch e.Kind {
	case "astate":
		// Action status and form-field status are client-only runtime state; the
		// server has none at render time, so first paint shows "not pending" / "no
		// error" / "not dirty" / "not touched".
		if e.Op == "failed" {
			return ""
		}
		return false // pending | dirty | touched
	case "call":
		return evalCall(e, scope)
	case "un":
		x := eval(e.X, scope)
		switch e.Op {
		case "!":
			return !truthy(x)
		case "-":
			return -toInt(x)
		case "~":
			return ^toInt(x)
		}
	case "bin":
		return applyBin(e.Op, eval(e.L, scope), eval(e.R, scope))
	}
	return nil
}

// applyBin evaluates one binary operator over its already-evaluated operands.
// Split out of evalRest's "bin" case so evalInFrame (a proc body's expression
// evaluator, which has no flat scope map to hand eval) can share the exact same
// operator semantics rather than reimplementing them — eval() and evalInFrame()
// must never disagree about what `+`/`==`/etc. mean.
func applyBin(op string, l, r any) any {
	switch op {
	case "&&":
		return truthy(l) && truthy(r)
	case "||":
		return truthy(l) || truthy(r)
	case "+":
		if ls, ok := l.(string); ok {
			return ls + toStr(r)
		}
		if rs, ok := r.(string); ok {
			return toStr(l) + rs
		}
		return toInt(l) + toInt(r)
	case "-":
		return toInt(l) - toInt(r)
	case "*":
		return toInt(l) * toInt(r)
	case "/":
		if toInt(r) == 0 {
			return 0
		}
		return toInt(l) / toInt(r)
	case "%":
		if toInt(r) == 0 {
			return 0
		}
		return toInt(l) % toInt(r)
	case "==":
		return equal(l, r)
	case "!=":
		return !equal(l, r)
	case "<":
		return toInt(l) < toInt(r)
	case "<=":
		return toInt(l) <= toInt(r)
	case ">":
		return toInt(l) > toInt(r)
	case ">=":
		return toInt(l) >= toInt(r)
	case "in":
		items, _ := r.([]any)
		for _, el := range items {
			if equal(l, el) {
				return true
			}
		}
		return false
	case "&":
		return toInt(l) & toInt(r)
	case "|":
		return toInt(l) | toInt(r)
	case "^":
		return toInt(l) ^ toInt(r)
	case "<<":
		return shiftLeft(toInt(l), toInt(r))
	case ">>":
		return shiftRight(toInt(l), toInt(r))
	}
	return nil
}

// shiftLeft and shiftRight implement `<<`/`>>` on this runtime's int (a plain
// Go `int` — 64-bit two's complement on every platform this compiles for).
// Go itself panics at run time on a negative shift count (the shift count
// must be non-negative), which is not an acceptable way for an .fct value to
// misbehave — the same reasoning applyBin already applies to `/` and `%` by
// zero, just for a different illegal operand: a negative shift count is
// defined here as 0, the same sentinel division/modulo by zero already
// answers with, rather than propagating Go's panic into a request.
//
// A count at or beyond the operand's bit width is NOT special-cased: unlike C,
// Go's own shift semantics are already well-defined for any non-negative
// count (as if shifted one bit at a time that many times), so `1 << 100`
// naturally yields 0 and `-1 >> 100` naturally yields -1 — exactly the
// two's-complement behavior a bit-manipulation algorithm expects.
func shiftLeft(x, n int) int {
	if n < 0 {
		return 0
	}
	return x << uint(n)
}

func shiftRight(x, n int) int {
	if n < 0 {
		return 0
	}
	return x >> uint(n)
}

// evalCall interprets a builtin invocation. now/rand are effectful and the
// placement calculus guarantees they only run here on the authority; the rest are
// the pure standard library (string/date/math/money), evaluated identically here
// and in assets/facet.js so every executor agrees.
func evalCall(e *ir.Expr, scope map[string]any) any {
	args := make([]any, len(e.Args))
	for i, a := range e.Args {
		args[i] = eval(a, scope)
	}
	return callBuiltin(e.Name, args)
}

// callBuiltin dispatches a builtin over its already-evaluated arguments. Split
// out of evalCall the same way applyBin is split out of evalRest's "bin" case:
// evalInFrame has no flat scope map to hand eval, but a proc body may still call
// a pure builtin (abs/min/max/…), and it must resolve identically either way.
func callBuiltin(name string, argVals []any) any {
	arg := func(i int) any {
		if i < len(argVals) {
			return argVals[i]
		}
		return nil
	}
	switch name {
	case "now":
		return int(time.Now().Unix())
	case "rand":
		n := toInt(arg(0))
		if n <= 0 {
			return 0
		}
		return rand.Intn(n)
	case "abs":
		if n := toInt(arg(0)); n < 0 {
			return -n
		} else {
			return n
		}
	case "min":
		a, b := toInt(arg(0)), toInt(arg(1))
		if a < b {
			return a
		}
		return b
	case "max":
		a, b := toInt(arg(0)), toInt(arg(1))
		if a > b {
			return a
		}
		return b
	case "floor", "round":
		// integers only (no floats in the language), so these are identity.
		return toInt(arg(0))
	case "money":
		return formatMoney(toInt(arg(0)))
	case "len":
		switch v := arg(0).(type) {
		case []any:
			return len(v)
		default:
			return utf8.RuneCountInString(toStr(v))
		}
	case "append":
		// Functional, Go-`append`-flavored, but deliberately never reusing the
		// input's backing array (unlike Go's own append, which may extend it in
		// place when spare capacity exists) — it always allocates a fresh one, so
		// a caller holding the old array (e.g. an alias made before this call)
		// never sees the appended element land in it. Combined with
		// cloneArrayValue (copy-on-assign), no two proc-local array variables
		// ever share a backing array, which is what makes `xs[i] = v` a safe,
		// genuinely-in-place mutation of exactly the one variable it names.
		arr, _ := arg(0).([]any)
		out := make([]any, len(arr)+1)
		copy(out, arr)
		out[len(arr)] = arg(1)
		return out
	case "upper":
		return strings.ToUpper(toStr(arg(0)))
	case "lower":
		return strings.ToLower(toStr(arg(0)))
	case "trim":
		return strings.TrimSpace(toStr(arg(0)))
	case "contains":
		return strings.Contains(toStr(arg(0)), toStr(arg(1)))
	case "ago":
		return ago(toInt(arg(0)), int(clock().Unix()))
	case "compact":
		return compact(toInt(arg(0)))
	case "commas":
		return commas(toInt(arg(0)))
	case "take":
		r := []rune(toStr(arg(0)))
		n := toInt(arg(1))
		if n < 0 {
			n = 0
		}
		if n > len(r) {
			n = len(r)
		}
		return string(r[:n])
	case "year":
		return int(time.Unix(int64(toInt(arg(0))), 0).UTC().Year())
	case "month":
		return int(time.Unix(int64(toInt(arg(0))), 0).UTC().Month())
	case "day":
		return time.Unix(int64(toInt(arg(0))), 0).UTC().Day()
	}
	return nil
}

// formatMoney renders integer minor units (cents) as a fixed two-decimal string,
// the canonical text form of the money type. Mirrors facet.js exactly.
func formatMoney(cents int) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	frac := cents % 100
	s := itoa(cents/100) + "." + string([]byte{byte('0' + frac/10), byte('0' + frac%10)})
	if neg {
		s = "-" + s
	}
	return s
}

func litValue(e *ir.Expr) any {
	switch e.VType {
	case "int":
		return toInt(e.Val)
	case "bool":
		b, _ := e.Val.(bool)
		return b
	default:
		return toStr(e.Val)
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int:
		return t != 0
	case string:
		return t != ""
	// Text from a driver — empty is empty, however it arrived. See toStr.
	case []byte:
		return len(t) != 0
	case []any:
		return len(t) > 0
	case nil:
		return false
	}
	return true
}

// numericText is what both interpreters accept as "a number written as text":
// optional sign, digits with an optional fractional part, optional exponent.
//
// It is deliberately narrower than either language's built-in parser. Go's
// strconv.ParseFloat also accepts "inf", "nan" and "0x1p-2"; JavaScript's Number
// also accepts "0x10", "Infinity" and "" (as 0). Leaning on the built-ins would
// make the two halves of this runtime disagree about the same input, which is
// the one thing eval.go and facet.js must never do — a value would then render
// one way on first paint and another after the client took over.
var numericText = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// parseNumericText reports the value of a number written as text, and whether
// it was one at all. Truncation is toward zero, matching the float64 case below
// and Go's own float→int conversion.
func parseNumericText(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if !numericText.MatchString(t) {
		return 0, false
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return int(n), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case bool:
		if t {
			return 1
		}
	// A number that arrived as text.
	//
	// Every value that crosses a boundary into this runtime is text: a route
	// parameter (`/post/:id`), an HTML form field, a `<input>`'s .value on the
	// client. Returning 0 for all of them was not a conservative default, it was
	// a silent wrong answer — `reply("1", ...)` wrote `tweet: 0` and orphaned the
	// row with `{"ok":true}` and no diagnostic, and every `int`-typed input on
	// the client stored 0 no matter what was typed into it.
	//
	// A string that is not a number still yields 0 here, because this function is
	// total and is called from rendering and comparison paths that have nowhere
	// to report a failure. Rejecting bad input is the job of the boundary that
	// has somewhere to put the error — see `coerceParam` in server.go.
	case string:
		n, _ := parseNumericText(t)
		return n
	// The same number, still text, in the shape a driver hands back — see toStr.
	case []byte:
		n, _ := parseNumericText(string(t))
		return n
	}
	return 0
}

// toStr renders a runtime value as text.
//
// The []byte case is not decoration: it is the shape a database driver hands
// back for a text column. `database/sql` fills a `Scan(&v)` into `any` with the
// driver's own type, and lib/pq gives TEXT/VARCHAR (and NUMERIC, and BYTEA) as
// []byte rather than string — pgStore.CountBy scans a group key exactly that
// way. Without this case every such key converted to "" and every group in a
// hoisted per-row count collapsed onto that one key, so `count(f in Follow
// where f.handle == u.handle)` rendered 0 for every row, with no error to see.
// Only the columns lib/pq happens to convert for us (BIGINT, BOOLEAN) worked.
//
// The typed scan path already knew this — `normalize` in store.go has carried a
// []byte case since it was written — but the total conversions here, which are
// where a value with no declared column type ends up, did not. So toInt and
// truthy read it as text too: bytes-of-text must convert exactly as the string
// they hold, or the three would disagree about the same value.
func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case int:
		return itoa(t)
	case int64:
		return itoa(int(t))
	case float64:
		return itoa(int(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		// mirror JS `"" + array`: elements joined by commas.
		parts := make([]string, len(t))
		for i, el := range t {
			parts[i] = toStr(el)
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	}
	return ""
}

func equal(a, b any) bool {
	// Read a driver's []byte as the string it holds before dispatching, so it
	// takes the text branch below rather than falling through to the numeric one
	// — where two different non-numeric byte strings would both convert to 0 and
	// compare equal. See toStr.
	if ab, ok := a.([]byte); ok {
		a = string(ab)
	}
	if bb, ok := b.([]byte); ok {
		b = string(bb)
	}
	if as, ok := a.(string); ok {
		return as == toStr(b)
	}
	if bs, ok := b.(string); ok {
		return toStr(a) == bs
	}
	if ab, ok := a.(bool); ok {
		return ab == truthy(b)
	}
	return toInt(a) == toInt(b)
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
