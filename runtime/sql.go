package runtime

// Pure SQL generation for the Postgres store. Everything here is a function from
// the IR (and a snapshot of the live schema) to SQL text plus bound arguments —
// no database handle is touched. That keeps the brains of the store testable
// without a live Postgres: the wrappers in store.go only execute what these
// functions produce. The Facet types map to real, indexed columns:
//
//	int          -> BIGINT
//	money        -> BIGINT (integer minor units / cents)
//	date         -> BIGINT (unix seconds)
//	text         -> TEXT
//	bool         -> BOOLEAN
//	<Entity>     -> BIGINT, a foreign key to facet_<Entity>(id) ON DELETE CASCADE
//
// Identifiers (entity/field names) are validated identifiers in the compiler, so
// they need no escaping, but we still double-quote them so a column can never
// collide with a SQL keyword.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"facet/internal/ir"
)

// table is the physical table name for an entity.
func table(entity string) string { return "facet_" + entity }

// q double-quotes an identifier.
func q(ident string) string { return `"` + ident + `"` }

// sqlType maps a field to its Postgres column type.
func sqlType(f ir.Field) string {
	switch {
	case f.IsRelation():
		return "BIGINT"
	case f.Type == "int", f.Type == "money", f.Type == "date":
		return "BIGINT"
	case f.Type == "bool":
		return "BOOLEAN"
	default:
		return "TEXT"
	}
}

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

// createTableSQL is the CREATE TABLE for an entity: one typed column per field,
// id as the primary key. Foreign keys are added separately (addFKSQL) so tables
// can be created in any order even when relations point at entities declared
// later.
func createTableSQL(e ir.Entity) string {
	fb := fieldByName(e)
	var defs []string
	for _, c := range columns(e) {
		if c == "id" {
			defs = append(defs, q("id")+" BIGINT PRIMARY KEY")
			continue
		}
		defs = append(defs, q(c)+" "+sqlType(fb[c]))
	}
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", q(table(e.Name)), strings.Join(defs, ", "))
}

// fkName is the deterministic constraint name for a relation column.
func fkName(entity, field string) string { return "fk_" + entity + "_" + field }

// addFKSQL emits an idempotent foreign key for every relation field, with ON
// DELETE CASCADE so removing a parent row drops its children in the database.
// Postgres has no ADD CONSTRAINT IF NOT EXISTS, so each is wrapped in a DO block
// that swallows a duplicate-object error — safe to run on every startup.
func addFKSQL(e ir.Entity) []string {
	var out []string
	for _, f := range e.Fields {
		if !f.IsRelation() {
			continue
		}
		out = append(out, fmt.Sprintf(
			`DO $$ BEGIN ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s(%s) ON DELETE CASCADE; EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
			q(table(e.Name)), q(fkName(e.Name, f.Name)), q(f.Name), q(table(f.Ref)), q("id")))
	}
	return out
}

// indexName is the deterministic index name for a column.
func indexName(entity, field string) string { return "idx_" + entity + "_" + field }

// createIndexSQL emits CREATE INDEX IF NOT EXISTS for every field the compiler
// flagged (relations, and any filtered or ordered field).
func createIndexSQL(e ir.Entity) []string {
	var out []string
	for _, f := range e.Fields {
		if !f.Index {
			continue
		}
		out = append(out, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)",
			q(indexName(e.Name, f.Name)), q(table(e.Name)), q(f.Name)))
	}
	return out
}

// upsertSQL is the INSERT … ON CONFLICT (id) DO UPDATE for an entity — one write
// path for both insert and update, keyed by id.
func upsertSQL(e ir.Entity) string {
	cols := columns(e)
	ph := make([]string, len(cols))
	qc := make([]string, len(cols))
	var sets []string
	for i, c := range cols {
		ph[i] = fmt.Sprintf("$%d", i+1)
		qc[i] = q(c)
		if c != "id" {
			sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", q(c), q(c)))
		}
	}
	if len(sets) == 0 {
		// an entity with only an id: nothing to update, just ignore a re-insert.
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO NOTHING",
			q(table(e.Name)), strings.Join(qc, ", "), strings.Join(ph, ", "))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (id) DO UPDATE SET %s",
		q(table(e.Name)), strings.Join(qc, ", "), strings.Join(ph, ", "), strings.Join(sets, ", "))
}

// rowArgs returns a row's values in column order, coerced to the column type, for
// an upsertSQL execution. A missing field becomes the type's zero value.
func rowArgs(e ir.Entity, row map[string]any) []any {
	fb := fieldByName(e)
	cols := columns(e)
	args := make([]any, len(cols))
	for i, c := range cols {
		args[i] = colValue(fb[c], row[c])
	}
	return args
}

// colValue coerces a Go value to the representation its column stores. A
// @secret text column is encrypted here, so the database only ever holds
// ciphertext for it.
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

// Query is a read pushed down to SQL: an optional predicate over the row, an
// order, a page size, and a keyset cursor for the next page. The runtime builds
// one from a list's `where`/`by`/`limit` (or the JSON API's query params) so a
// large table is never loaded whole.
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

// selectSQL compiles a Query to an indexed SELECT with keyset pagination. It
// asks for one row more than the page size so the caller can tell whether a next
// page exists and mint a cursor. Ordering always breaks ties on id, so the
// keyset is total and stable.
func selectSQL(query Query, e ir.Entity) (string, []any, error) {
	args := []any{}
	var conds []string

	if query.Where != nil {
		pred, err := exprSQL(query.Where, query.ItemVar, &args)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, "("+pred+")")
	}

	order := query.Order
	if order == "" {
		order = "id"
	}
	dir, cmp := "ASC", ">"
	if query.Desc {
		dir, cmp = "DESC", "<"
	}

	if query.After != "" {
		ov, id, ok := decodeCursor(query.After)
		if !ok {
			return "", nil, fmt.Errorf("invalid cursor")
		}
		// keyset: (order, id) strictly past the last row of the previous page.
		if order == "id" {
			args = append(args, int64(id))
			conds = append(conds, fmt.Sprintf("%s %s $%d", q("id"), cmp, len(args)))
		} else {
			args = append(args, ov)
			oph := len(args)
			args = append(args, int64(id))
			iph := len(args)
			conds = append(conds, fmt.Sprintf("(%s %s $%d OR (%s = $%d AND %s %s $%d))",
				q(order), cmp, oph, q(order), oph, q("id"), cmp, iph))
		}
	}

	limit := query.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}

	var qc []string
	for _, c := range columns(e) {
		qc = append(qc, q(c))
	}
	sql := fmt.Sprintf("SELECT %s FROM %s", strings.Join(qc, ", "), q(table(e.Name)))
	if len(conds) > 0 {
		sql += " WHERE " + strings.Join(conds, " AND ")
	}
	if order == "id" {
		sql += fmt.Sprintf(" ORDER BY %s %s", q("id"), dir)
	} else {
		sql += fmt.Sprintf(" ORDER BY %s %s, %s %s", q(order), dir, q("id"), dir)
	}
	sql += fmt.Sprintf(" LIMIT %d", limit+1) // +1 sentinel: is there a next page?
	return sql, args, nil
}

// exprSQL compiles a pushed-down predicate to a SQL boolean, appending each
// literal as a bound parameter (so values are never interpolated). It accepts
// the subset of the expression language that maps cleanly to a single-table
// WHERE: literals, the loop item's fields (item.field -> column), comparison and
// boolean operators, and arithmetic. References it cannot push down (a bare
// state name, an aggregate, an effectful call) are rejected so the caller can
// fall back rather than emit wrong SQL.
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

// ── migration planning ──────────────────────────────────────────────────────

// schema is a snapshot of the live database the planner diffs against: which
// columns each table already has, and which indexes/constraints exist by name.
type schema struct {
	cols    map[string]map[string]bool // table -> column -> exists
	indexes map[string]bool            // index/constraint name -> exists
}

// planMigration is the pure diff: given the live schema and the desired
// entities, it returns the ordered DDL that makes the database match the IR.
// Every statement is additive — CREATE TABLE / ADD COLUMN / ADD CONSTRAINT /
// CREATE INDEX — so applying a plan never drops data and never locks a table for
// a rewrite: schema changes are safe to run while the old version is serving.
func planMigration(s schema, entities []ir.Entity) []string {
	var plan []string
	for _, e := range entities {
		t := table(e.Name)
		existing := s.cols[t]
		if existing == nil {
			// brand-new table: full create, then its keys and indexes.
			plan = append(plan, createTableSQL(e))
			plan = append(plan, addFKSQL(e)...)
			plan = append(plan, createIndexSQL(e)...)
			continue
		}
		// existing table: add any column the IR has gained.
		fb := fieldByName(e)
		for _, c := range columns(e) {
			if c == "id" || existing[c] {
				continue
			}
			plan = append(plan, fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
				q(t), q(c), sqlType(fb[c])))
		}
		// add any missing relation key.
		for _, f := range e.Fields {
			if f.IsRelation() && !s.indexes[fkName(e.Name, f.Name)] {
				plan = append(plan, addFKSQL(e)...)
				break
			}
		}
		// add any missing index.
		for _, f := range e.Fields {
			if f.Index && !s.indexes[indexName(e.Name, f.Name)] {
				plan = append(plan, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)",
					q(indexName(e.Name, f.Name)), q(t), q(f.Name)))
			}
		}
	}
	return plan
}
