package runtime

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"facet/internal/ir"
)

// ── who may read an entity over the JSON API ────────────────────────────────
//
// `GET /api/<Entity>` used to answer every question anyone asked of a table. No
// actor was resolved, no policy was consulted, and `buildAPIQuery` compiled the
// caller's own query string straight into an indexed SELECT. An app whose
// row-level read rule lives — as it must, since this language has no
// entity-level read policy — in the `where` clause of its views therefore had
// that rule enforced on the rendered page and nowhere else: an unauthenticated
// stranger asking for /api/Post received every draft, body and all.
//
// The rule this file establishes is that a projection may not invent authority
// the application never granted. The web projection serves the rows a view's
// `where` selected. The JSON projection has no `where` and nowhere to put one,
// so for an entity whose read is not declared there is no honest answer except
// to refuse.
//
// FAIL CLOSED, AND WHY IT HAS TO BE THIS WAY ROUND. The two defaults are not
// symmetrical. Serving by default is a hole no application can close, because
// the language gives an author no words to close it — the journal app wrote its
// visibility rule five times over in its views, correctly, and was still handing
// out drafts. Refusing by default is an inconvenience any application can lift
// with one deliberate, auditable statement. A framework's default is the answer
// it gives to the app that never thought about the question, and "everything, to
// everyone" is not an answer a framework may give to that app.
//
// THE STATEMENT, TODAY, IS `FACET_API_READ`. It is a comma-separated list:
//
//	FACET_API_READ=Product,Category        rows are public — anyone may list them
//	FACET_API_READ=Person:admins           served only to an actor `admins` admits
//	FACET_API_READ=*                       every declared entity, unconditionally
//
// A guard names a ZERO-ARGUMENT policy — the same shape a route guard and a
// field gate take: "may this actor query this collection at all". It composes
// with, and is orthogonal to, the entity's own `read:` clause (ast.Entity.Read)
// when it declares one: `read:` is the per-ROW rule the leak actually called
// for — "an actor may read this Post if it is published or they wrote it" —
// applied to every row a query would otherwise have returned (see
// Server.apiQueryRows). An entity published here with no `read:` clause still
// means what it always meant: the whole of it, unconditionally (or gated by
// `guard` alone). This setting is deliberately configuration rather than a
// naming convention over policies: a convention that silently publishes an
// entity because someone named a policy a certain way would be the same class
// of accident as the default this replaces. And it stays a required,
// deliberate step even for an entity with `read:` declared — the language can
// express the per-row rule now, but whether a collection is reachable over
// `/api` AT ALL is still this file's question to answer, not something a
// `read:` clause implies on its own.
//
// Publishing an entity does not undo any other gate. The rows still pass through
// `visibleRows`, so a `@requires` field is stripped for an actor its policy
// denies exactly as before, and a reserved entity (the credential store) is
// never routable at all.

// apiReadEnv is the setting that publishes entities over the JSON API.
const apiReadEnv = "FACET_API_READ"

// entityRead is the resolved read rule for one published entity: its rows are
// served, and — when guard is non-empty — only to an actor that zero-argument
// policy admits.
//
// The per-row filter this struct used to have no way to express now lives on
// the entity itself: ir.Entity.Read, an app's `read:` clause, evaluated per row
// exactly the way a view's `where` already is (Server.apiQueryRows binds the
// row, evaluates it, drops the row it rejects, and keeps pulling store pages
// until `limit` is met — precisely what listRows does for a `for` region with a
// residual predicate). This struct still only says whether the collection may
// be asked about at all, and by whom; the per-row question, when the entity
// declares one, is answered downstream of here, at query time.
type entityRead struct {
	guard string // zero-argument policy the actor must pass, or "" for public
}

// apiReadFromEnv resolves the FACET_API_READ setting against the app's entities
// and policies. It returns the rules it could resolve plus one message per entry
// it could not, which the server logs at startup: an entry naming an entity or a
// policy that does not exist is a typo, and because the default is closed a typo
// leaves the entity closed rather than open — the failure mode a
// misconfiguration should have.
func apiReadFromEnv(spec string, ents []ir.Entity, policies map[string]*ir.Policy) (map[string]entityRead, []string) {
	rules := map[string]entityRead{}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return rules, nil
	}

	declared := map[string]bool{}
	for _, e := range ents {
		if !isReservedEntity(e.Name) {
			declared[e.Name] = true // a reserved table is never routable, so it is never publishable
		}
	}

	var problems []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			for name := range declared {
				rules[name] = entityRead{}
			}
			continue
		}
		name, guard := part, ""
		if c := strings.IndexByte(part, ':'); c >= 0 {
			name, guard = strings.TrimSpace(part[:c]), strings.TrimSpace(part[c+1:])
		}
		if !declared[name] {
			problems = append(problems, fmt.Sprintf(
				"%s names %q, which is not an entity of this app; it stays closed", apiReadEnv, name))
			continue
		}
		if guard != "" {
			pol, ok := policies[guard]
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"%s guards %s with policy %q, which this app does not declare; %s stays closed",
					apiReadEnv, name, guard, name))
				continue
			}
			if len(pol.Params) != 0 {
				problems = append(problems, fmt.Sprintf(
					"%s guards %s with row-level policy %q, which takes arguments the entity list cannot supply; "+
						"an entity read guard must be a zero-argument policy, so %s stays closed",
					apiReadEnv, name, guard, name))
				continue
			}
		}
		rules[name] = entityRead{guard: guard}
	}
	return rules, problems
}

// apiMayRead answers whether this caller may list an entity's rows over the JSON
// API and, when they may not, the reason to hand back. An unpublished entity's
// message names the declaration that is missing, because a developer meeting
// this refusal has to be able to tell "I forgot to publish it" from "the actor
// is not allowed".
func (s *Server) apiMayRead(entity string, scope map[string]any) (bool, string) {
	rule, published := s.apiRead[entity]
	if !published {
		return false, fmt.Sprintf(
			"%s is not published over the JSON API: no read authorization is declared for it, and an entity list "+
				"served without one hands every row to every caller. Publish it with %s=%s (rows are public) or "+
				"%s=%s:<zero-argument policy> (rows are served only to an actor that policy admits).",
			entity, apiReadEnv, entity, apiReadEnv, entity)
	}
	if rule.guard == "" {
		return true, ""
	}
	pol := s.byPolicy[rule.guard]
	if pol == nil || !truthy(eval(pol.Expr, scope)) {
		return false, "forbidden: " + rule.guard
	}
	return true, ""
}

// apiPublished is the sorted set of entities this app publishes, reported by the
// schema endpoint so a client can see which collections it may fetch instead of
// discovering it one 403 at a time.
func (s *Server) apiPublished() []string {
	out := make([]string, 0, len(s.apiRead))
	for name := range s.apiRead {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sidForRequest resolves a request's signed session cookie to a live session id,
// read-only: a missing, unsigned, garbage, or expired session resolves to "",
// which every caller here treats as the guest identity. Unlike `session` it
// never mints one — right for any channel with no honest place to set a cookie
// from (a machine-facing GET, or a long-lived SSE response), where minting would
// turn an unauthenticated read into a memory write anyone could repeat just by
// asking.
//
// apiScope and handleLive both resolve "who is asking" through this one
// function — the API's per-request read and the live stream's per-connection
// read must agree on what a session cookie means, or a subscriber and the same
// person's own `GET /api/<Entity>` could end up authorized differently for
// identical identities.
func (s *Server) sidForRequest(r *http.Request) string {
	// A native client carries the session as `Authorization: Bearer <token>`,
	// where the token is exactly the signed value the cookie would carry (the
	// login reply hands it out as `token`); a browser carries the cookie.
	raw := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimSpace(h[len("Bearer "):])
	} else if c, err := r.Cookie("fa_sid"); err == nil {
		raw = c.Value
	}
	if raw == "" {
		return ""
	}
	sid, ok := verifySigned(raw)
	if !ok {
		return ""
	}
	s.mu.Lock()
	ses, live := s.sessions[sid]
	// Stateless servers: a request can land on a cold instance, so rehydrate the
	// session from the shared store on a local cache miss (as `session` does).
	if !live && s.cluster != nil {
		if ps, found, err := s.store.LoadSession(sid); err == nil && found {
			ses = sessionFromPersisted(ps)
			s.sessions[sid] = ses
			live = true
		}
	}
	now := time.Now()
	if !live || !now.Before(ses.expires) {
		s.mu.Unlock()
		return ""
	}
	// A signed-in caller slides its session forward exactly as Server.session
	// does for a cookie, so a native client presenting `Bearer` stays signed in
	// while it is active. Only an existing, signed-in session is touched: a
	// guest's or an anonymous read writes nothing.
	slid := ses.actor != "" && ses.actor != roleGuest
	if slid {
		ses.expires = now.Add(sessionTTL)
	}
	s.mu.Unlock()
	if slid {
		s.persistSession(sid)
	}
	return sid
}

// apiScope resolves the JSON API's caller to an evaluation scope, from the same
// signed session cookie the web channel reads — the API is a projection of one
// application, not a second application with its own idea of who is asking.
//
// It is deliberately read-only where `session` is not: it never mints a session
// for a caller that did not present one. `session` writes a row into s.sessions
// and sets a cookie on every cookieless request, which is right for a browser
// arriving at a page and wrong for a machine-facing GET, where it would make an
// unauthenticated read a memory write anyone could repeat. A caller with no
// valid session is a guest, and a guest scope is what the policies are then
// evaluated against.
func (s *Server) apiScope(r *http.Request) map[string]any {
	sid := s.sidForRequest(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	// sid == "" finds no sessionState and so builds the guest identity, over the
	// same entity set — the same fallback `sidForRequest`'s own doc describes.
	return s.scope(sid)
}

// apiReadEnvValue is the raw setting, read once at construction.
func apiReadEnvValue() string { return os.Getenv(apiReadEnv) }
