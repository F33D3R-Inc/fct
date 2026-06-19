package ir

import "facet/internal/parser"

// CompileExpr compiles a single standalone expression against an already-built
// application graph, returning its lowered, validated IR form. It is the bridge
// the `facet console` and the test runner use to evaluate ad-hoc expressions
// (entity reads, derives, aggregates, builtins) over a live app, reusing the
// exact lowering and checking the compiler applies everywhere else — so the
// console computes a value identically to the running app.
func CompileExpr(graph *IR, src string) (*Expr, error) {
	ex, err := parser.ParseExpr(src)
	if err != nil {
		return nil, err
	}
	e := envFromIR(graph)
	if err := e.check(ex, withActor(nil), 1); err != nil {
		return nil, err
	}
	return e.low(ex), nil
}

// envFromIR reconstructs the name environment from a compiled graph: the
// states, entities (and their fields), enums, and the inlinable zero-arg
// policies and derives. It is enough to validate and lower an expression that
// reads them. Actions, views, and components are not referenceable from an
// expression, so they are omitted.
func envFromIR(graph *IR) *env {
	e := &env{
		states:       map[string]string{},
		entities:     map[string]bool{},
		entityFields: map[string]map[string]bool{},
		indexFields:  map[string]map[string]bool{},
		inline:       map[string]*Expr{},
		policySet:    map[string]bool{},
		policyParams: map[string][]Param{},
		enums:        map[string][]string{},
		components:   map[string][]Param{},
		compDeps:     map[string]map[string]bool{},
		stateTypes:   map[string]string{},
	}
	for _, en := range graph.Enums {
		e.enums[en.Name] = en.Values
	}
	for _, ent := range graph.Entities {
		e.entities[ent.Name] = true
		e.entityFields[ent.Name] = map[string]bool{"id": true}
		for _, f := range ent.Fields {
			e.entityFields[ent.Name][f.Name] = true
		}
	}
	for _, st := range graph.States {
		e.states[st.Name] = st.Placement
		core := st.Type
		if st.List {
			core = st.Elem
		}
		e.stateTypes[st.Name] = core
	}
	for i := range graph.Policies {
		p := &graph.Policies[i]
		e.policySet[p.Name] = true
		e.policyParams[p.Name] = p.Params
		if len(p.Params) == 0 {
			e.inline[p.Name] = p.Expr // already lowered
		}
	}
	for i := range graph.Derives {
		e.inline[graph.Derives[i].Name] = graph.Derives[i].Expr // already lowered
	}
	return e
}
