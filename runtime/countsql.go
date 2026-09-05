package runtime

// Cardinality reads, compiled to SQL.
//
// `selectSQL` (sql.go) compiles a Query to a page of rows. A count is the same
// predicate asked a different question — how many, not which — and the answer is
// one integer however many rows it visits, so it has no page, no order and no
// cursor. Those fields are deliberately absent here rather than accepted and
// ignored: a caller handed a `limit` that did nothing would reasonably believe
// it had counted a page.
//
// The predicate compiler itself is not duplicated; both forms go through
// `exprSQL`, so "what can be pushed down" keeps one definition.

import (
	"fmt"
	"strings"

	"facet/internal/ir"
)

// countSQL compiles a Query to `SELECT count(*)` over the rows it selects.
func countSQL(query Query, e ir.Entity) (string, []any, error) {
	args := []any{}
	where, err := whereSQL(query, &args)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("SELECT count(*) FROM %s%s", q(table(e.Name)), where), args, nil
}

// countBySQL compiles a Query to one count per distinct value of a column.
//
// When values are given the read is narrowed to them — which is the shape a
// rendered page needs, and the shape an index over the grouped column answers by
// walking one key range per value rather than the whole table. With no values it
// groups everything, which is a genuine "how many of each" question and costs
// proportionally.
func countBySQL(query Query, e ir.Entity, groupBy string, values []any) (string, []any, error) {
	f, ok := fieldOf(e, groupBy)
	if !ok {
		return "", nil, fmt.Errorf("cannot group by unknown field %q of %s", groupBy, e.Name)
	}
	args := []any{}
	where, err := whereSQL(query, &args)
	if err != nil {
		return "", nil, err
	}
	if len(values) > 0 {
		holes := make([]string, len(values))
		for i, v := range values {
			args = append(args, colValue(f, v))
			holes[i] = fmt.Sprintf("$%d", len(args))
		}
		clause := fmt.Sprintf("%s IN (%s)", q(groupBy), strings.Join(holes, ", "))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}
	return fmt.Sprintf("SELECT %s, count(*) FROM %s%s GROUP BY %s",
		q(groupBy), q(table(e.Name)), where, q(groupBy)), args, nil
}

// whereSQL compiles a Query's predicate to a WHERE clause (empty when there is
// none), appending its literals as bound parameters.
func whereSQL(query Query, args *[]any) (string, error) {
	if query.Where == nil {
		return "", nil
	}
	pred, err := exprSQL(query.Where, query.ItemVar, args)
	if err != nil {
		return "", err
	}
	return " WHERE " + pred, nil
}

// aggregateSQL compiles a Query to `SELECT <fn>(col)` over the rows it selects.
//
// COALESCE is what makes the empty reduction 0 rather than NULL. The language
// types these as the column's own numeric type and has no hole to put a NULL in,
// so `max(Order.amount)` over no orders is 0 here exactly as it is in reduceAgg.
// Without it the seam would answer NULL where the mirror answers 0, and which of
// the two a viewer saw would depend on whether the aggregate happened to be
// pushed down.
func aggregateSQL(query Query, e ir.Entity, spec AggSpec) (string, []any, error) {
	fn, col, err := aggColumn(e, spec)
	if err != nil {
		return "", nil, err
	}
	args := []any{}
	where, err := whereSQL(query, &args)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("SELECT COALESCE(%s(%s), 0) FROM %s%s",
		fn, col, q(table(e.Name)), where), args, nil
}

// aggregateBySQL compiles a Query to one reduction per distinct value of a
// column — the grouped form, narrowed to the values a page is rendering for the
// same reason countBySQL is.
func aggregateBySQL(query Query, e ir.Entity, spec AggSpec, groupBy string, values []any) (string, []any, error) {
	fn, col, err := aggColumn(e, spec)
	if err != nil {
		return "", nil, err
	}
	g, ok := fieldOf(e, groupBy)
	if !ok {
		return "", nil, fmt.Errorf("cannot group by unknown field %q of %s", groupBy, e.Name)
	}
	args := []any{}
	where, err := whereSQL(query, &args)
	if err != nil {
		return "", nil, err
	}
	if len(values) > 0 {
		holes := make([]string, len(values))
		for i, v := range values {
			args = append(args, colValue(g, v))
			holes[i] = fmt.Sprintf("$%d", len(args))
		}
		clause := fmt.Sprintf("%s IN (%s)", q(groupBy), strings.Join(holes, ", "))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}
	return fmt.Sprintf("SELECT %s, COALESCE(%s(%s), 0) FROM %s%s GROUP BY %s",
		q(groupBy), fn, col, q(table(e.Name)), where, q(groupBy)), args, nil
}

// aggColumn validates a reduction against the schema and returns the SQL
// function and the quoted column.
//
// Both halves are checked here rather than trusted from the caller, because this
// is the one place a name reaches a statement: an unknown function or an
// undeclared column would otherwise be interpolated straight into SQL. The
// function is matched against a closed set and the column against the entity's
// declared fields, so nothing the caller supplies can be anything else.
func aggColumn(e ir.Entity, spec AggSpec) (string, string, error) {
	var fn string
	switch spec.Func {
	case "sum":
		fn = "SUM"
	case "min":
		fn = "MIN"
	case "max":
		fn = "MAX"
	default:
		return "", "", fmt.Errorf("cannot push down aggregate %q", spec.Func)
	}
	f, ok := fieldOf(e, spec.Field)
	if !ok {
		return "", "", fmt.Errorf("cannot %s unknown field %q of %s", spec.Func, spec.Field, e.Name)
	}
	if !numericField(f) {
		return "", "", fmt.Errorf("cannot %s %s.%s: %s is not a numeric column",
			spec.Func, e.Name, spec.Field, f.Type)
	}
	return fn, q(spec.Field), nil
}
