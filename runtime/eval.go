package runtime

import (
	"math/rand"
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
	case "eget":
		rows, _ := scope[e.Name].([]any)
		key := eval(e.Key, scope)
		for _, r := range rows {
			if m, ok := r.(record); ok && equal(m["id"], key) {
				return m[e.Field]
			}
		}
		return nil
	case "agg":
		rows, _ := scope[e.Name].([]any)
		// Filtered form: keep only rows the predicate accepts, with the item
		// variable bound to each row. Mutate-and-restore keeps eval allocation-free.
		if e.Where != nil {
			prev, had := scope[e.Var]
			kept := make([]any, 0, len(rows))
			for _, r := range rows {
				if m, ok := r.(record); ok {
					scope[e.Var] = m
					if truthy(eval(e.Where, scope)) {
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
		total := 0 // sum
		for _, r := range rows {
			if m, ok := r.(record); ok {
				total += toInt(m[e.Field])
			}
		}
		return total
	case "astate":
		// Action status is client-only runtime state; the server has none at render
		// time, so first paint shows "not pending" / "no error".
		if e.Op == "pending" {
			return false
		}
		return ""
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
	case []any:
		return len(t) > 0
	case nil:
		return false
	}
	return true
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
	}
	return 0
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
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
