package runtime

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"facet/internal/ir"
)

// record is one entity row.
type record = map[string]any

// structVal is a proc-local struct value (`struct Name: field: Type` — see
// LANGUAGE.md's `proc` section and ast.Struct's doc): a real named-field
// composite, constructed by a `Type{field: expr, ...}` literal and read back
// with ordinary `.field` access (evalInFrame's "struct"/"get" cases).
//
// A dedicated Go type rather than a bare map[string]any (which would work
// just as well at the type-assertion level) so it can never be confused, in
// a runtime type switch, with an entity row (this file's own `record` alias
// for map[string]any) or a proc map value (map[any]any, the "map" case
// below) — three genuinely different domains that happen to all be
// string/any-ish, kept apart by using three different concrete Go types.
// Type names which struct shape it is, purely for a clear diagnostic if a
// value ever reaches a place expecting a different one; Fields holds the
// field values by name.
type structVal struct {
	Type   string
	Fields map[string]any
}

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
//
// A Server method (not a free function, unlike eval()) because the "call"
// case below may reach one of the I/O capability builtins (readFile/
// writeFile/httpGet/httpPost — runtime/io.go), which need the server's
// sandboxed data-directory root and HTTP client config; every other proc
// builtin ignores s entirely.
func (s *Server) evalInFrame(e *ir.Expr, fr *frame) (any, error) {
	if e == nil {
		return nil, nil
	}
	switch e.Kind {
	case "lit":
		return litValue(e), nil
	case "list":
		out := make([]any, len(e.Args))
		for i, el := range e.Args {
			v, err := s.evalInFrame(el, fr)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case "map":
		// A map literal (`{k1: v1, ...}`) — see ast.MapLit's doc. Keys and Args
		// (its values) are parallel, exactly as the AST holds them; each key is
		// evaluated and validated (mapKey) exactly as an index read/write's key
		// is, so a bad-typed key is refused here at construction time too, not
		// only when it is later read or overwritten.
		out := make(map[any]any, len(e.Args))
		for i, valExpr := range e.Args {
			kv, err := s.evalInFrame(e.Keys[i], fr)
			if err != nil {
				return nil, err
			}
			key, err := mapKey(kv)
			if err != nil {
				return nil, err
			}
			v, err := s.evalInFrame(valExpr, fr)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	case "struct":
		// A struct literal (`Type{f1: v1, ...}`) — see ast.StructLit's doc.
		// internal/ir/build.go's checkStructFieldTypes already proved every
		// field the struct declares is set exactly once (for the shapes it
		// can see statically), so this just evaluates each value in field
		// order; Fields and Args stay parallel exactly as the IR holds them.
		fields := make(map[string]any, len(e.Args))
		for i, valExpr := range e.Args {
			v, err := s.evalInFrame(valExpr, fr)
			if err != nil {
				return nil, err
			}
			fields[e.Fields[i]] = v
		}
		return structVal{Type: e.Name, Fields: fields}, nil
	case "get":
		// `.field` read off a struct value (ast.Get) — the one composite-value
		// field access a proc has (an entity/record's `.field` never reaches
		// evalInFrame at all; those are eval()'s own "get" case, below).
		// internal/ir/build.go's checkStructFieldTypes already proves this
		// wherever the object's type is statically known; a value that
		// reaches here as something other than a structVal is the runtime
		// backstop for whatever that static check couldn't see through (a
		// proc parameter's dynamic value, say) — a clean error, not a Go
		// panic, the same stance an out-of-bounds array read already takes.
		obj, err := s.evalInFrame(e.Obj, fr)
		if err != nil {
			return nil, err
		}
		sv, ok := obj.(structVal)
		if !ok {
			return nil, fmt.Errorf("cannot read field %q of a value that is not a struct", e.Field)
		}
		return sv.Fields[e.Field], nil
	case "ref":
		v, _ := fr.get(e.Name)
		return v, nil
	case "index":
		obj, err := s.evalInFrame(e.Obj, fr)
		if err != nil {
			return nil, err
		}
		idxV, err := s.evalInFrame(e.Key, fr)
		if err != nil {
			return nil, err
		}
		switch coll := obj.(type) {
		case []any:
			idx := toInt(idxV)
			if idx < 0 || idx >= len(coll) {
				return nil, fmt.Errorf("array index %d out of bounds (length %d)", idx, len(coll))
			}
			return coll[idx], nil
		case map[any]any:
			key, err := mapKey(idxV)
			if err != nil {
				return nil, err
			}
			// A missing key answers nil (Go's own zero value for an absent map
			// entry) rather than a clean error — deliberately the opposite
			// stance from an out-of-bounds array read, above. This mirrors the
			// language's existing precedent for "read of something absent"
			// elsewhere: eval()'s "get" case already returns a missing record
			// field silently (`m[e.Field]` on a Go map), not an error. A hash
			// map's entire reason to exist is answering "is this key here" —
			// unlike an array bound (fixed once the array is built, so any
			// out-of-range index is a programmer bug), a map lookup missing is
			// the ordinary, expected shape of building one up (a frequency
			// counter's `m[k] = m[k] + 1` needs to read an as-yet-absent key
			// without a guard first) — and toInt(nil) already answers 0, so
			// that idiom works for free. See runtime/array_test.go's
			// TestArrayOutOfBoundsIsCleanError for the array side of this
			// same contrast and runtime/map_test.go's missing-key test for
			// this one.
			return coll[key], nil
		default:
			return nil, fmt.Errorf("cannot index a value that is not an array or map")
		}
	case "un":
		x, err := s.evalInFrame(e.X, fr)
		if err != nil {
			return nil, err
		}
		switch e.Op {
		case "!":
			return !truthy(x), nil
		case "-":
			return negate(x), nil
		case "~":
			return ^toInt(x), nil
		}
	case "bin":
		// `&&`/`||` short-circuit: the right operand must not even be
		// evaluated once the left side already determines the result — see
		// evalRest's "bin" case (this function's flat-scope counterpart) for
		// the full reasoning, which applies here unchanged. This matters more
		// here than there: a proc body is the one place an unevaluated right
		// operand can otherwise throw a genuine runtime error (an
		// out-of-bounds "index" read, e.g. `i < len(xs) && xs[i] == y`), not
		// just waste work, so this has to happen before e.R is ever passed to
		// evalInFrame, not inside applyBin (which only ever sees
		// already-evaluated operands).
		if e.Op == "&&" || e.Op == "||" {
			l, err := s.evalInFrame(e.L, fr)
			if err != nil {
				return nil, err
			}
			lt := truthy(l)
			if e.Op == "&&" && !lt {
				return false, nil
			}
			if e.Op == "||" && lt {
				return true, nil
			}
			r, err := s.evalInFrame(e.R, fr)
			if err != nil {
				return nil, err
			}
			return truthy(r), nil
		}
		l, err := s.evalInFrame(e.L, fr)
		if err != nil {
			return nil, err
		}
		r, err := s.evalInFrame(e.R, fr)
		if err != nil {
			return nil, err
		}
		return applyBin(e.Op, l, r), nil
	case "call":
		args := make([]any, len(e.Args))
		for i, a := range e.Args {
			v, err := s.evalInFrame(a, fr)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		return s.callProcBuiltin(e.Name, args)
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

// cloneMapValue is cloneArrayValue's counterpart for a map value: returns v
// unchanged unless it is a map[any]any (a map value, runtime/eval.go's "map"
// case), in which case it returns a fresh copy with its own backing map.
//
// This gives a proc-local map the same copy-on-assign value semantics an
// array already has (see cloneArrayValue's doc, above, for the full
// reasoning — it applies here unchanged): called at every point a value
// flows into a new binding, so `let m2 = m1` never leaves m1 and m2 sharing
// one backing map, and mutating one afterward (via an index-write,
// runtime/server.go's "indexset") can never be observed through the other.
func cloneMapValue(v any) any {
	m, ok := v.(map[any]any)
	if !ok {
		return v
	}
	out := make(map[any]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	return out
}

// mapKey validates and normalizes a map-index/-literal key to this
// milestone's two allowed key types — int and text, the two scalar types
// with obvious, unambiguous equality/hashing (see ast.MapLit's doc). It is
// the runtime backstop for whatever internal/ir/build.go's checkMapKeyTypes
// could not decide at compile time (a proc parameter, a `do`-bound result,
// or any other key whose type isn't statically provable) — the same
// division of labor checkIndexTypes/evalInFrame's "index" case already have
// for an array's bounds. A key of any other type (bool, money, date, an
// array, or a map) is a clean error here, not a Go panic and not a silently
// wrong answer — this codebase's established convention (see
// runtime/array_test.go's TestArrayOutOfBoundsIsCleanError).

// cloneCompositeValue applies cloneArrayValue, cloneMapValue, then
// cloneStructValue, so a value flowing into a new proc-local binding (a
// `let`, a plain reassignment, or a parameter — see each clone helper's own
// doc for why that copy matters) is copied whichever of the three
// proc-local composite value kinds it happens to be. Composing them is safe
// and total: each helper touches only its own kind and passes everything
// else through unchanged, so a scalar passes through all three unchanged,
// an array is copied by the first and passed through the other two, a map
// the second, and a struct the third.
func cloneCompositeValue(v any) any {
	return cloneStructValue(cloneMapValue(cloneArrayValue(v)))
}

// cloneStructValue is cloneArrayValue's/cloneMapValue's counterpart for a
// struct value (see structVal): returns v unchanged unless it is a
// structVal, in which case it returns a copy with its own backing Fields
// map, so a struct gets the same copy-on-assign value semantics an array or
// map already has (see cloneArrayValue's doc for the full reasoning — it
// applies here unchanged). One level deep, exactly like the other two: a
// field that itself holds an array/map/struct is carried over by reference
// at this level, the same way an array of arrays or a map of maps already
// is — there is no in-place field mutation this milestone (a struct's
// fields are set once, by its literal), so this one-level copy is already
// enough to make two bindings of the same struct value fully independent.
func cloneStructValue(v any) any {
	sv, ok := v.(structVal)
	if !ok {
		return v
	}
	out := make(map[string]any, len(sv.Fields))
	for k, val := range sv.Fields {
		out[k] = val
	}
	return structVal{Type: sv.Type, Fields: out}
}

func mapKey(v any) (any, error) {
	switch t := v.(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case string:
		return t, nil
	case []byte:
		// A text value from a database driver — see toStr's doc for why this
		// shape reaches the runtime at all; normalized to string so the same
		// text key always hashes and compares equal no matter which shape it
		// arrived in.
		return string(t), nil
	default:
		return nil, fmt.Errorf("map key must be int or text, got %T", v)
	}
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
			return negate(x)
		case "~":
			return ^toInt(x)
		}
	case "bin":
		// `&&`/`||` short-circuit: evaluate the left operand first, and only
		// evaluate the right operand if the left side doesn't already
		// determine the result (`false && ...` -> false, `true || ...` ->
		// true, neither ever touching the right side). This has to live
		// here, at the call site that decides whether e.R gets evaluated at
		// all, rather than inside applyBin — by the time applyBin runs, both
		// operands have already been evaluated, which is exactly the bug
		// this fixes (see evalInFrame's "bin" case, this function's
		// proc-body counterpart, for the motivating out-of-bounds case that
		// makes this more than a performance nicety there). eval() itself
		// has no "index" case today (only a proc body can index an array —
		// see evalInFrame), so no expression eval() evaluates can fail the
		// way an unguarded `xs[i]` can; this still keeps eval() and
		// evalInFrame's short-circuit semantics identical, per this file's
		// existing convention that the two interpreters never disagree
		// about what an operator means, and protects any future eval() case
		// that can fail or have a side effect on the right of `&&`/`||`.
		if e.Op == "&&" || e.Op == "||" {
			l := truthy(eval(e.L, scope))
			if e.Op == "&&" && !l {
				return false
			}
			if e.Op == "||" && l {
				return true
			}
			return truthy(eval(e.R, scope))
		}
		return applyBin(e.Op, eval(e.L, scope), eval(e.R, scope))
	}
	return nil
}

// textOperands reports whether `<`/`<=`/`>`/`>=` should compare l and r as
// text, and if so, their string values.
//
// Before this existed, every ordering comparison went straight through
// toInt/toFloat regardless of operand type — so two text values ("apple" <
// "banana") were silently parsed as numbers (both failing to parse as one,
// both becoming 0) and compared as 0 < 0, always false. That is the same
// class of silent-wrong-answer toInt("sold out") == 0 already warns about at
// equal's definition above, just reached from `<` instead of `==`. Real text
// deserves real lexicographic (codepoint) ordering — the same ordering Go's
// own `<` on strings already gives, and the same ordering Python/Swift give
// their own string types — not a coercion through a numeric parse that
// throws the actual characters away.
//
// A []byte is normalized to string first, matching equal's own treatment of
// the shape a database driver hands back for a text column (see toStr's
// doc) — otherwise a text column's value would take this string path when it
// arrived as a Go string literal but silently fall through to the numeric
// path when it arrived from a driver scan, and the two would disagree about
// ordering the exact same column.
//
// Only fires when at least one side is genuinely text; a comparison that
// mixes text with a non-text, non-numeric value (impossible from a
// well-typed proc — checkNumericTypes and its surrounding static checks see
// to that — but not something this defensive runtime path assumes) still
// compares as text via toStr, the same total, never-guess stance equal
// already takes for a mismatched pair.
func textOperands(l, r any) (string, string, bool) {
	if lb, ok := l.([]byte); ok {
		l = string(lb)
	}
	if rb, ok := r.([]byte); ok {
		r = string(rb)
	}
	ls, lok := l.(string)
	rs, rok := r.(string)
	if !lok && !rok {
		return "", "", false
	}
	if !lok {
		ls = toStr(l)
	}
	if !rok {
		rs = toStr(r)
	}
	return ls, rs, true
}

// applyBin evaluates one binary operator over its already-evaluated operands.
// Split out of evalRest's "bin" case so evalInFrame (a proc body's expression
// evaluator, which has no flat scope map to hand eval) can share the exact same
// operator semantics rather than reimplementing them — eval() and evalInFrame()
// must never disagree about what `+`/`==`/etc. mean.
func applyBin(op string, l, r any) any {
	switch op {
	case "&&":
		// Neither of applyBin's two callers (evalRest's and evalInFrame's
		// "bin" cases, above) ever reaches this arm for "&&"/"||" — both
		// intercept these two ops before calling applyBin at all, so they
		// can short-circuit and skip evaluating e.R entirely (see their own
		// "bin" cases for why that has to happen before evaluation, not
		// here after both operands already exist). Kept as a correct,
		// non-short-circuiting fallback rather than removed, in case a
		// future caller ever hands applyBin two already-evaluated operands
		// directly — it must still answer the right value for those two
		// operators, just without the short-circuit guarantee that requires
		// deciding whether to evaluate r before r exists.
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
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) + toFloat(r)
		}
		return toInt(l) + toInt(r)
	case "-":
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) - toFloat(r)
		}
		return toInt(l) - toInt(r)
	case "*":
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) * toFloat(r)
		}
		return toInt(l) * toInt(r)
	case "/":
		// Float division follows IEEE 754 exactly as Go's own `/` does — a
		// finite/0 divide is +/-Inf and 0/0 is NaN, not the int-only "0"
		// sentinel a few lines below. This is a deliberate divergence from
		// int's zero-guard, not an oversight: int division by zero would
		// otherwise panic (Go's own runtime behavior for integer division),
		// which this total runtime has always caught with the 0 sentinel, but
		// float division by zero already can't panic — Go defines it, and a
		// real numeric algorithm (this milestone's whole reason for existing)
		// needs to see the same Inf/NaN Go's own math package would produce
		// for the identical expression, not a silently-substituted 0. See
		// runtime/float_test.go's cross-check against Go's own math for why
		// this has to match exactly.
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) / toFloat(r)
		}
		if toInt(r) == 0 {
			return 0
		}
		return toInt(l) / toInt(r)
	case "%":
		// int-only (see checkNumericTypes, which refuses a float operand here
		// at compile time in a proc body) — this int-modulo fallback is only
		// ever reached for two genuine ints; kept exactly as it was.
		if toInt(r) == 0 {
			return 0
		}
		return toInt(l) % toInt(r)
	case "==":
		return equal(l, r)
	case "!=":
		return !equal(l, r)
	case "<":
		if ls, rs, ok := textOperands(l, r); ok {
			return ls < rs
		}
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) < toFloat(r)
		}
		return toInt(l) < toInt(r)
	case "<=":
		if ls, rs, ok := textOperands(l, r); ok {
			return ls <= rs
		}
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) <= toFloat(r)
		}
		return toInt(l) <= toInt(r)
	case ">":
		if ls, rs, ok := textOperands(l, r); ok {
			return ls > rs
		}
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) > toFloat(r)
		}
		return toInt(l) > toInt(r)
	case ">=":
		if ls, rs, ok := textOperands(l, r); ok {
			return ls >= rs
		}
		if isFloatVal(l) || isFloatVal(r) {
			return toFloat(l) >= toFloat(r)
		}
		return toInt(l) >= toInt(r)
	case "in":
		// Extends the existing array-membership operator to map key presence
		// (`k in m`) rather than adding a separate `has` builtin for one more
		// container kind — Python draws the same equivalence (`x in list` /
		// `k in dict`), and this language already has the array precedent to
		// match. A key of a type a map cannot hold (bool/money/date/an array/
		// another map) can never be present, so this reports false for it
		// rather than the clean error a get/set raises for the same bad key
		// (runtime/eval.go's mapKey) — `in` is a pure predicate with an
		// always-safe answer here, unlike a read or write, which the
		// key-type restriction is really about.
		if m, ok := r.(map[any]any); ok {
			key, err := mapKey(l)
			if err != nil {
				return false
			}
			_, ok := m[key]
			return ok
		}
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

// callProcBuiltin dispatches a proc-body builtin call, exactly like callBuiltin
// below, except that it also recognizes the I/O capability builtins
// (readFile/writeFile/httpGet/httpPost — see runtime/io.go — and
// listen/accept/readBytes/writeBytes/closeConn — see runtime/netconn.go),
// which callBuiltin itself cannot: they need the server's sandboxed
// data-directory root, HTTP client config, or listener/connection registry,
// and — unlike every builtin callBuiltin handles — they can fail for reasons
// outside the program's control (missing file, permission, unreachable host,
// non-2xx response, connection reset), so this returns an error where
// callBuiltin never does. Only evalInFrame's "call" case reaches this;
// callBuiltin itself stays the single dispatch table for eval()/evalCall's
// flat-scope (action/view) path, which can never be asked to run one of
// these (internal/ir/build.go's checkNoIO bars them from ever reaching
// action/view/policy/derive source in the first place).
func (s *Server) callProcBuiltin(name string, argVals []any) (any, error) {
	arg := func(i int) any {
		if i < len(argVals) {
			return argVals[i]
		}
		return nil
	}
	switch name {
	case "readFile":
		return s.ioReadFile(toStr(arg(0)))
	case "writeFile":
		return s.ioWriteFile(toStr(arg(0)), toStr(arg(1)))
	case "httpGet":
		return s.ioHTTPGet(toStr(arg(0)))
	case "httpPost":
		return s.ioHTTPPost(toStr(arg(0)), toStr(arg(1)))
	case "channel":
		return s.channels.create(), nil
	case "send":
		return s.channels.send(toInt(arg(0)), toStr(arg(1)))
	case "recv":
		return s.channels.recv(toInt(arg(0)))
	case "listen":
		return s.ioListen(toInt(arg(0)))
	case "accept":
		return s.ioAccept(toInt(arg(0)))
	case "readBytes":
		return s.ioReadBytes(toInt(arg(0)), toInt(arg(1)))
	case "writeBytes":
		return s.ioWriteBytes(toInt(arg(0)), arg(1))
	case "closeConn":
		return s.ioCloseConn(toInt(arg(0)))
	}
	return callBuiltin(name, argVals), nil
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
		// Preserves whichever numeric flavour it is handed — abs(-2.5) is a
		// float, abs(-2) is an int — matching internal/ir/build.go's
		// inferProcType (which types abs() the same way for the compiler).
		if isFloatVal(arg(0)) {
			return math.Abs(toFloat(arg(0)))
		}
		if n := toInt(arg(0)); n < 0 {
			return -n
		} else {
			return n
		}
	case "min":
		// Compile time (internal/ir/build.go's checkNumericTypes) already
		// refuses a proc calling min/max with one int and one float argument,
		// so by the time this runs both agree — this only has to decide
		// WHICH arithmetic to run, not reconcile a mismatch.
		if isFloatVal(arg(0)) || isFloatVal(arg(1)) {
			a, b := toFloat(arg(0)), toFloat(arg(1))
			if a < b {
				return a
			}
			return b
		}
		a, b := toInt(arg(0)), toInt(arg(1))
		if a < b {
			return a
		}
		return b
	case "max":
		if isFloatVal(arg(0)) || isFloatVal(arg(1)) {
			a, b := toFloat(arg(0)), toFloat(arg(1))
			if a > b {
				return a
			}
			return b
		}
		a, b := toInt(arg(0)), toInt(arg(1))
		if a > b {
			return a
		}
		return b
	case "floor":
		// int input is unchanged (identity — this builtin's entire pre-float
		// behavior, preserved exactly for backward compatibility: no existing
		// .fct source could ever have passed floor() anything but an int
		// before this milestone). A float input rounds toward negative
		// infinity and the fractional part is genuinely gone, so the result
		// is int-typed, not a float with a zero fraction — matching
		// internal/ir/build.go's inferProcType ("floor/round always return
		// int") and this milestone's stated design choice: a rounded value's
		// declared type follows what it now IS (a whole number), the same
		// way this language already lets a computed value's type follow its
		// computation elsewhere (internal/ir/types.go's arith).
		if isFloatVal(arg(0)) {
			return int(math.Floor(toFloat(arg(0))))
		}
		return toInt(arg(0))
	case "round":
		// Round-half-away-from-zero (Go's math.Round: round(2.5) == 3,
		// round(-2.5) == -3) — not round-half-to-even/banker's rounding.
		// Chosen because it is Go's own math.Round semantics (this runtime's
		// float arithmetic is defined to match Go's exactly — see applyBin's
		// "/" case — so round() matching it too means a program can predict
		// this builtin's answer from Go's documentation alone) and it is the
		// convention most people mean by "round" absent a specific reason to
		// want banker's rounding. See runtime/float_test.go for the explicit
		// round(2.5)==3 / round(-2.5)==-3 proof. Same identity-for-int,
		// int-typed-result-for-float shape as floor, above.
		if isFloatVal(arg(0)) {
			return int(math.Round(toFloat(arg(0))))
		}
		return toInt(arg(0))
	case "toFloat":
		return toFloat(arg(0))
	case "toInt":
		return toInt(arg(0))
	case "floatBits":
		// The raw IEEE-754 bit pattern of a float, reinterpreted as a signed
		// 64-bit int — a bit-cast, not a numeric conversion (contrast
		// toInt, which truncates the VALUE). This is exactly Go's own
		// math.Float64bits, so a program can predict this builtin's answer
		// from Go's documentation alone, the same stance round() already
		// takes on math.Round (see its doc above). int is this runtime's
		// only integer width (see toInt), so the uint64 result is
		// reinterpreted as int64 and handed back as int with no value
		// change — the top bit (the float's sign bit) survives as int's
		// sign bit, which is exactly what an order-preserving encoder
		// (this builtin's motivating caller — see selfhost/index_text.fct)
		// needs to detect and flip.
		return int(int64(math.Float64bits(toFloat(arg(0)))))
	case "floatFromBits":
		// floatBits' exact inverse: reinterpret an int's bit pattern as
		// IEEE-754 and return the float it spells, via Go's own
		// math.Float64frombits — see floatBits' doc above.
		return math.Float64frombits(uint64(toInt(arg(0))))
	case "toMoney":
		n, _ := parseMoneyText(toStr(arg(0)))
		return n
	case "print":
		// A developer debugging aid, not I/O to an external resource (no `uses`
		// capability, unlike readFile/writeFile/httpGet/httpPost — see printCap's
		// doc in internal/ir/build.go): writes one clearly-tagged line straight to
		// the server process's own stdout, visible in `facet serve`'s console
		// alongside (but never mixed into) the JSON http_request logs
		// runtime/observability.go writes to stderr. Returns its argument
		// unchanged (Rust's dbg!() shape) so `let y = print(x)` types and
		// evaluates identically to `let y = x` — see internal/ir/build.go's
		// inferProcType "print" case.
		fmt.Fprintln(os.Stdout, "[print] "+formatDebugValue(arg(0)))
		return arg(0)
	case "money":
		return formatMoney(toInt(arg(0)))
	case "len":
		switch v := arg(0).(type) {
		case []any:
			return len(v)
		case map[any]any:
			return len(v)
		default:
			return utf8.RuneCountInString(toStr(v))
		}
	case "byteLen":
		// byteLen(s) -> int: s's real UTF-8 byte length, as opposed to len's
		// rune count above — a plain Go len() on the string's own byte
		// representation, with no decoding at all.
		return len(toStr(arg(0)))
	case "textToBytes":
		// textToBytes(s) -> [int]: s's real UTF-8 byte encoding, one int
		// (0-255) per byte — a multi-byte codepoint produces its real
		// multi-byte encoding, not one int per rune (contrast charAt/slice/
		// len, which are rune-indexed). Represented as the same []any a
		// bytes(n)/readBytes byte buffer already is (see bytesType's doc in
		// internal/ir/build.go), so it is len()-able, index-readable, and
		// accepted wherever a byte buffer already is (e.g. writeBytes).
		buf := []byte(toStr(arg(0)))
		out := make([]any, len(buf))
		for i, b := range buf {
			out[i] = int(b)
		}
		return out
	case "bytesToText":
		// bytesToText(b) -> text: textToBytes' inverse, decoding a [int]
		// byte buffer as UTF-8. Invalid input (a byte value out of range, or
		// a byte sequence that isn't valid UTF-8) is handled the same
		// lenient way Go's own string([]byte) conversion already handles it
		// everywhere else in this runtime (e.g. ioReadFile's os.ReadFile
		// bytes, or readBytes' own buf above turned into text by a caller):
		// bytes are copied through verbatim, never an error, and any
		// ill-formed sequence only surfaces as U+FFFD replacement runes the
		// next time something decodes the string rune-by-rune (len, charAt,
		// slice, split, range) — exactly the same place Go's own decoder
		// would show it, and the only "invalid UTF-8" behavior available
		// without inventing an error-return shape none of this runtime's
		// other pure builtins have. A byte value outside 0-255 is clamped
		// into range first, the same defensive clamp writeBytes already
		// applies to its own []int argument.
		arr, _ := arg(0).([]any)
		buf := make([]byte, len(arr))
		for i, v := range arr {
			n := toInt(v)
			if n < 0 {
				n = 0
			} else if n > 255 {
				n = 255
			}
			buf[i] = byte(n)
		}
		return string(buf)
	case "bytes":
		// bytes(n): an n-length, zero-filled byte buffer — the common way to start
		// building one up byte-by-byte in a loop (mirroring `[]` + append for a
		// plain array, but pre-sized since a real codec/hash loop indexes by
		// position rather than growing one element at a time). Represented as
		// exactly the same []any a plain array literal produces (see arrayType/
		// bytesType in internal/ir/build.go): a byte buffer is a specialization
		// of the array value, not a parallel runtime kind, so it is len()-able,
		// index-readable, and copy-on-assign (cloneArrayValue) for free. What
		// makes it a byte buffer rather than a plain array is purely the 0-255
		// range check on every index-write (runtime/server.go's execProcBlock,
		// "indexset" case, gated on the IR's Stmt.Bytes flag) — a compile-time
		// distinction the elements themselves carry no runtime tag for.
		n := toInt(arg(0))
		if n < 0 {
			n = 0
		}
		out := make([]any, n)
		for i := range out {
			out[i] = 0
		}
		return out
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
	case "split":
		// split(s, sep) -> [text], matching Go's strings.Split exactly,
		// including its edge cases (empty sep splits after every UTF-8
		// sequence; a sep not present in s returns a single-element slice
		// holding s unchanged; adjacent separators produce empty-string
		// elements; a leading/trailing separator produces a leading/trailing
		// empty element). strings.Split operates on bytes, but that is
		// rune-safe here for the same reason strings.Contains/Index already
		// are: UTF-8 is self-synchronizing, so a byte-for-byte search for a
		// valid UTF-8 separator can only ever match at rune boundaries — it
		// never lands inside another rune's encoding. The result is returned
		// as the same []any representation every other array value uses (see
		// bytesType's doc), so it is len()-able and index-readable for free.
		parts := strings.Split(toStr(arg(0)), toStr(arg(1)))
		out := make([]any, len(parts))
		for i, s := range parts {
			out[i] = s
		}
		return out
	case "slice":
		// slice(s, start, end) -> text, a general (not prefix-only) substring,
		// rune-indexed to match take/len's own rune-based indexing (see their
		// cases above/below) so a multi-byte character is never cut in half.
		// Out-of-range start/end never error — both are clamped into
		// [0, len(r)], and a start left past end after clamping yields "" —
		// the exact same "clamp, never error" convention take already
		// established for an n longer than the string.
		r := []rune(toStr(arg(0)))
		start, end := toInt(arg(1)), toInt(arg(2))
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
	case "charAt":
		// charAt(s, i) -> text, a length-1 string (this language has no
		// separate character/rune type) — defined as exactly slice(s, i,
		// i+1), so it inherits the same clamp-not-error behavior: an
		// out-of-range i (negative or >= len) yields "" rather than a runtime
		// error.
		r := []rune(toStr(arg(0)))
		i := toInt(arg(1))
		if i < 0 || i >= len(r) {
			return ""
		}
		return string(r[i])
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

// formatDebugValue renders any runtime value print() can be handed as a
// readable one-line debug form: this runtime's own toStr for the flat scalar
// kinds already reachable from an action (int/text/bool/date/money — the
// latter two are plain ints at this layer, same as toStr already treats
// them, matching the fact that neither auto-formats in a `text "{...}"`
// interpolation either without an explicit money()/ago() call), plus the
// composite kinds a proc value can additionally be (array, proc map, and a
// proc-local struct) that toStr was never asked to handle (its one []any
// case flattens comma-joined, mirroring JS `"" + array`, which reads as one
// value rather than the array print(...) should show it as). Deterministic
// key order (sorted) so a printed map/struct's fields never shuffle between
// two runs of the same program — a debugging aid whose whole point is a
// stable line to compare across requests.
func formatDebugValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return strconv.Quote(t)
	case []byte:
		return strconv.Quote(string(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, el := range t {
			parts[i] = formatDebugValue(el)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		return formatDebugMap(t)
	case map[any]any:
		// A proc's own `map` type (runtime/eval.go's "map" case) — keys can be
		// any scalar, unlike an entity row's always-string keys, so they are
		// rendered through formatDebugValue too rather than assumed to be text.
		keyed := make(map[string]any, len(t))
		for k, val := range t {
			keyed[formatDebugValue(k)] = val
		}
		return formatDebugMap(keyed)
	case structVal:
		keys := make([]string, 0, len(t.Fields))
		for k := range t.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + formatDebugValue(t.Fields[k])
		}
		return t.Type + "{" + strings.Join(parts, ", ") + "}"
	default:
		// int/int64 and anything else this runtime hands print() go through
		// toStr, which already has their exact rendering (itoa, etc.) — no
		// reason for a second, possibly-drifting int formatter here.
		return toStr(v)
	}
}

// formatDebugMap renders a string-keyed map's entries sorted by key, shared
// by formatDebugValue's map[string]any and map[any]any cases (the latter
// keys are turned into their own formatDebugValue text first, then handed
// here exactly like a real string key — see its call site).
func formatDebugMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + formatDebugValue(m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func litValue(e *ir.Expr) any {
	switch e.VType {
	case "int":
		return toInt(e.Val)
	case "float":
		// A float literal's Val is already a Go float64 — the parser
		// (internal/parser/expr.go's parseAtom) produces it via
		// strconv.ParseFloat, and internal/ir/build.go's lower() copies it
		// through unchanged — so this is a plain type assertion, not a
		// conversion: there is no "numeric text" fallback the way toInt has
		// one, because a float literal can only ever reach here already typed
		// (checkNoFloat bars a float literal everywhere but a proc body,
		// where every literal is compiler-generated, never boundary input).
		f, _ := e.Val.(float64)
		return f
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
	case float64:
		return t != 0
	case string:
		return t != ""
	// Text from a driver — empty is empty, however it arrived. See toStr.
	case []byte:
		return len(t) != 0
	case []any:
		return len(t) > 0
	case map[any]any:
		return len(t) > 0
	case nil:
		return false
	}
	return true
}

// isFloatVal reports whether v is a genuine runtime float value (a Go
// float64) — applyBin's switch on this, rather than always converting both
// operands with toInt, is what makes `+ - * / < <= > >=` compute the correct
// float64 result for a float operand instead of silently truncating it
// through toInt first. Compile time (checkNumericTypes) already refuses an
// int/float mix in a proc body, so in practice both operands agree by the
// time this runs; this is what lets the one operand that IS a float decide
// the arithmetic even in a defensive/unchecked path.
func isFloatVal(v any) bool { _, ok := v.(float64); return ok }

// negate implements unary `-`: float-preserving for a float64 operand (so
// `-3.14` is a float, not toInt(3.14) truncated to 0 and then negated to 0),
// int otherwise — shared by evalInFrame's and evalRest's "un" cases so they
// cannot disagree, the same reason applyBin is factored out for "bin".
func negate(x any) any {
	if isFloatVal(x) {
		return -toFloat(x)
	}
	return -toInt(x)
}

// toFloat is toInt's float64 counterpart: a total conversion from whatever
// shape a numeric value might arrive in (a proc's own float64, a Go int this
// runtime never mixes with a float without an explicit toFloat()/toInt() —
// see checkNumericTypes — but which a defensive runtime path may still hand
// this, a number written as text, or the driver's []byte shape of one) to a
// float64 — 0 for anything not interpretable as a number, the same "total,
// never panics" stance toInt already takes.
func toFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case []byte:
		f, _ := strconv.ParseFloat(strings.TrimSpace(string(t)), 64)
		return f
	}
	return 0
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

// parseMoneyText reports the value of a money amount written as decimal text
// (dollars-and-cents, e.g. "12.34", "-5", "0.5"), in minor units (cents), and
// whether it was one at all — toMoney's total parse, and the exact inverse of
// formatMoney. It accepts the same numericText shape parseNumericText does
// (so "$12.34" is not money text any more than it is int text — a caller
// strips a currency symbol itself), then rounds to the nearest cent rather
// than truncating, matching round()'s own round-half-away-from-zero
// convention (see LANGUAGE.md's `float` section) rather than silently
// dropping a third decimal.
func parseMoneyText(s string) (int, bool) {
	t := strings.TrimSpace(s)
	if !numericText.MatchString(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return int(math.Round(f * 100)), true
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
		// A `float64` reaching this switch is one of two unrelated things
		// that share a Go type but not a meaning: a wire-decoded int/date/
		// money value (JSON has no separate int type, so these always
		// arrive as float64, and always hold a whole number — the ORIGINAL
		// reason this case truncated via itoa(int(t))), or a genuine proc
		// `float` (LANGUAGE.md's `float` section) that can legitimately
		// hold a fractional value a caller needs rendered exactly, not
		// silently floored. The two are indistinguishable by Go type alone,
		// so the split here is by VALUE instead: whole-number floats keep
		// the exact old int-rendering path (byte-for-byte unchanged for
		// every existing int/date/money caller), and only a value with a
		// real fractional part — which no int/date/money field ever has —
		// falls through to a real decimal rendering, the same
		// `strconv.FormatFloat(t, 'g', -1, 64)` formatDebugValue already
		// uses for exactly this type one function up.
		//
		// The magnitude bounds guard a real bug this file used to have: a
		// wire int/date/money value is never outside int64 range, but a
		// genuine proc `float` can be (e.g. a `sum` accumulator over
		// values near i64::MAX) — `int(t)` for a float64 that doesn't fit
		// int64 is an out-of-range conversion Go leaves
		// implementation-defined, and on this platform it silently
		// produces `math.MinInt64`, which `itoa`'s own `n = -n` then
		// cannot negate back (`-math.MinInt64` overflows right back to
		// `math.MinInt64`), rendering a bare `"-"` with no digits at all
		// — found via aggregate.fct's own port of a real Rust test
		// (`an_integer_sum_wider_than_i64_is_still_right`) exercising
		// exactly this case. `9223372036854775808.0` (2^63) is exactly
		// representable in float64, so this comparison is exact, not an
		// approximation — the fast int-rendering path is skipped only for
		// the (astronomically rare in practice) values it was never
		// actually safe for.
		if !math.IsNaN(t) && !math.IsInf(t, 0) && t == math.Trunc(t) &&
			t >= -9223372036854775808.0 && t < 9223372036854775808.0 {
			return itoa(int(t))
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
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
	// A float on either side must compare as a float, not through toInt —
	// toInt(2.5) truncates to 2, which would make 2.5 == 2 report true, the
	// exact kind of silent lie this runtime's total conversions otherwise
	// refuse (see internal/ir/types.go's doc on what toInt("sold out") == 0
	// already means for text). Compile time already refuses `2.5 == 2` in a
	// proc body (checkNumericTypes), so this branch is a defensive backstop —
	// reached only through a path static checking couldn't see through — not
	// the primary guard.
	if _, ok := a.(float64); ok {
		return toFloat(a) == toFloat(b)
	}
	if _, ok := b.(float64); ok {
		return toFloat(a) == toFloat(b)
	}
	return toInt(a) == toInt(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	// A plain `n = -n` overflows right back to n itself for exactly one
	// value, math.MinInt64 — its magnitude has no positive int64
	// representation — which used to leave the digit loop below never
	// running (n stayed negative) and this function returning a bare
	// "-". Working in uint64 sidesteps that: `-(n+1)` is always safely
	// representable as an int64 (n+1 undoes MinInt64's own one-past-the-end
	// asymmetry), and adding the 1 back after widening to uint64 lands on
	// the exact right magnitude for every negative n, MinInt64 included.
	var mag uint64
	if neg {
		mag = uint64(-(n + 1)) + 1
	} else {
		mag = uint64(n)
	}
	var buf [20]byte
	i := len(buf)
	for mag > 0 {
		i--
		buf[i] = byte('0' + mag%10)
		mag /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
