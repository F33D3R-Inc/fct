package runtime

// Multi-tenancy — orgs/teams, memberships, and invitations, with a per-session
// *active tenant* the rest of the graph can see. Turn it on with
// FACET_MULTI_TENANT=1; the runtime then manages three reserved tables and a
// small set of reserved actions, and threads two new identity values into every
// evaluation scope:
//
//   - `tenant`     — the id of the session's active tenant (0 = none).
//   - `tenantRole` — the actor's role *within* that tenant ("owner"/"admin"/
//     "member", or "" if they are not a member).
//
// Those are ordinary builtin references, like `actor` and `role`, so an app
// expresses tenant isolation in its own policies — `policy sees(id): Doc(id).org
// == tenant` — and the authority enforces it on the same gate as everything else.
// Tenancy is membership management; the app still scopes its own rows by `tenant`,
// which is exactly the data the compiler already knows how to filter and index.

import (
	"net/http"
	"strings"
	"time"

	"facet/internal/ir"
)

const (
	tenantEntity     = "FacetTenant"
	membershipEntity = "FacetMembership"
	inviteEntity     = "FacetInvite"

	tenantRoleOwner  = "owner"
	tenantRoleAdmin  = "admin"
	tenantRoleMember = "member"

	inviteTTL = 7 * 24 * time.Hour // an invitation token's lifetime
)

// multiTenantEnabled reports whether tenancy is on (FACET_MULTI_TENANT=1).
func multiTenantEnabled() bool { return envOn("FACET_MULTI_TENANT") }

// tenantEntities are the reserved tables tenancy manages: organizations, the
// users that belong to each, and the outstanding invitations. `tenant` columns
// are plain ids (the runtime owns their lifecycle), so they carry no foreign-key
// cascade of their own.
func tenantEntities() []ir.Entity {
	return []ir.Entity{
		{Name: tenantEntity, Fields: []ir.Field{
			{Name: "id", Type: "int"},
			{Name: "name", Type: "text"},
			{Name: "slug", Type: "text"},
			{Name: "owner", Type: "text"},
			{Name: "created", Type: "int"},
		}},
		{Name: membershipEntity, Fields: []ir.Field{
			{Name: "id", Type: "int"},
			{Name: "tenant", Type: "int"},
			{Name: "username", Type: "text"},
			{Name: "role", Type: "text"},
			{Name: "created", Type: "int"},
		}},
		{Name: inviteEntity, Fields: []ir.Field{
			{Name: "id", Type: "int"},
			{Name: "tenant", Type: "int"},
			{Name: "email", Type: "text"},
			{Name: "token", Type: "text"},
			{Name: "role", Type: "text"},
			{Name: "inviter", Type: "text"},
			{Name: "accepted", Type: "bool"},
			{Name: "expires", Type: "int"},
		}},
	}
}

func isTenantAction(name string) bool {
	switch name {
	case "createTenant", "switchTenant", "inviteMember", "acceptInvite",
		"setMemberRole", "removeMember", "leaveTenant":
		return true
	}
	return false
}

// runTenantAction dispatches a reserved tenancy action. It refuses everything
// when tenancy is off, and otherwise routes by name. All mutate the reserved
// tables under the lock and persist through the store.
func (s *Server) runTenantAction(w http.ResponseWriter, r *http.Request, name string, args []any) {
	if !multiTenantEnabled() {
		http.Error(w, "multi-tenancy is not enabled (set FACET_MULTI_TENANT=1)", http.StatusNotImplemented)
		return
	}
	sid := s.session(w, r)
	switch name {
	case "createTenant":
		s.tenantCreate(w, sid, argStr(args, 0))
	case "switchTenant":
		s.tenantSwitch(w, sid, toInt(argAny(args, 0)))
	case "inviteMember":
		s.tenantInvite(w, sid, argStr(args, 0), argStr(args, 1))
	case "acceptInvite":
		s.tenantAccept(w, sid, argStr(args, 0))
	case "setMemberRole":
		s.tenantSetRole(w, sid, argStr(args, 0), argStr(args, 1))
	case "removeMember":
		s.tenantRemoveMember(w, sid, argStr(args, 0))
	case "leaveTenant":
		s.tenantLeave(w, sid)
	default:
		http.Error(w, "unknown tenant action", http.StatusNotFound)
	}
}

// activeTenant returns a session's active tenant id (0 when none). Caller holds
// s.mu. The active tenant rides in the session's own state under a reserved key,
// so it persists with the session (shared store) at no extra schema cost.
func activeTenant(ses *sessionState) int {
	if ses == nil {
		return 0
	}
	return toInt(ses.state["__tenant"])
}

// tenantRoleFor resolves the actor's role within a tenant, or "" if they are not
// a member (caller holds s.mu).
func (s *Server) tenantRoleFor(actor string, tid int) string {
	if tid == 0 || actor == "" || actor == roleGuest {
		return ""
	}
	for _, r := range s.entities[membershipEntity] {
		if m, ok := r.(record); ok && toInt(m["tenant"]) == tid && toStr(m["username"]) == actor {
			return toStr(m["role"])
		}
	}
	return ""
}

// membershipOf returns the membership row for an actor in a tenant, or nil
// (caller holds s.mu).
func (s *Server) membershipOf(actor string, tid int) record {
	for _, r := range s.entities[membershipEntity] {
		if m, ok := r.(record); ok && toInt(m["tenant"]) == tid && toStr(m["username"]) == actor {
			return m
		}
	}
	return nil
}

// tenantManager reports whether a role may administer a tenant (owner or admin).
func tenantManager(role string) bool { return role == tenantRoleOwner || role == tenantRoleAdmin }

func (s *Server) tenantCreate(w http.ResponseWriter, sid, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		http.Error(w, "a tenant name is required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	if actor == "" || actor == roleGuest {
		s.mu.Unlock()
		http.Error(w, "sign in first", http.StatusForbidden)
		return
	}
	tenant := s.insertReserved(tenantEntity, record{
		"name": name, "slug": slugify(name), "owner": actor, "created": int(time.Now().Unix()),
	})
	tid := toInt(tenant["id"])
	s.insertReserved(membershipEntity, record{
		"tenant": tid, "username": actor, "role": tenantRoleOwner, "created": int(time.Now().Unix()),
	})
	if ses := s.sessions[sid]; ses != nil {
		ses.state["__tenant"] = tid // make the new org the active one
	}
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(actor, "createTenant", true, name)
	writeJSON(w, map[string]any{"ok": true, "reload": true, "tenant": tenant})
}

func (s *Server) tenantSwitch(w http.ResponseWriter, sid string, tid int) {
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	if s.membershipOf(actor, tid) == nil {
		s.mu.Unlock()
		http.Error(w, "you are not a member of that tenant", http.StatusForbidden)
		return
	}
	if ses := s.sessions[sid]; ses != nil {
		ses.state["__tenant"] = tid
	}
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(actor, "switchTenant", true, itoa(tid))
	writeJSON(w, map[string]any{"ok": true, "reload": true})
}

func (s *Server) tenantInvite(w http.ResponseWriter, sid, email, role string) {
	if role == "" {
		role = tenantRoleMember
	}
	if role != tenantRoleMember && role != tenantRoleAdmin {
		http.Error(w, "role must be member or admin", http.StatusBadRequest)
		return
	}
	token := randomToken(24)
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid := activeTenant(s.sessions[sid])
	if !tenantManager(s.tenantRoleFor(actor, tid)) {
		s.mu.Unlock()
		s.recordAudit(actor, "inviteMember", false, "not a tenant manager")
		http.Error(w, "forbidden: only a tenant owner or admin may invite", http.StatusForbidden)
		return
	}
	s.insertReserved(inviteEntity, record{
		"tenant": tid, "email": strings.TrimSpace(email), "token": hashToken(token),
		"role": role, "inviter": actor, "accepted": false,
		"expires": int(time.Now().Add(inviteTTL).Unix()),
	})
	s.mu.Unlock()
	s.recordAudit(actor, "inviteMember", true, email+" -> "+role)
	// The token would be emailed in production; surfaced here so the flow is usable.
	writeJSON(w, map[string]any{"ok": true, "token": token})
}

func (s *Server) tenantAccept(w http.ResponseWriter, sid, token string) {
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	if actor == "" || actor == roleGuest {
		s.mu.Unlock()
		http.Error(w, "sign in first", http.StatusForbidden)
		return
	}
	var inv record
	for _, r := range s.entities[inviteEntity] {
		m, ok := r.(record)
		if ok && !truthy(m["accepted"]) && tokenEqual(token, toStr(m["token"])) &&
			int64(toInt(m["expires"])) > time.Now().Unix() {
			inv = m
			break
		}
	}
	if inv == nil {
		s.mu.Unlock()
		s.recordAudit(actor, "acceptInvite", false, "invalid token")
		http.Error(w, "invalid or expired invitation", http.StatusBadRequest)
		return
	}
	tid := toInt(inv["tenant"])
	if s.membershipOf(actor, tid) == nil {
		s.insertReserved(membershipEntity, record{
			"tenant": tid, "username": actor, "role": toStr(inv["role"]), "created": int(time.Now().Unix()),
		})
	}
	inv["accepted"] = true
	s.saveReserved(inviteEntity, inv)
	if ses := s.sessions[sid]; ses != nil {
		ses.state["__tenant"] = tid
	}
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(actor, "acceptInvite", true, itoa(tid))
	writeJSON(w, map[string]any{"ok": true, "reload": true})
}

func (s *Server) tenantSetRole(w http.ResponseWriter, sid, username, role string) {
	if role != tenantRoleMember && role != tenantRoleAdmin && role != tenantRoleOwner {
		http.Error(w, "role must be owner, admin, or member", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid := activeTenant(s.sessions[sid])
	if !tenantManager(s.tenantRoleFor(actor, tid)) {
		s.mu.Unlock()
		http.Error(w, "forbidden: only a tenant owner or admin may set roles", http.StatusForbidden)
		return
	}
	m := s.membershipOf(username, tid)
	if m == nil {
		s.mu.Unlock()
		http.Error(w, "no such member", http.StatusNotFound)
		return
	}
	m["role"] = role
	s.saveReserved(membershipEntity, m)
	s.mu.Unlock()
	s.recordAudit(actor, "setMemberRole", true, username+" -> "+role)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) tenantRemoveMember(w http.ResponseWriter, sid, username string) {
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid := activeTenant(s.sessions[sid])
	if !tenantManager(s.tenantRoleFor(actor, tid)) {
		s.mu.Unlock()
		http.Error(w, "forbidden: only a tenant owner or admin may remove members", http.StatusForbidden)
		return
	}
	m := s.membershipOf(username, tid)
	if m == nil {
		s.mu.Unlock()
		http.Error(w, "no such member", http.StatusNotFound)
		return
	}
	if toStr(m["role"]) == tenantRoleOwner {
		s.mu.Unlock()
		http.Error(w, "the tenant owner cannot be removed", http.StatusBadRequest)
		return
	}
	s.removeReserved(membershipEntity, toInt(m["id"]))
	s.mu.Unlock()
	s.recordAudit(actor, "removeMember", true, username)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) tenantLeave(w http.ResponseWriter, sid string) {
	s.mu.Lock()
	actor, _ := s.actorOf(sid)
	tid := activeTenant(s.sessions[sid])
	m := s.membershipOf(actor, tid)
	if m == nil {
		s.mu.Unlock()
		http.Error(w, "you are not a member of the active tenant", http.StatusBadRequest)
		return
	}
	if toStr(m["role"]) == tenantRoleOwner {
		s.mu.Unlock()
		http.Error(w, "the owner cannot leave; transfer ownership or delete the tenant first", http.StatusBadRequest)
		return
	}
	s.removeReserved(membershipEntity, toInt(m["id"]))
	if ses := s.sessions[sid]; ses != nil {
		ses.state["__tenant"] = 0
	}
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(actor, "leaveTenant", true, itoa(tid))
	writeJSON(w, map[string]any{"ok": true, "reload": true})
}

// slugify renders a tenant name as a url-safe slug.
func slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
