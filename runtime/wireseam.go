package runtime

import (
	"encoding/json"
	"fmt"

	"facet/internal/ir"
	wire "facet/schema/generated"
)

// wireExprFromIR/wireExprToIR are the real translation seam SCHEMA_IDL_SCOPE.md
// asked for between fct's own compiler/runtime predicate IR (ir.Expr,
// internal/ir/ir.go — used live by eval.go/region.go/server.go/pushdown.go,
// not merely a wire-serialization convenience) and the wire Expr type
// generated from the one authoritative schema (schema/facetql_wire.fct).
//
// Before this file existed, ir.Expr's own JSON tags happened to mirror
// FacetQL's real predicate.rs shape by convention — a second, informal
// definition of the same wire type, exactly the "mirrored twice" drift
// surface SCHEMA_IDL_SCOPE.md opened by naming. Every place that sends or
// receives predicate JSON over the wire now goes through this seam instead;
// ir.Expr's tags are no longer wire-facing (see ir.Expr's own doc comment).

// wireExprFromIR converts an ir.Expr predicate to its wire form.
//
// e.Sel (an aggregate reduction — kind "agg" only) has no wire counterpart:
// FacetQL's predicate.rs has never had an aggregation concept, which is
// exactly why region.go's pushable()/exprSQL only ever pushes lit/get/un/bin
// nodes and refuses — forcing in-memory evaluation instead — any expression
// containing an "agg" node. A non-nil Sel reaching this function means that
// guarantee broke somewhere upstream of the wire boundary, so this returns an
// error rather than silently dropping data a caller might expect to have
// been sent.
func wireExprFromIR(e *ir.Expr) (*wire.Expr, error) {
	if e == nil {
		return nil, nil
	}
	if e.Sel != nil {
		return nil, fmt.Errorf("wireExprFromIR: %q expression carries a Sel (aggregate reduction), which has no wire representation — pushable() should have refused this before it reached the wire boundary", e.Kind)
	}
	var val json.RawMessage
	if e.Val != nil {
		b, err := json.Marshal(e.Val)
		if err != nil {
			return nil, fmt.Errorf("wireExprFromIR: encode %q literal: %w", e.Kind, err)
		}
		val = b
	}
	obj, err := wireExprFromIR(e.Obj)
	if err != nil {
		return nil, err
	}
	key, err := wireExprFromIR(e.Key)
	if err != nil {
		return nil, err
	}
	l, err := wireExprFromIR(e.L)
	if err != nil {
		return nil, err
	}
	r, err := wireExprFromIR(e.R)
	if err != nil {
		return nil, err
	}
	x, err := wireExprFromIR(e.X)
	if err != nil {
		return nil, err
	}
	where, err := wireExprFromIR(e.Where)
	if err != nil {
		return nil, err
	}
	var args []wire.Expr
	if e.Args != nil {
		args = make([]wire.Expr, 0, len(e.Args))
		for _, a := range e.Args {
			wa, err := wireExprFromIR(a)
			if err != nil {
				return nil, err
			}
			if wa != nil {
				args = append(args, *wa)
			}
		}
	}
	return &wire.Expr{
		Kind:  e.Kind,
		Val:   val,
		Vtype: e.VType,
		Name:  e.Name,
		Field: e.Field,
		Obj:   obj,
		Key:   key,
		Op:    e.Op,
		Args:  args,
		L:     l,
		R:     r,
		X:     x,
		Var:   e.Var,
		Where: where,
	}, nil
}

// wireExprToIR is wireExprFromIR's inverse. It never fails: the wire Expr has
// no Sel case to reject in the first place, so a decoded wire Expr is always
// representable as an ir.Expr (with Sel left nil — nothing on the wire ever
// carries one).
func wireExprToIR(e *wire.Expr) *ir.Expr {
	if e == nil {
		return nil
	}
	var val any
	if len(e.Val) > 0 {
		// Best-effort: a predicate literal is always a JSON scalar (see
		// litSQLValue), so this only fails on a malformed wire payload, which
		// the caller's own json.Unmarshal into wire.Expr would already have
		// surfaced first for any structural problem.
		_ = json.Unmarshal(e.Val, &val)
	}
	var args []*ir.Expr
	if e.Args != nil {
		args = make([]*ir.Expr, 0, len(e.Args))
		for i := range e.Args {
			args = append(args, wireExprToIR(&e.Args[i]))
		}
	}
	return &ir.Expr{
		Kind:  e.Kind,
		Val:   val,
		VType: e.Vtype,
		Name:  e.Name,
		Field: e.Field,
		Obj:   wireExprToIR(e.Obj),
		Key:   wireExprToIR(e.Key),
		Op:    e.Op,
		Args:  args,
		L:     wireExprToIR(e.L),
		R:     wireExprToIR(e.R),
		X:     wireExprToIR(e.X),
		Var:   e.Var,
		Where: wireExprToIR(e.Where),
	}
}
