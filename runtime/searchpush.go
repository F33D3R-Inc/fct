package runtime

import (
	"context"
	"sync/atomic"
	"time"

	"facet/internal/ir"
)

// Search pushdown: a case-insensitive substring filter answered by the
// database's folded text index instead of by reading every row.
//
// `contains(lower(w.body), lower(q))` over a kind of 10 000 rows was a scan of
// all of them — the whole of a search request's cost once the per-result
// aggregates were indexed (rowindex.go). FacetQL keeps a folded text index
// over a field the app searches this way when one is declared (the app's
// migrate declares them under FACET_FOLDED_INDEXES=1 — ir.SearchedFields
// names the fields, fqWantedIndexes declares them — or an operator does), and
// answers the same test from it: the
// addresses whose lowered text holds the probe's windows, rechecked there.
//
// The engine is asked for *candidates*, not for the answer. Its ids select
// the working set's own rows — in their own order — and the whole predicate
// is still evaluated on each, by the same evalRowPredicate a scan uses; the
// engine's `lower` is the language's own (predicate.rs's go_lower, table
// generated from Go's strings.ToLower; the fct engine uses the builtin), so
// every row the scan would keep is among them.
//
// What the engine does not have, it cannot name: a write this request made
// is in the working set but not yet in the database (an action commits at
// its end). So an entity this request has written — an `add` or `set` of
// it, or a `remove`, `clear`, `call`, `run` or `do`, which may reach any
// entity — is read by scanning for the rest of the request (markWritten). A
// write another request made was committed before it released the lock this
// request holds.

// searchIndexer is a store that keeps text indexes for the fields an app
// searches: it is told them (ir.SearchedFields — derived from the program,
// never part of its IR or schema) before Init or Migrate declares them.
type searchIndexer interface {
	setSearched(map[string][]string)
}

// tellSearched hands a store that indexes searches the fields graph's
// queries search.
func tellSearched(store Store, graph *ir.IR) {
	if si, ok := store.(searchIndexer); ok {
		si.setSearched(ir.SearchedFields(graph))
	}
}

// pushdownKey is the scope slot holding the server a request evaluates for;
// writtenKey the entities it has written ("" : all of them). `@` keys never
// reach a client.
const (
	pushdownKey = "@srv"
	writtenKey  = "@wrote"
)

// markWritten records that this request wrote entity (every entity for "").
func markWritten(scope map[string]any, entity string) {
	w, _ := scope[writtenKey].(map[string]bool)
	if w == nil {
		w = map[string]bool{}
		scope[writtenKey] = w
	}
	w[entity] = true
}

func writtenIn(scope map[string]any, entity string) bool {
	w, _ := scope[writtenKey].(map[string]bool)
	return w[""] || w[entity]
}

// searchPushdown is how many searches the database answered (tests read it).
var searchPushdown atomic.Int64

// minPushedNeedle: a needle shorter than one trigram cannot be served by
// the index — the engine would scan, and scanning the working set is cheaper.
const minPushedNeedle = 3

// searchedRows answers the rows of e's entity (rows, as the scope holds
// them) the database's folded text index names for a `contains(lower(v.f),
// probe)` conjunct of e's predicate, in row order — and whether it could.
func searchedRows(scope map[string]any, e *ir.Expr, rows []any) ([]any, bool) {
	s, _ := scope[pushdownKey].(*Server)
	if s == nil || !rowIndexing || len(rows) < 64 {
		return nil, false
	}
	fq, ok := s.store.(*fqStore)
	if !ok {
		return nil, false
	}
	field, pe, ok := searchedConjunct(e.Where, e.Var)
	if !ok || writtenIn(scope, e.Name) {
		return nil, false
	}
	ent, ok := s.entityByName(e.Name)
	if !ok || ent.Ephemeral || !fq.folded[ir.Index{Entity: e.Name, Field: field}] {
		return nil, false // no folded index over it: the engine would only scan
	}
	needle, ok := evalPerRow(pe, scope).(string)
	if !ok || len(needle) < minPushedNeedle {
		return nil, false
	}
	where := &ir.Expr{Kind: "bin", Op: "contains",
		L: &ir.Expr{Kind: "call", Name: "lower", Args: []*ir.Expr{{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: e.Var}, Field: field}}},
		R: &ir.Expr{Kind: "lit", Val: needle, VType: "text"}}
	ids := map[int]bool{}
	after := ""
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		nodes, next, err := fq.c.query(ctx, fqQueryRequest{Kind: e.Name, Where: where, ItemVar: e.Var, Limit: 500, After: after})
		if err != nil {
			s.obs.log.Error("search pushdown failed; scanning the working set", "entity", e.Name, "field", field, "error", err)
			return nil, false
		}
		for _, n := range nodes {
			rec, err := nodeRecord(ent, n)
			if err != nil {
				return nil, false
			}
			ids[toInt(rec["id"])] = true
		}
		if next == "" {
			break
		}
		after = next
	}
	out := make([]any, 0, len(ids))
	for _, r := range rows {
		if m, ok := r.(record); ok && ids[toInt(m["id"])] {
			out = append(out, r)
		}
	}
	searchPushdown.Add(1)
	return out, true
}

// searchedConjunct finds a top-level conjunct `contains(lower(v.f), e)` —
// the language's call, or its binary form — with e not reading v.
func searchedConjunct(where *ir.Expr, v string) (string, *ir.Expr, bool) {
	if where == nil || v == "" {
		return "", nil, false
	}
	if where.Kind == "bin" && where.Op == "&&" {
		if f, e, ok := searchedConjunct(where.L, v); ok {
			return f, e, true
		}
		return searchedConjunct(where.R, v)
	}
	var hay, needle *ir.Expr
	switch {
	case where.Kind == "call" && where.Name == "contains" && len(where.Args) == 2:
		hay, needle = where.Args[0], where.Args[1]
	case where.Kind == "bin" && where.Op == "contains":
		hay, needle = where.L, where.R
	default:
		return "", nil, false
	}
	if hay == nil || hay.Kind != "call" || hay.Name != "lower" || len(hay.Args) != 1 {
		return "", nil, false
	}
	f, ok := itemField(hay.Args[0], v)
	if !ok || !probeExpr(needle, v) {
		return "", nil, false
	}
	return f, needle, true
}
