package runtime

// The query-pushdown contract every Store backend shares.
//
// This used to be "pure SQL generation for the Postgres store" (see
// AGENT_LOG.md, "Changelog — 2026-09-06 (evening)" for why that backend is
// gone) — but the symbols below outlived it, because they turned out to be
// backend-agnostic even though they speak in SQL-shaped terms:
//
//   - Query is the one pushdown request shape every Store.Query/Count/
//     Aggregate implementation takes, FacetQL's fqStore included.
//   - exprSQL is "the one definition of what predicate a store can push
//     down" (see region.go's pushable), not "the SQL FacetQL's store
//     actually runs" — fqStore never calls it to build a request; it calls
//     it, via pushable, purely to ask the same question Postgres used to
//     answer by compiling the predicate and checking for an error.
//   - columns/fieldByName/colValue/fkName/indexName are the shared column
//     and naming conventions fqStore's own at-rest encoding and index/
//     reference naming were built to match, so a row (or an index name)
//     means the same thing regardless of which store wrote it.
//
// Identifiers (entity/field names) are validated in the compiler, so q here
// needs no real escaping — it exists because exprSQL's "get" case still
// needs to spell a column name in the predicate text it returns.
//
// Nothing in this file touches a database handle; it is pure functions from
// the IR (and, for cursors, an opaque page position) to values or text.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"facet/internal/ir"
)

// q double-quotes an identifier — kept only because exprSQL's returned
// predicate text still spells column names this way.
func q(ident string) string { return `"` + ident + `"` }

// columns returns an entity's column names in a stable order: id first (always
// the primary key), then every declared field except a redeclared id.
func columns(e ir.Entity) []string {
	cols := []string{"id"}
	for _, f := range e.Fields {
		if f.Name == "id" {
			continue
		}
		cols = append(cols, f.Name)
	}
	return cols
}

// fieldByName indexes an entity's fields for type lookups (id is implicit int).
func fieldByName(e ir.Entity) map[string]ir.Field {
	m := map[string]ir.Field{"id": {Name: "id", Type: "int"}}
	for _, f := range e.Fields {
		m[f.Name] = f
	}
	return m
}

// fkName is the deterministic constraint name for a relation column — shared
// by fqStore's reference naming (fqSafeName wraps this) so the two backends'
// migrations name the same thing the same way.
func fkName(entity, field string) string { return "fk_" + entity + "_" + field }

// indexName is the deterministic index name for a column — see fkName.
func indexName(entity, field string) string { return "idx_" + entity + "_" + field }

// colValue coerces a Go value to the representation its column stores. A
// @secret text column is encrypted here, so the database only ever holds
// ciphertext for it. fqStore's rowNode uses this for the same reason a SQL
// store once did: one definition of at-rest coercion, so the two backends
// store equivalent values.
func colValue(f ir.Field, v any) any {
	switch {
	case f.IsRelation() || f.Type == "int":
		return int64(toInt(v))
	case f.Type == "bool":
		return truthy(v)
	default:
		s := toStr(v)
		if f.Secret {
			s = encryptSecret(s)
		}
		return s
	}
}

// ── query pushdown ──────────────────────────────────────────────────────────

// Query is a read pushed down to the store: an optional predicate over the
// row, an order, a page size, and a keyset cursor for the next page. The
// runtime builds one from a list's `where`/`by`/`limit` (or the JSON API's
// query params) so a large table is never loaded whole.
type Query struct {
	Entity  string
	Where   *ir.Expr // predicate over the item; nil = all rows
	ItemVar string   // the loop variable the predicate's field accesses use
	Order   string   // order column; "" = by id
	Desc    bool
	Limit   int    // page size; 0 = a default page
	After   string // opaque keyset cursor from a previous page; "" = first page
}

const defaultPageSize = 100

// exprSQL compiles a pushed-down predicate to the SQL-shaped boolean text a
// store that speaks SQL would use, appending each literal as a bound
// parameter. It accepts the subset of the expression language that maps
// cleanly to a single-table WHERE: literals, the loop item's fields
// (item.field -> column), comparison and boolean operators, and arithmetic.
// References it cannot push down (a bare state name, an aggregate, an
// effectful call) are rejected so the caller can fall back rather than emit
// wrong SQL.
//
// The only backend that still executes this text is gone (see the file
// comment), but the compilation itself is not: region.go's pushable calls
// this purely for the error — "can this predicate be pushed down at all" is
// one definition, shared, rather than restated per backend.
func exprSQL(e *ir.Expr, itemVar string, args *[]any) (string, error) {
	if e == nil {
		return "", fmt.Errorf("nil predicate")
	}
	switch e.Kind {
	case "lit":
		*args = append(*args, litSQLValue(e))
		return fmt.Sprintf("$%d", len(*args)), nil
	case "get":
		if e.Obj != nil && e.Obj.Kind == "ref" && e.Obj.Name == itemVar {
			return q(e.Field), nil
		}
		return "", fmt.Errorf("cannot push down field access %q", e.Field)
	case "un":
		x, err := exprSQL(e.X, itemVar, args)
		if err != nil {
			return "", err
		}
		switch e.Op {
		case "!":
			return "(NOT " + x + ")", nil
		case "-":
			return "(-" + x + ")", nil
		}
	case "bin":
		l, err := exprSQL(e.L, itemVar, args)
		if err != nil {
			return "", err
		}
		r, err := exprSQL(e.R, itemVar, args)
		if err != nil {
			return "", err
		}
		op, ok := sqlBinOp(e.Op)
		if !ok {
			return "", fmt.Errorf("cannot push down operator %q", e.Op)
		}
		return fmt.Sprintf("(%s %s %s)", l, op, r), nil
	}
	return "", fmt.Errorf("cannot push down %q expression", e.Kind)
}

// sqlBinOp maps a Facet operator to its SQL spelling.
func sqlBinOp(op string) (string, bool) {
	switch op {
	case "==":
		return "=", true
	case "!=":
		return "<>", true
	case "&&":
		return "AND", true
	case "||":
		return "OR", true
	case "<", "<=", ">", ">=", "+", "-", "*", "/", "%":
		return op, true
	}
	return "", false
}

// litSQLValue is a literal's bound-parameter value.
func litSQLValue(e *ir.Expr) any {
	switch e.VType {
	case "int":
		return int64(toInt(e.Val))
	case "bool":
		b, _ := e.Val.(bool)
		return b
	default:
		return toStr(e.Val)
	}
}

// ── keyset cursors ──────────────────────────────────────────────────────────

type cursor struct {
	O any `json:"o"` // the last row's order value
	I int `json:"i"` // the last row's id (the tiebreak)
}

// encodeCursor mints an opaque cursor pointing just past one row.
func encodeCursor(orderVal any, id int) string {
	b, _ := json.Marshal(cursor{O: orderVal, I: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a cursor; ok is false if it is malformed.
func decodeCursor(s string) (orderVal any, id int, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, 0, false
	}
	var c cursor
	if json.Unmarshal(b, &c) != nil {
		return nil, 0, false
	}
	// JSON numbers decode as float64; the order value may be numeric or text.
	switch v := c.O.(type) {
	case float64:
		return int64(v), c.I, true
	default:
		return v, c.I, true
	}
}
