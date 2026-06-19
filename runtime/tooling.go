package runtime

import (
	"fmt"
	"net/http"
	"time"

	"facet/internal/ir"
)

// This file is the programmatic surface the developer-experience tools drive the
// runtime through — `facet console`, `facet seed`, and `facet test`. They build a
// server (usually NewInMemory), then run actions, read entities, and evaluate
// expressions against it exactly as the web and API projections do, so a tool
// observes the same behavior a deployed app would.

// toolSID is the single synthetic session the tooling acts within.
const toolSID = "tool"

// toolSession returns the singleton tool session, creating it with every state
// cell (client and server) at its declared default — so both server- and
// client-placed actions, and ad-hoc expressions, can read and write them. The
// given identity (actor/role/verified) is applied on every call.
func (s *Server) toolSession(actor, role string, verified bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.sessions[toolSID]
	if ses == nil {
		state := map[string]any{}
		for _, st := range s.ir.States {
			state[st.Name] = eval(st.Init, map[string]any{})
		}
		ses = &sessionState{state: state, expires: time.Now().Add(sessionTTL)}
		s.sessions[toolSID] = ses
	}
	if actor == "" {
		actor = "guest"
	}
	if role == "" {
		role = "guest"
	}
	ses.actor, ses.role, ses.verified = actor, role, verified
}

// Run executes an action under the tool session with the given identity. It
// returns the per-session scalar deltas, or an error carrying the authority's
// rejection (a failed policy `requires` or an input `check`). Server- and
// client-placed actions both run; auth's built-in actions do not (they need the
// real login flow).
func (s *Server) Run(actor, role string, verified bool, action string, args []any) (map[string]any, error) {
	s.toolSession(actor, role, verified)
	if s.ir.Auth && isAuthAction(action) {
		return nil, fmt.Errorf("built-in auth action %q is not runnable here; exercise it through the web/API login flow", action)
	}
	act := s.byAction[action]
	if act == nil {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	deltas, status, msg := s.runAction(toolSID, act, args)
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s", msg)
	}
	return deltas, nil
}

// EvalExpr evaluates a compiled expression against the tool session: the entity
// working set plus the session's state and identity.
func (s *Server) EvalExpr(e *ir.Expr, actor, role string, verified bool) any {
	s.toolSession(actor, role, verified)
	s.mu.Lock()
	defer s.mu.Unlock()
	return eval(e, s.scope(toolSID))
}

// StateValue returns the current value of a state cell in the tool session.
func (s *Server) StateValue(name string) any {
	s.toolSession("guest", "guest", false)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ses := s.sessions[toolSID]; ses != nil {
		return ses.state[name]
	}
	return nil
}

// EntityRows returns a snapshot of an entity's in-memory working set.
func (s *Server) EntityRows(name string) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any{}, s.entities[name]...)
}

// EntityNames lists the app's entity names (excluding the reserved user table).
func (s *Server) EntityNames() []string {
	var out []string
	for _, e := range s.ir.Entities {
		if e.Name == reservedUserEntity {
			continue
		}
		out = append(out, e.Name)
	}
	return out
}

// AddRow inserts a row into an entity and writes it through to the store (used by
// `facet seed`). If the row carries an explicit "id" it is honored (so seeded
// relations line up); otherwise the next id is assigned. The id counter advances
// past any explicit id so later inserts never collide.
func (s *Server) AddRow(entity string, fields map[string]any) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.nextID[entity]; !ok {
		return 0, fmt.Errorf("unknown entity %q", entity)
	}
	row := record{}
	for k, v := range fields {
		row[k] = v
	}
	id := toInt(row["id"])
	if id <= 0 {
		s.nextID[entity]++
		id = s.nextID[entity]
	} else if id > s.nextID[entity] {
		s.nextID[entity] = id
	}
	row["id"] = id
	s.entities[entity] = append(s.entities[entity], row)
	s.commit([]durOp{{kind: "save", entity: entity, row: row}})
	return id, nil
}

// IR exposes the compiled graph backing this server (read-only), for tools that
// introspect actions, entities, and derives.
func (s *Server) IR() *ir.IR { return s.ir }
