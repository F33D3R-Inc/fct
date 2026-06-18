package runtime

import (
	"math/rand"
	"time"

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
		if e.Op == "count" {
			return len(rows)
		}
		total := 0 // sum
		for _, r := range rows {
			if m, ok := r.(record); ok {
				total += toInt(m[e.Field])
			}
		}
		return total
	case "call":
		// Effectful builtins. The placement calculus guarantees these only ever
		// run here, on the authority — never on a client — so all clients agree.
		switch e.Name {
		case "now":
			return int(time.Now().Unix())
		case "rand":
			n := 0
			if len(e.Args) > 0 {
				n = toInt(eval(e.Args[0], scope))
			}
			if n <= 0 {
				return 0
			}
			return rand.Intn(n)
		}
		return nil
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
	case float64:
		return itoa(int(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
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
