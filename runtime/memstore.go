package runtime

import (
	"context"
	"sort"
	"sync"
	"time"

	"facet/internal/ir"
)

// memStore is a complete, in-memory implementation of Store. It exists so the
// developer-experience tooling — `facet test`, `facet console`, and `facet seed
// --dry` — runs with no database at all: an app's behavior (actions, policies,
// queries, jobs) is exercised against the same runtime, just with a volatile
// backend. It is not used to serve production traffic; `facet run` always uses
// Postgres. Every method mirrors the Postgres backend's contract closely enough
// that the runtime above it cannot tell the difference.
type memStore struct {
	mu   sync.Mutex
	ents map[string]ir.Entity
	// children is the reverse-relation graph, parent entity -> the relations that
	// point at it, rebuilt by Init from ir.References. It is the same derivation
	// the runtime and fqStore use, so all three cascade along exactly one graph.
	children map[string][]ir.Reference
	rows     map[string][]any // entity -> rows (records)
	sessions map[string]*persistedSession
	audit    []auditEntry
	jobs     []*durableJob
	nextJob  int64
	crons    map[string]time.Time
}

func newMemStore() *memStore {
	return &memStore{
		ents:     map[string]ir.Entity{},
		children: map[string][]ir.Reference{},
		rows:     map[string][]any{},
		sessions: map[string]*persistedSession{},
		crons:    map[string]time.Time{},
	}
}

func (s *memStore) Init(entities []ir.Entity) (map[string][]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entities {
		s.ents[e.Name] = e
		if s.rows[e.Name] == nil {
			s.rows[e.Name] = []any{}
		}
	}
	// Init may be called more than once (the dev reload re-enters it), so the
	// graph is rebuilt from every entity known so far rather than appended to.
	s.children = map[string][]ir.Reference{}
	for _, r := range ir.References(sortedEntities(s.ents)) {
		s.children[r.Parent] = append(s.children[r.Parent], r)
	}
	out := map[string][]any{}
	for name, rs := range s.rows {
		out[name] = append([]any{}, rs...)
	}
	return out, nil
}

func (s *memStore) Save(entity string, row map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := toInt(row["id"])
	rows := s.rows[entity]
	for i, r := range rows {
		if m, ok := r.(record); ok && toInt(m["id"]) == id {
			rows[i] = copyRecord(row)
			return nil
		}
	}
	s.rows[entity] = append(rows, copyRecord(row))
	return nil
}

func (s *memStore) Delete(entity string, id any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCascade(entity, map[int]bool{toInt(id): true})
	return nil
}

// deleteCascade removes the given parent rows and, following the reverse-relation
// graph, every child row that referenced them — the same effect as the database's
// ON DELETE CASCADE.
func (s *memStore) deleteCascade(entity string, ids map[int]bool) {
	rows := s.rows[entity]
	kept := rows[:0:0]
	for _, r := range rows {
		if m, ok := r.(record); ok && ids[toInt(m["id"])] {
			continue
		}
		kept = append(kept, r)
	}
	s.rows[entity] = kept
	for _, ch := range s.children[entity] {
		childIDs := map[int]bool{}
		for _, r := range s.rows[ch.Entity] {
			if m, ok := r.(record); ok && ids[toInt(m[ch.Field])] {
				childIDs[toInt(m["id"])] = true
			}
		}
		if len(childIDs) > 0 {
			s.deleteCascade(ch.Entity, childIDs)
		}
	}
}

// sortedEntities is the entity map as a slice in name order, so a derivation over
// it (ir.References) does not depend on Go's randomized map iteration.
func sortedEntities(ents map[string]ir.Entity) []ir.Entity {
	names := make([]string, 0, len(ents))
	for name := range ents {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ir.Entity, 0, len(names))
	for _, name := range names {
		out = append(out, ents[name])
	}
	return out
}

func (s *memStore) Clear(entity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := map[int]bool{}
	for _, r := range s.rows[entity] {
		if m, ok := r.(record); ok {
			ids[toInt(m["id"])] = true
		}
	}
	s.deleteCascade(entity, ids)
	return nil
}

// Query reproduces the pushed-down read in memory: filter by the predicate, order
// (with an id tiebreak), apply the keyset cursor, and cap to the page size,
// returning the page plus a cursor for the next one.
func (s *memStore) Query(query Query) ([]any, string, error) {
	s.mu.Lock()
	scopeRows := map[string][]any{}
	for k, v := range s.rows {
		scopeRows[k] = append([]any{}, v...)
	}
	src := append([]any{}, s.rows[query.Entity]...)
	s.mu.Unlock()

	var filtered []any
	for _, r := range src {
		if query.Where != nil {
			scope := map[string]any{}
			for k, v := range scopeRows {
				scope[k] = v
			}
			scope[query.ItemVar] = r
			if !truthy(eval(query.Where, scope)) {
				continue
			}
		}
		filtered = append(filtered, r)
	}

	order := query.Order
	sort.SliceStable(filtered, func(i, j int) bool {
		a, b := filtered[i].(record), filtered[j].(record)
		var less bool
		if order == "" {
			less = toInt(a["id"]) < toInt(b["id"])
		} else if av, bv := a[order], b[order]; equalVals(av, bv) {
			less = toInt(a["id"]) < toInt(b["id"])
		} else {
			less = lessVal(av, bv)
		}
		if query.Desc {
			return !less
		}
		return less
	})

	if query.After != "" {
		if _, afterID, ok := decodeCursor(query.After); ok {
			for i, r := range filtered {
				if m, ok := r.(record); ok && toInt(m["id"]) == afterID {
					filtered = filtered[i+1:]
					break
				}
			}
		}
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	next := ""
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if len(filtered) == limit {
		if m, ok := filtered[len(filtered)-1].(record); ok {
			ov := any(nil)
			if order != "" {
				ov = m[order]
			}
			next = encodeCursor(ov, toInt(m["id"]))
		}
	}
	return filtered, next, nil
}

// Count and CountBy are the in-memory answers to the same questions the durable
// stores push down. memStore *is* the working set, so "without materializing the
// rows" costs nothing here — but the semantics must match exactly, because a
// test that runs in memory and an app that runs on FacetQL have to agree.

func (s *memStore) Count(query Query) (int, error) {
	rows, err := s.matching(query)
	return len(rows), err
}

func (s *memStore) CountBy(query Query, groupBy string, values []any) (map[string]int, error) {
	rows, err := s.matching(query)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	out := map[string]int{}
	for _, v := range values {
		want[toStr(v)] = true
		out[toStr(v)] = 0 // every value asked about comes back, zero included
	}
	for _, r := range rows {
		m, ok := r.(record)
		if !ok {
			continue
		}
		key := toStr(m[groupBy])
		if len(values) > 0 && !want[key] {
			continue
		}
		out[key]++
	}
	return out, nil
}

// matching returns the rows of an entity a predicate accepts, with the item
// variable bound per row — the selection half of Query, without the paging.
func (s *memStore) matching(query Query) ([]any, error) {
	s.mu.Lock()
	scope := map[string]any{}
	for k, v := range s.rows {
		scope[k] = append([]any{}, v...)
	}
	src := append([]any{}, s.rows[query.Entity]...)
	s.mu.Unlock()

	if query.Where == nil {
		return src, nil
	}
	var out []any
	for _, r := range src {
		scope[query.ItemVar] = r
		if truthy(eval(query.Where, scope)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func equalVals(a, b any) bool { return toStr(a) == toStr(b) && lessVal(a, b) == lessVal(b, a) }

// ── transactions ──────────────────────────────────────────────────────────────

type memTx struct {
	s   *memStore
	ops []func() error
}

func (s *memStore) Begin() (Tx, error) { return &memTx{s: s}, nil }
func (t *memTx) Save(entity string, row map[string]any) error {
	t.ops = append(t.ops, func() error { return t.s.Save(entity, row) })
	return nil
}
func (t *memTx) Delete(entity string, id any) error {
	t.ops = append(t.ops, func() error { return t.s.Delete(entity, id) })
	return nil
}
func (t *memTx) Clear(entity string) error {
	t.ops = append(t.ops, func() error { return t.s.Clear(entity) })
	return nil
}
func (t *memTx) Commit() error {
	for _, op := range t.ops {
		if err := op(); err != nil {
			return err
		}
	}
	return nil
}
func (t *memTx) Rollback() error { t.ops = nil; return nil }

// ── migrations / introspection ──────────────────────────────────────────────

func (s *memStore) Migrate(entities []ir.Entity, apply bool) ([]string, error) {
	if apply {
		s.mu.Lock()
		for _, e := range entities {
			s.ents[e.Name] = e
			if s.rows[e.Name] == nil {
				s.rows[e.Name] = []any{}
			}
		}
		s.mu.Unlock()
	}
	return nil, nil
}

func (s *memStore) Load(entity string) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any{}, s.rows[entity]...), nil
}

// ── audit ──────────────────────────────────────────────────────────────────────

func (s *memStore) Audit(e auditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	return nil
}

func (s *memStore) RecentAudit(limit int) ([]auditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit > 0 && len(s.audit) > limit {
		return append([]auditEntry{}, s.audit[len(s.audit)-limit:]...), nil
	}
	return append([]auditEntry{}, s.audit...), nil
}

// ── operations / clustering (no-ops for a single in-memory instance) ──────────

func (s *memStore) Ping(ctx context.Context) error { return nil }
func (s *memStore) Notify(payload string) error    { return nil }

func (s *memStore) LoadSession(sid string) (*persistedSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, ok := s.sessions[sid]
	return ps, ok, nil
}
func (s *memStore) SaveSession(sid string, ps *persistedSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sid] = ps
	return nil
}
func (s *memStore) DeleteSession(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sid)
	return nil
}
func (s *memStore) PurgeExpiredSessions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for sid, ps := range s.sessions {
		if ps.Expires.Before(now) {
			delete(s.sessions, sid)
		}
	}
	return nil
}

// ── durable jobs ────────────────────────────────────────────────────────────

func (s *memStore) EnqueueJob(j *durableJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextJob++
	j.ID = s.nextJob
	if j.Status == "" {
		j.Status = "pending"
	}
	cp := *j
	s.jobs = append(s.jobs, &cp)
	return nil
}

func (s *memStore) ClaimJob(worker string) (*durableJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, j := range s.jobs {
		if j.Status == "pending" && !j.RunAt.After(now) {
			j.Status = "running"
			cp := *j
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *memStore) FinishJob(id int64, status, lastErr string, nextRun time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.Status = status
			j.LastError = lastErr
			j.RunAt = nextRun
			if status == "retry" {
				j.Status = "pending"
				j.Attempts++
			}
			return nil
		}
	}
	return nil
}

func (s *memStore) PendingJobs() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, j := range s.jobs {
		if j.Status == "pending" {
			n++
		}
	}
	return n, nil
}

func (s *memStore) ReserveCron(name string, next time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if due, ok := s.crons[name]; ok && next.Before(due) {
		return false, nil
	}
	s.crons[name] = next
	return true, nil
}

func (s *memStore) Close() error { return nil }

// copyRecord makes a shallow copy of a row so stored data is isolated from the
// caller's mutations.
func copyRecord(row map[string]any) record {
	out := make(record, len(row))
	for k, v := range row {
		out[k] = v
	}
	return out
}
