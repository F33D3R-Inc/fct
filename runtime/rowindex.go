package runtime

import (
	"sort"
	"sync/atomic"

	"facet/internal/ir"
)

// Row indexes: how an aggregate or lookup whose predicate pins a field to a
// value reads the rows that can match instead of every row.
//
// `count(q in Work where q.quoted == w.id)` inside a per-result DTO is asked
// once for every result, and each asking walked the whole working set — 20
// results over 10 000 works was 200 000 predicate evaluations for one count.
// A predicate with an equality conjunct `x.f == v` (v not reading x) can only
// be true of a row whose f equals v, so the rows worth testing are the ones
// an index of the rows by f names for v. The index is a candidate generator:
// the whole predicate is still evaluated on every candidate, by the same
// evalRowPredicate the scan uses, so an answer is the scan's by construction
// — the index changes how many rows are read, never which ones match.
//
// Scope and lifetime. An index belongs to one evaluation scope — one request,
// which runs under the server lock — and to the exact rows slice it was built
// from: a slice the scope no longer holds (an `add` appended, a `remove`
// rebuilt it) is rebuilt on the next use. What a slice identity cannot see is
// a row edited in place, so every action statement that can write rows drops
// the scope's indexes before it runs (dropRowIndexes, from execActionBlock).
// Nothing outlives the request: no cross-request invalidation to get wrong.

// rowIndexKey is the scope slot the indexes live in; like every `@` key it is
// never shipped to a client.
const rowIndexKey = "@ix"

// rowIndexing is off only in a test that checks the answers are the scan's.
var rowIndexing = true

// rowIndexHits counts the reads an index answered (tests read it to know the
// index was used, not merely harmless).
var rowIndexHits atomic.Int64

type rowIndexSet map[string]*rowIndex

// rowIndex indexes one entity's rows by one field, in two views built as
// needed: by integer value (the probe an int or nil — equal's final branch)
// and by text (a text probe — equal compares text through toStr either way
// round). Rows whose value equal might convert into a match some other way
// are "loose": always candidates.
type rowIndex struct {
	first *any // &rows[0] when built: the slice identity, with n
	n     int
	rows  []any
	field string

	ints      map[int][]int // int-like value -> row positions, ascending
	intsLoose []int         // positions whose value is not int-like

	texts      map[string][]int // toStr(value) -> row positions, ascending
	textsLoose []int            // positions whose value toStr cannot stand for (byte buffers)
}

// dropRowIndexes forgets every row index a scope holds — before a statement
// that can edit rows in place.
func dropRowIndexes(scope map[string]any) {
	delete(scope, rowIndexKey)
}

// intLike reports whether equal would compare v through its final, integer
// branch against another int-like value: a Go integer, or nil.
func intLike(v any) (int, bool) {
	switch t := v.(type) {
	case nil:
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case int32:
		return int(t), true
	}
	return 0, false
}

// indexedRows answers the rows of entity (the slice rows, as scope holds
// it) that can satisfy `row.field == probe`, in their original order, and
// whether an index could answer at all (false: scan every row).
func indexedRows(scope map[string]any, entity, field string, rows []any, probe any) ([]any, bool) {
	if !rowIndexing || len(rows) < 64 {
		return nil, false // a scan is cheaper than building anything
	}
	_, isInt := intLike(probe)
	_, isText := probe.(string)
	if !isInt && !isText {
		return nil, false
	}
	set, _ := scope[rowIndexKey].(rowIndexSet)
	if set == nil {
		set = rowIndexSet{}
		scope[rowIndexKey] = set
	}
	key := entity + "\x00" + field
	ix := set[key]
	if ix == nil || ix.first != &rows[0] || ix.n != len(rows) {
		ix = &rowIndex{first: &rows[0], n: len(rows), rows: rows, field: field}
		set[key] = ix
	}
	var exact, loose []int
	if isInt {
		ix.buildInts()
		if ix.ints == nil {
			return nil, false
		}
		k, _ := intLike(probe)
		exact, loose = ix.ints[k], ix.intsLoose
	} else {
		ix.buildTexts()
		if ix.texts == nil {
			return nil, false
		}
		exact, loose = ix.texts[probe.(string)], ix.textsLoose
	}
	rowIndexHits.Add(1)
	return ix.pick(exact, loose), true
}

// pick merges two ascending position lists into the rows they name, in row
// order.
func (ix *rowIndex) pick(a, b []int) []any {
	out := make([]any, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if j >= len(b) || (i < len(a) && a[i] < b[j]) {
			out = append(out, ix.rows[a[i]])
			i++
		} else {
			out = append(out, ix.rows[b[j]])
			j++
		}
	}
	return out
}

// buildInts fills the integer view; it stays nil when a row is not a record
// (a list cell's values are not indexed).
func (ix *rowIndex) buildInts() {
	if ix.ints != nil {
		return
	}
	ints := map[int][]int{}
	var loose []int
	for i, r := range ix.rows {
		m, ok := r.(record)
		if !ok {
			return
		}
		if k, ok := intLike(m[ix.field]); ok {
			ints[k] = append(ints[k], i)
		} else {
			loose = append(loose, i)
		}
	}
	ix.ints, ix.intsLoose = ints, loose
}

// buildTexts fills the text view.
func (ix *rowIndex) buildTexts() {
	if ix.texts != nil {
		return
	}
	texts := map[string][]int{}
	var loose []int
	for i, r := range ix.rows {
		m, ok := r.(record)
		if !ok {
			return
		}
		switch m[ix.field].(type) {
		case bytesVal, []any:
			loose = append(loose, i)
		default:
			k := toStr(m[ix.field])
			texts[k] = append(texts[k], i)
		}
	}
	ix.texts, ix.textsLoose = texts, loose
}

// pinnedEquality finds, among the top-level conjuncts of a predicate over
// item variable v, one that pins a field: `v.f == e` or `e == v.f` (isIn
// false), or `v.f in e` (isIn: e is a list of values), whose e is
// independent of v and plain to evaluate once — its field and the
// expression.
func pinnedEquality(where *ir.Expr, v string) (string, *ir.Expr, bool, bool) {
	if where == nil || v == "" {
		return "", nil, false, false
	}
	if where.Kind == "bin" && where.Op == "&&" {
		if f, e, isIn, ok := pinnedEquality(where.L, v); ok {
			return f, e, isIn, true
		}
		return pinnedEquality(where.R, v)
	}
	if where.Kind != "bin" {
		return "", nil, false, false
	}
	switch where.Op {
	case "==":
		if f, ok := itemField(where.L, v); ok && probeExpr(where.R, v) {
			return f, where.R, false, true
		}
		if f, ok := itemField(where.R, v); ok && probeExpr(where.L, v) {
			return f, where.L, false, true
		}
	case "in":
		if f, ok := itemField(where.L, v); ok && probeExpr(where.R, v) {
			return f, where.R, true, true
		}
	}
	return "", nil, false, false
}

// indexedRowsIn is indexedRows for `v.f in values`: the rows any one value
// pins, once each, in row order. A value no index can stand for (a bool, a
// float) means scanning after all.
func indexedRowsIn(scope map[string]any, entity, field string, rows []any, values any) ([]any, bool) {
	list, ok := values.([]any)
	if !ok || !rowIndexing || len(rows) < 64 {
		return nil, false
	}
	set, _ := scope[rowIndexKey].(rowIndexSet)
	if set == nil {
		set = rowIndexSet{}
		scope[rowIndexKey] = set
	}
	key := entity + "\x00" + field
	ix := set[key]
	if ix == nil || ix.first != &rows[0] || ix.n != len(rows) {
		ix = &rowIndex{first: &rows[0], n: len(rows), rows: rows, field: field}
		set[key] = ix
	}
	seen := map[int]bool{}
	var pos []int
	add := func(ps []int) {
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				pos = append(pos, p)
			}
		}
	}
	for _, v := range list {
		if k, ok := intLike(v); ok {
			ix.buildInts()
			if ix.ints == nil {
				return nil, false
			}
			add(ix.ints[k])
			add(ix.intsLoose)
		} else if t, ok := v.(string); ok {
			ix.buildTexts()
			if ix.texts == nil {
				return nil, false
			}
			add(ix.texts[t])
			add(ix.textsLoose)
		} else {
			return nil, false
		}
	}
	sort.Ints(pos)
	rowIndexHits.Add(1)
	return ix.pick(pos, nil), true
}

// itemField: `v.f`, a stored field of the item itself.
func itemField(e *ir.Expr, v string) (string, bool) {
	if e != nil && e.Kind == "get" && e.Field != "" && e.Obj != nil && e.Obj.Kind == "ref" && e.Obj.Name == v {
		return e.Field, true
	}
	return "", false
}

// probeExpr: an expression evaluated once for the whole predicate instead of
// once per row — so it must not read the item, and must be a plain value
// (literals, names, field reads, arithmetic, and the conversions a route
// parameter is read through), never an aggregate or a call whose value could
// depend on where it is evaluated.
func probeExpr(e *ir.Expr, v string) bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case "lit":
		return true
	case "ref":
		return e.Name != v
	case "get":
		return e.Obj != nil && probeExpr(e.Obj, v)
	case "un":
		return probeExpr(e.X, v)
	case "bin":
		return probeExpr(e.L, v) && probeExpr(e.R, v)
	case "eget":
		// a row looked up by a key that does not read the item: one value
		return e.Key != nil && probeExpr(e.Key, v)
	case "list":
		for _, a := range e.Args {
			if !probeExpr(a, v) {
				return false
			}
		}
		return true
	case "call":
		switch e.Name {
		case "toInt", "toStr", "lower", "upper", "trim":
			for _, a := range e.Args {
				if !probeExpr(a, v) {
					return false
				}
			}
			return true
		}
	}
	return false
}
