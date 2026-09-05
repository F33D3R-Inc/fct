package runtime

import (
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
		// A count/exists the database can answer is asked of the database: that is
		// what stops the in-memory mirror from being the only thing that can answer
		// it. Anything else — a sum, an unpushable predicate, an aggregate a list
		// could not batch — is counted here, over the working set, as before.
		if v, ok := m.resolveAgg(e, scope); ok {
			return m.record(e, v)
		}
		return m.record(e, evalColl(e, scope))
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
				return m[e.Field]
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
		}
	case "bin":
		l := eval(e.L, scope)
		r := eval(e.R, scope)
		switch e.Op {
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
		}
	}
	return nil
}

// evalCall interprets a builtin invocation. now/rand are effectful and the
// placement calculus guarantees they only run here on the authority; the rest are
// the pure standard library (string/date/math/money), evaluated identically here
// and in assets/facet.js so every executor agrees.
func evalCall(e *ir.Expr, scope map[string]any) any {
	arg := func(i int) any {
		if i < len(e.Args) {
			return eval(e.Args[i], scope)
		}
		return nil
	}
	switch e.Name {
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
	case "upper":
		return strings.ToUpper(toStr(arg(0)))
	case "lower":
		return strings.ToLower(toStr(arg(0)))
	case "trim":
		return strings.TrimSpace(toStr(arg(0)))
	case "contains":
		return strings.Contains(toStr(arg(0)), toStr(arg(1)))
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
