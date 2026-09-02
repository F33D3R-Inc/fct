package runtime

// fqStore is the native FacetQL implementation of Store — the replacement for
// pgStore (AGENT_LOG §2). It translates each Store method into FacetQL HTTP calls
// via fqClient. Nothing above the Store interface changes; selecting this backend
// is a matter of pointing FACET_DATABASE_URL at a facetql:// URL (see openStore).
//
// Data-model mapping (decided — AGENT_LOG §2):
//
//	entity          -> FacetQL `kind`
//	row id          -> `address` = "<entity>:<id>"   (client-supplied address)
//	row fields      -> JSON in the node's opaque `data`
//	relation column -> a plain id field inside `data` (NOT an edge, v1)
//
// This file implements the "green" methods fully (Save, Delete, Clear, Init,
// Load, Ping, Notify, Migrate-as-noop, Begin/Tx). The remaining surface (Query,
// audit, sessions, jobs) is stubbed with TODO(fqStore) markers keyed to the
// AGENT_LOG §3 gap list; every stub compiles and returns a clear error rather
// than panicking.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"facet/internal/ir"
)

// envToken is the fallback FacetQL bearer token when the URL carries none.
func envToken() string { return os.Getenv("FACETQL_TOKEN") }

// fqStore persists each entity as FacetQL nodes of a matching kind. It remembers
// the entity definitions so it can encode/decode a row's `data` JSON with the
// correct per-field types (and @secret encryption), exactly as pgStore does for
// its typed columns.
type fqStore struct {
	c    *fqClient
	ents map[string]ir.Entity
}

// fqPageSize is how many nodes we pull per GET /nodes page when loading a kind.
const fqPageSize = 500

func openFacetQL(rawURL string) (Store, error) {
	baseURL, token, err := parseFacetQLURL(rawURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		token = envToken()
	}
	s := &fqStore{c: newFQClient(baseURL, token), ents: map[string]ir.Entity{}}
	// Verify reachability up front, mirroring openPostgres's eager Ping.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.c.ping(ctx); err != nil {
		return nil, fmt.Errorf("connect to FacetQL at %s: %w", baseURL, err)
	}
	return s, nil
}

func (s *fqStore) setEntities(entities []ir.Entity) {
	s.ents = make(map[string]ir.Entity, len(entities))
	for _, e := range entities {
		s.ents[e.Name] = e
	}
}

// ── address / node encoding ─────────────────────────────────────────────────

// fqAddress is the node address for a row: "<entity>:<id>".
func fqAddress(entity string, id any) string {
	return entity + ":" + strconv.Itoa(toInt(id))
}

// rowNode encodes a row into a FacetQL node. Every field (including relation ids)
// is written into the opaque `data` JSON using the same at-rest coercion pgStore
// applies to a column (int/relation -> int64, bool -> bool, text -> string with
// @secret encryption), so the two backends store equivalent values.
func rowNode(e ir.Entity, row map[string]any) (fqNode, error) {
	fb := fieldByName(e)
	m := make(map[string]any, len(row))
	for _, c := range columns(e) {
		m[c] = colValue(fb[c], row[c])
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fqNode{}, fmt.Errorf("encode %s row: %w", e.Name, err)
	}
	return fqNode{
		Address: fqAddress(e.Name, row["id"]),
		Kind:    e.Name,
		Data:    string(data),
		// X/Y/Z/Q coordinate axes and Public are left at their zero values for v1
		// (see risk notes). Owner-scoping/visibility is a later concern.
	}, nil
}

// nodeRecord decodes a FacetQL node back into a runtime record, normalizing each
// field to the Go type the evaluator expects (int/string/bool) and decrypting
// @secret fields — the inverse of rowNode, reusing the same helpers as scanRows.
func nodeRecord(e ir.Entity, n fqNode) (record, error) {
	var raw map[string]any
	if n.Data != "" {
		if err := json.Unmarshal([]byte(n.Data), &raw); err != nil {
			return nil, fmt.Errorf("decode %s node %q: %w", e.Name, n.Address, err)
		}
	}
	fb := fieldByName(e)
	rec := record{}
	for _, c := range columns(e) {
		rec[c] = normalize(raw[c], fb[c])
	}
	return rec, nil
}

// loadAll pages through every node of an entity's kind and decodes them into
// records (used at startup and by Load).
func (s *fqStore) loadAll(e ir.Entity) ([]any, error) {
	ctx := context.Background()
	out := []any{}
	for offset := 0; ; offset += fqPageSize {
		nodes, err := s.c.listKind(ctx, e.Name, fqPageSize, offset)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			rec, err := nodeRecord(e, n)
			if err != nil {
				return nil, err
			}
			out = append(out, rec)
		}
		if len(nodes) < fqPageSize {
			break
		}
	}
	return out, nil
}

// ── green methods ───────────────────────────────────────────────────────────

func (s *fqStore) Init(entities []ir.Entity) (map[string][]any, error) {
	// FacetQL is schemaless, so "bring the schema up to date" is just remembering
	// the entity definitions; there is no DDL to apply.
	s.setEntities(entities)
	out := map[string][]any{}
	for _, e := range entities {
		rows, err := s.loadAll(e)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", e.Name, err)
		}
		out[e.Name] = rows
	}
	return out, nil
}

func (s *fqStore) Save(entity string, row map[string]any) error {
	e := s.ents[entity]
	n, err := rowNode(e, row)
	if err != nil {
		return err
	}
	return s.c.upsert(context.Background(), n)
}

// Delete removes one row by id.
//
// TODO(fqStore) [AGENT_LOG §3 gap 2]: cascade-on-delete is NOT emulated here.
// Children referencing this row (relation id in their data) are left as orphans.
// v1 explicitly accepts orphans; a later pass emulates ON DELETE CASCADE (or the
// facetql side grows cascade support).
func (s *fqStore) Delete(entity string, id any) error {
	return s.c.deleteNode(context.Background(), fqAddress(entity, id))
}

// Clear empties an entity by deleting every node of its kind.
//
// TODO(fqStore) [AGENT_LOG §3 gap 4]: this is N round-trips (list, then delete
// each). Replace with a bulk/conditional delete once facetql exposes one. Also
// does not cascade to children (gap 2), same caveat as Delete.
func (s *fqStore) Clear(entity string) error {
	ctx := context.Background()
	for {
		nodes, err := s.c.listKind(ctx, entity, fqPageSize, 0)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			return nil
		}
		for _, n := range nodes {
			if err := s.c.deleteNode(ctx, n.Address); err != nil {
				return err
			}
		}
		if len(nodes) < fqPageSize {
			return nil
		}
	}
}

// Migrate is a near no-op: FacetQL is schemaless (fields live in the node's JSON
// `data`), so there is no DDL to plan or apply. It only records the entity
// definitions and returns an empty plan (AGENT_LOG §2, "What gets EASIER").
func (s *fqStore) Migrate(entities []ir.Entity, apply bool) ([]string, error) {
	if apply {
		s.setEntities(entities)
	}
	return nil, nil
}

func (s *fqStore) Ping(ctx context.Context) error { return s.c.ping(ctx) }

func (s *fqStore) Load(entity string) ([]any, error) {
	e, ok := s.ents[entity]
	if !ok {
		return nil, fmt.Errorf("unknown entity %q", entity)
	}
	return s.loadAll(e)
}

// Notify fans a payload out to every instance over FacetQL's pub/sub (POST
// /publish), replacing Postgres LISTEN/NOTIFY. The channel matches the pg backend.
func (s *fqStore) Notify(payload string) error {
	return s.c.publish(context.Background(), clusterChannel, payload)
}

func (s *fqStore) Close() error { return nil }

// ── transactions ────────────────────────────────────────────────────────────

// fqTx buffers Save/Delete/Clear operations and submits them as one atomic POST
// /transaction on Commit (AGENT_LOG §3, enabling primitives). Rollback discards
// the buffer without contacting the server.
type fqTx struct {
	s   *fqStore
	ops []fqTxOp
	err error // first encoding error; surfaced at Commit
}

func (s *fqStore) Begin() (Tx, error) { return &fqTx{s: s}, nil }

func (t *fqTx) Save(entity string, row map[string]any) error {
	if t.err != nil {
		return t.err
	}
	n, err := rowNode(t.s.ents[entity], row)
	if err != nil {
		t.err = err
		return err
	}
	t.ops = append(t.ops, fqTxOp{
		Type:    "insert_node",
		Address: n.Address,
		Kind:    n.Kind,
		X:       n.X,
		Y:       n.Y,
		Z:       n.Z,
		Q:       n.Q,
		Data:    n.Data,
		Public:  n.Public,
	})
	return nil
}

func (t *fqTx) Delete(entity string, id any) error {
	t.ops = append(t.ops, fqTxOp{Type: "delete_node", Address: fqAddress(entity, id)})
	return nil
}

// Clear buffers a single native clear_kind op (AGENT_LOG §4b): the FacetQL engine
// removes every node of the kind atomically within the transaction. This is the
// root solution — it is NOT expanded into N delete_node ops.
func (t *fqTx) Clear(entity string) error {
	t.ops = append(t.ops, fqTxOp{Type: "clear_kind", Kind: entity})
	return nil
}

func (t *fqTx) Commit() error {
	if t.err != nil {
		return t.err
	}
	if len(t.ops) == 0 {
		return nil
	}
	return t.s.c.transaction(context.Background(), t.ops)
}

func (t *fqTx) Rollback() error {
	t.ops = nil
	t.err = nil
	return nil
}

// ── stubs (AGENT_LOG §3 gap list — not implemented yet) ─────────────────────

// errFQTODO builds the standard "not implemented yet" error for a stubbed method.
func errFQTODO(method string) error {
	return fmt.Errorf("fqStore.%s: not implemented yet (native FacetQL backend in progress — see AGENT_LOG §3)", method)
}

// Query — keyset-cursor paginated read.
//
// TODO(fqStore) [AGENT_LOG §3 gap 1]: blocked on the facetql predicate-pushdown /
// keyset-cursor work (POST /nodes/query) being built in parallel. That endpoint
// must translate ir.Expr predicates and return an opaque keyset cursor (After ->
// next-cursor), not offset paging. Wire this to it once it lands.
func (s *fqStore) Query(query Query) ([]any, string, error) {
	return nil, "", errFQTODO("Query")
}

// Audit / RecentAudit — durable append-only audit log.
//
// TODO(fqStore) [AGENT_LOG §3 gap 4]: store as nodes of a reserved kind
// "__audit"; RecentAudit needs an ordered, limited read (blocked on the same
// query path as Query).
func (s *fqStore) Audit(e auditEntry) error { return errFQTODO("Audit") }

func (s *fqStore) RecentAudit(limit int) ([]auditEntry, error) {
	return nil, errFQTODO("RecentAudit")
}

// Shared sessions — reserved kind "__session".
//
// TODO(fqStore) [AGENT_LOG §3 gap 4]: LoadSession/SaveSession/DeleteSession map to
// GET/POST/DELETE on a "__session:<sid>" node; PurgeExpiredSessions needs a
// conditional/bulk delete (expires < now) to avoid N round-trips.
func (s *fqStore) LoadSession(sid string) (*persistedSession, bool, error) {
	return nil, false, errFQTODO("LoadSession")
}

func (s *fqStore) SaveSession(sid string, ps *persistedSession) error {
	return errFQTODO("SaveSession")
}

func (s *fqStore) DeleteSession(sid string) error { return errFQTODO("DeleteSession") }

func (s *fqStore) PurgeExpiredSessions() error { return errFQTODO("PurgeExpiredSessions") }

// Durable job queue — reserved kinds "__job" / "__cron".
//
// TODO(fqStore) [AGENT_LOG §3 gap 4]: ClaimJob maps to the verified atomic
// POST /node/:address/claim primitive (the FOR UPDATE SKIP LOCKED equivalent);
// ReserveCron to a conditional claim on a "__cron:<name>" node. EnqueueJob/
// FinishJob/PendingJobs are node writes/reads once reserved-kind conventions and
// the ordered query path exist.
func (s *fqStore) EnqueueJob(j *durableJob) error { return errFQTODO("EnqueueJob") }

func (s *fqStore) ClaimJob(worker string) (*durableJob, error) {
	return nil, errFQTODO("ClaimJob")
}

func (s *fqStore) FinishJob(id int64, status, lastErr string, nextRun time.Time) error {
	return errFQTODO("FinishJob")
}

func (s *fqStore) PendingJobs() (int64, error) { return 0, errFQTODO("PendingJobs") }

func (s *fqStore) ReserveCron(name string, next time.Time) (bool, error) {
	return false, errFQTODO("ReserveCron")
}
