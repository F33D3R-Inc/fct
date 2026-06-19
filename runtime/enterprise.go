package runtime

// Phase 6 — the enterprise platform. Everything here is another runtime service
// threaded through the same IR + placement model, exactly like the earlier
// phases: a reserved table the store already knows how to build, a reserved
// action the existing event/API channels already know how to dispatch, an HTTP
// handler hung off the same mux. Nothing forks the language.
//
//   - Multi-tenancy (tenancy.go)   — orgs/teams, memberships, invitations, and a
//     per-session active tenant exposed to the graph as `tenant`/`tenantRole`.
//   - Auto-admin (admin.go)        — a generated, admin-only CRUD dashboard over
//     every entity, Django-admin style, at /admin.
//   - Billing (billing.go)         — a subscription + usage ledger with a signed
//     provider webhook to sync state.
//   - Compliance (compliance.go)   — i18n message catalogs, GDPR data
//     export/erasure, and declarative retention sweeps.
//   - More targets (mobile.go)     — `facet generate` emits native mobile clients
//     (Swift / Kotlin / TypeScript) that read the same IR over the /api projection.

import (
	"net/http"
	"os"
	"strings"

	"facet/internal/ir"
)

// reservedEntities are the tables the runtime manages itself — the credential
// store plus the Phase 6 tenancy and billing ledgers. They are created on demand
// (only when their feature is enabled), and they are always hidden from the
// public JSON API and the live SSE stream, the same way the user table is: an
// admin reads them through /admin and the dedicated endpoints, never as ambient
// app data.
var reservedEntities = map[string]bool{
	reservedUserEntity: true,
	tenantEntity:       true,
	membershipEntity:   true,
	inviteEntity:       true,
	subscriptionEntity: true,
	usageEntity:        true,
}

// isReservedEntity reports whether an entity is runtime-managed and must never
// be projected to a client (API or live stream) as ordinary application data.
func isReservedEntity(name string) bool { return reservedEntities[name] }

// injectEnterpriseEntities appends the reserved tables for whichever enterprise
// features are enabled to the IR, before the store loads the working set. They
// ride the same migration/load path as a declared entity, so the store builds
// real columns for them with no special case. Called once, at server
// construction, while the graph is still mutable for this process.
func injectEnterpriseEntities(graph *ir.IR) {
	have := map[string]bool{}
	for _, e := range graph.Entities {
		have[e.Name] = true
	}
	add := func(ents []ir.Entity) {
		for _, e := range ents {
			if !have[e.Name] {
				graph.Entities = append(graph.Entities, e)
				have[e.Name] = true
			}
		}
	}
	if multiTenantEnabled() {
		add(tenantEntities())
	}
	if billingEnabled() {
		add(billingEntities())
	}
}

// runReserved dispatches the reserved Phase 6 actions that the tenancy and
// billing features provide over the same channels (/event, POST /api/<action>)
// that carry auth and ordinary actions. It returns true when it handled the
// request. Reserved actions are runtime-provided (not in the compiled IR), so a
// view cannot bind a button to one — they are driven by the generated admin
// console, a mobile/API client, or the dedicated endpoints — and each reports a
// clear error when its feature is disabled.
func (s *Server) runReserved(w http.ResponseWriter, r *http.Request, name string, args []any) bool {
	switch {
	case isTenantAction(name):
		s.runTenantAction(w, r, name, args)
		return true
	case isBillingAction(name):
		s.runBillingAction(w, r, name, args)
		return true
	}
	return false
}

// EnterpriseDescription summarizes the active enterprise posture for the startup
// banner: which Phase 6 services this process is running.
func EnterpriseDescription() string {
	parts := []string{}
	if multiTenantEnabled() {
		parts = append(parts, "multi-tenant")
	}
	if adminEnabled() {
		parts = append(parts, "auto-admin")
	}
	parts = append(parts, "GDPR export/erase")
	if billingEnabled() {
		parts = append(parts, "billing")
	}
	if os.Getenv("FACET_RETENTION") != "" {
		parts = append(parts, "retention")
	}
	if os.Getenv("FACET_I18N_DIR") != "" {
		parts = append(parts, "i18n")
	}
	if len(parts) == 0 {
		return "single-tenant"
	}
	return strings.Join(parts, " · ")
}

// insertReserved adds a row to a reserved (runtime-managed) entity: it assigns
// the next id, appends to the in-memory working set, and writes through to the
// durable store. The caller holds s.mu. It deliberately does not broadcast — a
// reserved table is private (per-tenant memberships, subscriptions), never fanned
// out to every live client.
func (s *Server) insertReserved(entity string, row record) record {
	s.nextID[entity]++
	row["id"] = s.nextID[entity]
	s.entities[entity] = append(s.entities[entity], row)
	s.persist(s.store.Save(entity, row))
	return row
}

// saveReserved persists an in-place update to a reserved entity row (caller holds
// s.mu).
func (s *Server) saveReserved(entity string, row record) {
	s.persist(s.store.Save(entity, row))
}

// removeReserved deletes a reserved entity row by id from the working set and the
// store (caller holds s.mu).
func (s *Server) removeReserved(entity string, id int) {
	rows := s.entities[entity]
	for i, r := range rows {
		if m, ok := r.(record); ok && toInt(m["id"]) == id {
			s.entities[entity] = append(rows[:i], rows[i+1:]...)
			s.persist(s.store.Delete(entity, id))
			return
		}
	}
}

// envOn reports whether an environment flag is switched on ("1" or "true").
func envOn(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "1" || v == "true" || v == "on" || v == "yes"
}

// argAny reads the i-th argument as-is, or nil if absent.
func argAny(args []any, i int) any {
	if i < len(args) {
		return args[i]
	}
	return nil
}

// actorOf returns the signed-in identity of a session (caller holds s.mu).
func (s *Server) actorOf(sid string) (actor, role string) {
	if ses := s.sessions[sid]; ses != nil {
		return ses.actor, ses.role
	}
	return "guest", "guest"
}
