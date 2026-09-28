package runtime

import "facet/internal/ir"

// argFor coerces one argument to the action parameter it binds. A scalar or
// scalar-list parameter is paramArg's; a parameter typed by a wire `type`
// (`ev: PlayEventIn`, or a list of them, `events: [PlayEventIn]` — a JSON
// body's array of objects) is shaped to that type: each element must be an
// object, and each declared field is coerced to its declared type the way a
// parameter of that type would be (an absent field is its zero, a nested
// wire-typed field is shaped in turn). Anything else refuses the argument,
// like a malformed scalar.
func (s *Server) argFor(v any, p ir.Param) (any, bool) {
	wt := s.wireTypeNamed(p.Type)
	if wt == nil {
		return paramArg(v, p)
	}
	if !p.List {
		if v == nil {
			return nil, true
		}
		return s.wireValue(v, wt)
	}
	var items []any
	switch t := v.(type) {
	case nil:
		return []any{}, true
	case []any:
		items = t
	case []map[string]any:
		for _, m := range t {
			items = append(items, m)
		}
	default:
		return nil, false
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		cv, ok := s.wireValue(it, wt)
		if !ok {
			return nil, false
		}
		out = append(out, cv)
	}
	return out, true
}

// wireValue shapes one object to wire type wt's declared fields.
func (s *Server) wireValue(v any, wt *ir.WireType) (any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(wt.Fields))
	for _, f := range wt.Fields {
		cv, ok := s.argFor(m[f.Name], ir.Param{Name: f.Name, Type: f.Type, List: f.List, Optional: f.Optional})
		if !ok {
			return nil, false
		}
		out[f.Name] = cv
	}
	return out, true
}

// wireTypeNamed is the app's `type` declaration called name, nil when there
// is none.
func (s *Server) wireTypeNamed(name string) *ir.WireType {
	if s.ir == nil {
		return nil
	}
	for i := range s.ir.Types {
		if s.ir.Types[i].Name == name {
			return &s.ir.Types[i]
		}
	}
	return nil
}
