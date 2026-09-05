package runtime

// Field-level authorization. An entity field marked `@requires(policy)` is gated
// on the data projections clients consume: the JSON API serves it only to an actor
// the (zero-argument) policy admits, and it never travels over the shared SSE
// stream — so a gated field reaches a client only through the per-actor API. The
// server's own logic (actions, policies, server-side rendering) sees full rows;
// the server is the authority. Use a route/region guard to hide a field in a
// server-rendered view.

// gatedField is one entity field gated by a read policy.
type gatedField struct {
	name   string
	policy string
}

// indexGatedFields records, per entity, the fields carrying a `@requires` gate, so
// the projection layer can find them in O(1). Built once at startup.
func (s *Server) indexGatedFields() {
	for _, e := range s.ir.Entities {
		for _, f := range e.Fields {
			if f.ReadPolicy != "" {
				s.gated[e.Name] = append(s.gated[e.Name], gatedField{name: f.Name, policy: f.ReadPolicy})
			}
		}
	}
}

// stripFields returns a copy of an entity's rows with the named fields removed. A
// non-record row passes through; an empty drop set returns the rows untouched (no
// copy), so non-gated entities pay nothing.
func stripFields(rows any, drop map[string]bool) any {
	if len(drop) == 0 {
		return rows
	}
	list, ok := rows.([]any)
	if !ok {
		return rows
	}
	out := make([]any, len(list))
	for i, r := range list {
		rec, ok := r.(record)
		if !ok {
			out[i] = r
			continue
		}
		c := make(record, len(rec))
		for k, v := range rec {
			if !drop[k] {
				c[k] = v
			}
		}
		out[i] = c
	}
	return out
}

// sseSafe strips every gated field from a delta map. The SSE broadcast reaches all
// subscribers with no actor to authorize against, so gated fields are never
// streamed — clients receive them only over the per-actor API.
//
// It is `visibleRows` with no actor rather than a rule of its own: "what may
// this actor receive of this entity" has one implementation, and an empty scope
// fails every gate, which is the right answer for a payload that cannot name
// who is reading it.
func (s *Server) sseSafe(deltas map[string]any) map[string]any {
	if len(s.gated) == 0 {
		return deltas
	}
	out := make(map[string]any, len(deltas))
	for ent, rows := range deltas {
		out[ent] = s.visibleRows(ent, rows, nil)
	}
	return out
}

// gateForActor returns the gated fields of an entity the scope's actor may NOT see
// (their read policy fails), so the API can drop just those fields per request.
func (s *Server) gateForActor(ent string, scope map[string]any) map[string]bool {
	g := s.gated[ent]
	if len(g) == 0 {
		return nil
	}
	drop := map[string]bool{}
	for _, gf := range g {
		pol := s.byPolicy[gf.policy]
		if pol == nil || !truthy(eval(pol.Expr, scope)) {
			drop[gf.name] = true
		}
	}
	return drop
}
