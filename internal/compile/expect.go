package compile

import (
	"fmt"
	"strings"

	"facet/internal/ast"
)

// resolveExpects settles every `expect` declaration the merged graph carries
// (ast.Expect) against what the graph actually declares, and clears them:
// an expectation never reaches internal/ir.
//
// A library facet is a fragment. Its StreamCard takes `s: Stream`, its
// PayButton calls `pay(cents)`, its ComposeBox binds `draft`, its UserChip
// links to `/profile/{h}` — and the entity, action, cell and route are the
// host app's to declare. `expect` lets the fragment say what it needs, and
// this is where that need meets the host, with three outcomes per
// expectation:
//
//   - the host declares it and the declaration fits: an entity carrying every
//     expected field with the expected type (more fields are fine — the shape
//     is structural), an action with the expected signature, a cell of the
//     expected type, a view at the expected route, a stream carrying the
//     expected event with the expected payload, a policy with the expected
//     signature. The expectation is discharged.
//   - the host declares it and it does not fit: an error naming the module
//     that expected it, the host declaration, and the exact mismatch. The
//     fragment was written against one shape and the host offers another;
//     compiling on would put a component over a row it cannot read.
//   - nothing declares it: the expectation stands in. The entity, action or
//     cell is synthesized as declared — an entity with just those fields, an
//     action with that signature and no body, a client cell with its type's
//     zero value — a route goes to App.ExpectedRoutes for the link check and
//     an event to App.ExpectedEvents for the emit check.
//     This is what makes `facet check live/streamcard.fct` meaningful: the
//     fragment compiles against exactly the shape it declared, and against
//     nothing it did not.
//
// Two modules may expect the same name. Their entity shapes are unioned (a
// field both name must agree on its type); their action signatures and cell
// types must be identical, since a host can only implement one.
func resolveExpects(app *ast.App) error {
	if len(app.Expects) == 0 {
		return nil
	}
	entities := map[string]*ast.Entity{}
	for _, e := range app.Entities {
		entities[e.Name] = e
	}
	actions := map[string]*ast.Action{}
	for _, a := range app.Actions {
		actions[a.Name] = a
	}
	states := map[string]*ast.State{}
	for _, s := range app.States {
		states[s.Name] = s
	}
	policies := map[string]*ast.Policy{}
	for _, p := range app.Policies {
		policies[p.Name] = p
	}
	served := map[string]bool{}
	for _, v := range app.Views {
		served[v.Path] = true
	}
	carried := map[string]string{} // event name -> payload type some stream carries
	for _, st := range app.Streams {
		for _, ev := range st.Events {
			carried[ev.Name] = ev.Type
		}
	}

	// Synthesized declarations, by name, so a second module expecting the
	// same name merges into (or is checked against) the first's stand-in
	// rather than being declared twice.
	synthEntity := map[string]*ast.Entity{}
	synthAction := map[string]*ast.Action{}
	synthState := map[string]*ast.State{}
	expectedRoute := map[string]bool{}
	expectedEvent := map[string]string{}
	synthPolicy := map[string]*ast.Policy{}

	where := func(ex *ast.Expect) string {
		src := ex.Source
		if src == "" {
			src = app.Source
		}
		if src == "" {
			return fmt.Sprintf("line %d", ex.Line)
		}
		return fmt.Sprintf("%s:%d", src, ex.Line)
	}

	for _, ex := range app.Expects {
		switch {
		case ex.Entity != nil:
			want := ex.Entity
			if have, ok := entities[want.Name]; ok {
				for _, wf := range want.Fields {
					hf := findField(have, wf.Name)
					if hf == nil {
						return fmt.Errorf("%s expects entity %s to have a field `%s: %s`, but this app's entity %s (line %d) declares no field %q — a fragment's expectation is structural: the host's entity must carry every field it names",
							where(ex), want.Name, wf.Name, fieldType(wf), want.Name, have.Line, wf.Name)
					}
					if fieldType(*hf) != fieldType(wf) {
						return fmt.Errorf("%s expects entity %s's field %q to be %s, but this app's entity %s (line %d) declares it as %s",
							where(ex), want.Name, wf.Name, fieldType(wf), want.Name, have.Line, fieldType(*hf))
					}
				}
				continue
			}
			syn, ok := synthEntity[want.Name]
			if !ok {
				syn = &ast.Entity{Name: want.Name, Line: want.Line}
				synthEntity[want.Name] = syn
				app.Entities = append(app.Entities, syn)
			}
			for _, wf := range want.Fields {
				if sf := findField(syn, wf.Name); sf != nil {
					if fieldType(*sf) != fieldType(wf) {
						return fmt.Errorf("%s expects entity %s's field %q to be %s, but another module expects it to be %s — two fragments cannot be hosted by one entity that way",
							where(ex), want.Name, wf.Name, fieldType(wf), fieldType(*sf))
					}
					continue
				}
				syn.Fields = append(syn.Fields, wf)
			}

		case ex.Action != nil:
			want := ex.Action
			if have, ok := actions[want.Name]; ok {
				if sig := signatureOf(have); sig != signatureOf(want) {
					return fmt.Errorf("%s expects action %s, but this app's action (line %d) is %s — the fragment invokes the signature it expected",
						where(ex), signatureOf(want), have.Line, sig)
				}
				continue
			}
			if syn, ok := synthAction[want.Name]; ok {
				if signatureOf(syn) != signatureOf(want) {
					return fmt.Errorf("%s expects action %s, but another module expects %s — one host action cannot satisfy both",
						where(ex), signatureOf(want), signatureOf(syn))
				}
				continue
			}
			syn := &ast.Action{Name: want.Name, Params: want.Params, Ret: want.Ret, RetList: want.RetList, Line: want.Line}
			synthAction[want.Name] = syn
			app.Actions = append(app.Actions, syn)

		case ex.State != nil:
			want := ex.State
			if have, ok := states[want.Name]; ok {
				if have.Type != want.Type || have.Optional != want.Optional {
					return fmt.Errorf("%s expects state %s: %s, but this app's state %s (line %d) is %s",
						where(ex), want.Name, stateType(want), want.Name, have.Line, stateType(have))
				}
				continue
			}
			if syn, ok := synthState[want.Name]; ok {
				if syn.Type != want.Type || syn.Optional != want.Optional {
					return fmt.Errorf("%s expects state %s: %s, but another module expects %s: %s — one host cell cannot be both",
						where(ex), want.Name, stateType(want), want.Name, stateType(syn))
				}
				continue
			}
			syn := &ast.State{Name: want.Name, Type: want.Type, Elem: want.Elem, List: want.List, Optional: want.Optional,
				Default: want.Default, Placement: "client", Line: want.Line}
			synthState[want.Name] = syn
			app.States = append(app.States, syn)

		case ex.Policy != nil:
			want := ex.Policy
			if have, ok := policies[want.Name]; ok {
				if sig := policySignature(have); sig != policySignature(want) {
					return fmt.Errorf("%s expects policy %s, but this app's policy (line %d) is %s — the fragment's `requires` passes the arguments it expected",
						where(ex), policySignature(want), have.Line, sig)
				}
				continue
			}
			if syn, ok := synthPolicy[want.Name]; ok {
				if policySignature(syn) != policySignature(want) {
					return fmt.Errorf("%s expects policy %s, but another module expects %s — one host policy cannot satisfy both",
						where(ex), policySignature(want), policySignature(syn))
				}
				continue
			}
			// Who passes is the host's rule; standalone the guard admits
			// everyone, so the fragment's actions run under it as declared.
			syn := &ast.Policy{Name: want.Name, Params: want.Params, Expr: ast.Lit{Kind: "bool", Val: true}, Line: want.Line}
			synthPolicy[want.Name] = syn
			app.Policies = append(app.Policies, syn)

		case ex.Event != nil:
			want := ex.Event
			if typ, ok := carried[want.Name]; ok {
				if typ != want.Type {
					return fmt.Errorf("%s expects a stream to carry event %s as %s, but this app's stream carries %q as %s",
						where(ex), want.Name, want.Type, want.Name, typ)
				}
				continue
			}
			if typ, ok := expectedEvent[want.Name]; ok {
				if typ != want.Type {
					return fmt.Errorf("%s expects event %s: %s, but another module expects %s: %s — one host stream cannot carry both",
						where(ex), want.Name, want.Type, want.Name, typ)
				}
				continue
			}
			expectedEvent[want.Name] = want.Type
			app.ExpectedEvents = append(app.ExpectedEvents, *want)

		case ex.Route != "":
			if served[ex.Route] || expectedRoute[ex.Route] {
				continue
			}
			expectedRoute[ex.Route] = true
			app.ExpectedRoutes = append(app.ExpectedRoutes, ex.Route)
		}
	}
	app.Expects = nil
	return nil
}

func findField(e *ast.Entity, name string) *ast.EntityField {
	for i := range e.Fields {
		if e.Fields[i].Name == name {
			return &e.Fields[i]
		}
	}
	return nil
}

// fieldType renders a field's type the way it is written (`text`, `text?`).
func fieldType(f ast.EntityField) string {
	if f.Optional {
		return f.Type + "?"
	}
	return f.Type
}

// policySignature renders a policy's signature as written: `owns(id: int)`.
func policySignature(p *ast.Policy) string {
	parts := make([]string, len(p.Params))
	for i, prm := range p.Params {
		parts[i] = prm.Name + ": " + prm.Type
	}
	return p.Name + "(" + strings.Join(parts, ", ") + ")"
}

func stateType(s *ast.State) string {
	if s.Optional {
		return s.Type + "?"
	}
	return s.Type
}

// signatureOf renders an action's signature as written, for comparing and
// for the diagnostic: `unsubscribe(stream: int)`.
func signatureOf(a *ast.Action) string {
	parts := make([]string, len(a.Params))
	for i, p := range a.Params {
		t := p.Type
		if p.List {
			t = "[" + t + "]"
		}
		if p.Optional {
			t += "?"
		}
		parts[i] = p.Name + ": " + t
	}
	sig := a.Name + "(" + strings.Join(parts, ", ") + ")"
	if a.Ret != "" {
		if a.RetList {
			sig += " -> [" + a.Ret + "]"
		} else {
			sig += " -> " + a.Ret
		}
	}
	return sig
}
