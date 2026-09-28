package runtime

import (
	"encoding/json"

	"facet/internal/ir"
)

// pageIR is the IR a page ships to the browser in its `#fa-ir` script: the
// fields runtime/assets/facet.js reads (`ir.states`, `ir.policies`,
// `ir.actions`, `ir.components`, `ir.routes`, `ir.bindings`, `ir.view`,
// `ir.depGraph` — there are no others), each cut to what this page can use,
// under the same JSON names and in the same order as ir.IR's.
//
// It used to be the whole graph with the page's own view swapped in, so
// every page carried every action body, proc, derive, type, message, API
// and entity of the app, none of which the client reads: on f33d3r.com
// that was 4.7 MB on an empty "/", 77% of it action bodies the page could
// never dispatch. A type naming only what the client reads keeps a field
// added to ir.IR tomorrow from riding along on every page by default.
type pageIR struct {
	States     []ir.State          `json:"states"`
	Policies   []ir.Policy         `json:"policies"`
	Actions    []ir.Action         `json:"actions"`
	Components []ir.Component      `json:"components,omitempty"`
	Routes     []ir.Route          `json:"routes,omitempty"`
	Bindings   []ir.Binding        `json:"bindings"`
	View       []ir.Node           `json:"view"`
	DepGraph   map[string][]string `json:"depGraph"`
}

// pageIRJSON builds and encodes pg's pageIR.
//
//   - states: every cell; the client coerces a control's value by its cell's
//     type, and the page's store holds every cell.
//   - policies: those a route's guard names — the client evaluates a policy
//     only to decide whether a link to a guarded route is shown.
//   - actions: those the page's view and the components it reaches can
//     dispatch (a button, a form, a control's `on change`, a list's `more`).
//     The client dispatches nothing else: an action body has no `run`, and a
//     name the page does not reach is never on an element.
//   - components: the closure of `use` from the page's view (pageComponents).
//   - routes: all of them, for SPA link matching and link hiding.
func (s *Server) pageIRJSON(pg *ir.Page) ([]byte, error) {
	comps := s.pageComponents(pg)

	used := map[string]bool{}
	var nodes func([]ir.Node)
	nodes = func(list []ir.Node) {
		for i := range list {
			nd := &list[i]
			if nd.Action != "" {
				used[nd.Action] = true
			}
			if nd.More != "" {
				used[nd.More] = true
			}
			nodes(nd.Children)
		}
	}
	nodes(pg.View)
	for i := range comps {
		nodes(comps[i].View)
	}
	var actions []ir.Action
	for i := range s.ir.Actions {
		if used[s.ir.Actions[i].Name] {
			actions = append(actions, s.ir.Actions[i])
		}
	}

	guards := map[string]bool{}
	for _, rt := range s.ir.Routes {
		if rt.Requires != "" {
			guards[rt.Requires] = true
		}
	}
	var policies []ir.Policy
	for i := range s.ir.Policies {
		if guards[s.ir.Policies[i].Name] {
			policies = append(policies, s.ir.Policies[i])
		}
	}

	return json.Marshal(&pageIR{
		States:     s.ir.States,
		Policies:   policies,
		Actions:    actions,
		Components: comps,
		Routes:     s.ir.Routes,
		Bindings:   pg.Bindings,
		View:       pg.View,
		DepGraph:   pg.DepGraph,
	})
}
