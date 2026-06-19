package runtime

// Audit log — an append-only record of every server action: who ran what, when,
// and whether the permission gate allowed it. It is the tamper-evident trail an
// operator (or a compliance review) reads to answer "who did this?". Entries are
// kept in a fast in-memory ring for the admin endpoint and written through to a
// durable table (seeded back into the ring on restart).

import (
	"net/http"
	"sync"
	"time"
)

// auditEntry is one logged action invocation.
type auditEntry struct {
	Time    int64  `json:"time"`             // unix seconds
	Actor   string `json:"actor"`            // who acted
	Action  string `json:"action"`           // the action name
	Allowed bool   `json:"allowed"`          // did the policy gate allow it
	Detail  string `json:"detail,omitempty"` // e.g. the denying policy
}

// auditLog is the in-memory ring plus a write-through to durable storage.
type auditLog struct {
	mu      sync.Mutex
	ring    []auditEntry
	size    int
	persist func(auditEntry) // best-effort durable write; nil disables it
}

func newAuditLog(persist func(auditEntry)) *auditLog {
	return &auditLog{size: 1000, persist: persist}
}

// record appends an entry to the ring and writes it through durably.
func (a *auditLog) record(e auditEntry) {
	a.mu.Lock()
	a.ring = append(a.ring, e)
	if len(a.ring) > a.size {
		a.ring = append(a.ring[:0:0], a.ring[len(a.ring)-a.size:]...)
	}
	a.mu.Unlock()
	if a.persist != nil {
		a.persist(e)
	}
}

// seed loads durable history into the ring (oldest first) at startup.
func (a *auditLog) seed(entries []auditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ring = append(a.ring[:0], entries...)
	if len(a.ring) > a.size {
		a.ring = a.ring[len(a.ring)-a.size:]
	}
}

// recent returns up to limit entries, newest first.
func (a *auditLog) recent(limit int) []auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.ring)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]auditEntry, limit)
	copy(out, a.ring[n-limit:])
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// recordAudit logs one action invocation (no-op if auditing is disabled).
func (s *Server) recordAudit(actor, action string, allowed bool, detail string) {
	if s.audit == nil {
		return
	}
	s.audit.record(auditEntry{
		Time:    time.Now().Unix(),
		Actor:   actor,
		Action:  action,
		Allowed: allowed,
		Detail:  detail,
	})
}

// handleAudit serves the admin-only audit feed: GET /api/_audit?limit=100,
// newest first. Only an admin session may read it.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	sid := s.session(w, r)
	if !s.isAdmin(sid) {
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := atoiPositive(v); err == nil {
			limit = n
		}
	}
	writeJSON(w, map[string]any{"entries": s.audit.recent(limit)})
}

// isAdmin reports whether a session's actor holds the admin role.
func (s *Server) isAdmin(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ses := s.sessions[sid]
	return ses != nil && ses.role == "admin"
}
