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
