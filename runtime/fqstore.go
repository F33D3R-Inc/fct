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
	"sync/atomic"
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

// ── query / audit / sessions / jobs ─────────────────────────────────────────

// errFQTODO builds the standard "not implemented yet" error for a method that is
// intentionally deferred pending a facetql-side primitive (currently only
// ReserveCron — see its doc comment).
func errFQTODO(method string) error {
	return fmt.Errorf("fqStore.%s: not implemented yet (native FacetQL backend in progress — see AGENT_LOG §3)", method)
}

// ── reserved-kind helpers ───────────────────────────────────────────────────
//
// Audit/session/job/cron records are stored as FacetQL nodes of a reserved kind
// (__audit / __session / __job / __cron) whose opaque `data` holds the record's
// JSON. These do NOT go through rowNode/nodeRecord (those need an entity
// definition); they marshal a value directly, since the adapter fully owns these
// kinds. Nodes are owned by the fct token identity, so reads (same identity) pass
// FacetQL's ownership check.

// reservedUpsert stores v as the `data` JSON of a reserved-kind node.
func (s *fqStore) reservedUpsert(ctx context.Context, kind, address string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s node: %w", kind, err)
	}
	return s.c.upsert(ctx, fqNode{Address: address, Kind: kind, Data: string(data)})
}

// fqIDCounter backs nextFQID; it makes reserved-record ids strictly increasing
// within a process even if two calls land in the same nanosecond, so a record's
// address (which embeds the id) is unique and chronologically sortable.
var fqIDCounter int64

func nextFQID() int64 {
	for {
		n := time.Now().UnixNano()
		cur := atomic.LoadInt64(&fqIDCounter)
		if n <= cur {
			n = cur + 1
		}
		if atomic.CompareAndSwapInt64(&fqIDCounter, cur, n) {
			return n
		}
	}
}

// ── predicate builders (for pushed-down __session/__job queries) ─────────────
//
// These construct the ir.Expr subset FacetQL can push down. The item variable is
// always "item" — matching FacetQL's fixed delete_where item var and the ItemVar
// we send on /nodes/query, so `get(item.field)` resolves the same on both paths.

func fqRef() *ir.Expr             { return &ir.Expr{Kind: "ref", Name: "item"} }
func fqGet(field string) *ir.Expr { return &ir.Expr{Kind: "get", Obj: fqRef(), Field: field} }
func fqLitInt(v int64) *ir.Expr   { return &ir.Expr{Kind: "lit", Val: v, VType: "int"} }
func fqLitText(v string) *ir.Expr { return &ir.Expr{Kind: "lit", Val: v, VType: "text"} }
func fqBin(op string, l, r *ir.Expr) *ir.Expr {
	return &ir.Expr{Kind: "bin", Op: op, L: l, R: r}
}

// Query is a predicate-pushdown, keyset-paginated read against FacetQL's
// POST /nodes/query (AGENT_LOG §3 gap 1, §4b). The whole filter — the pushed-down
// ir.Expr predicate, ordering, page size, and the opaque cursor — is sent to the
// engine, which evaluates it over each node's `data` JSON and returns one page of
// nodes plus the cursor for the next page. The incoming After is passed through
// and the server's next is returned unchanged: the cursor is opaque, never parsed
// or reconstructed here. An unpushable predicate is an error from the engine, not
// a silently wrong or empty page. Nodes are decoded back into rows the same way
// Load does, via nodeRecord (colValue/normalize + entity definition).
func (s *fqStore) Query(query Query) ([]any, string, error) {
	e, ok := s.ents[query.Entity]
	if !ok {
		return nil, "", fmt.Errorf("fqStore.Query: unknown entity %q", query.Entity)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultPageSize
	}
	nodes, next, err := s.c.query(context.Background(), fqQueryRequest{
		Kind:    query.Entity,
		Where:   query.Where,
		ItemVar: query.ItemVar,
		Order:   query.Order,
		Desc:    query.Desc,
		Limit:   limit,
		After:   query.After, // opaque cursor, passed through untouched
	})
	if err != nil {
		return nil, "", fmt.Errorf("fqStore.Query: %w", err)
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		rec, err := nodeRecord(e, n)
		if err != nil {
			return nil, "", fmt.Errorf("fqStore.Query: %w", err)
		}
		out = append(out, rec)
	}
	return out, next, nil // server's next returned opaque; "" = last page
}

// ── audit log (reserved kind "__audit") ──────────────────────────────────────

// Audit appends one entry as an immutable __audit node. The address embeds a
// strictly-increasing id (nextFQID) so entries sort chronologically by address —
// the base order the query path uses when no data field is named.
func (s *fqStore) Audit(e auditEntry) error {
	addr := fmt.Sprintf("__audit:%019d", nextFQID())
	return s.reservedUpsert(context.Background(), "__audit", addr, e)
}

// RecentAudit returns up to limit entries, oldest-first (to seed the in-memory
// ring chronologically), matching pgStore. It pages POST /nodes/query ordered by
// address descending (newest first) via the opaque cursor until it has limit
// entries, then reverses to oldest-first.
func (s *fqStore) RecentAudit(limit int) ([]auditEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	ctx := context.Background()
	newestFirst := make([]auditEntry, 0, limit)
	after := ""
	for len(newestFirst) < limit {
		page := limit - len(newestFirst)
		if page > 500 {
			page = 500
		}
		// Order "id" => base (address) order; Desc => newest (highest id) first.
		nodes, next, err := s.c.query(ctx, fqQueryRequest{
			Kind: "__audit", ItemVar: "item", Order: "id", Desc: true, Limit: page, After: after,
		})
		if err != nil {
			return nil, fmt.Errorf("fqStore.RecentAudit: %w", err)
		}
		for _, n := range nodes {
			var e auditEntry
			if n.Data != "" {
				if err := json.Unmarshal([]byte(n.Data), &e); err != nil {
					return nil, fmt.Errorf("fqStore.RecentAudit: decode %q: %w", n.Address, err)
				}
			}
			newestFirst = append(newestFirst, e)
		}
		if next == "" || len(nodes) == 0 {
			break
		}
		after = next
	}
	// reverse to oldest-first
	for i, j := 0, len(newestFirst)-1; i < j; i, j = i+1, j-1 {
		newestFirst[i], newestFirst[j] = newestFirst[j], newestFirst[i]
	}
	return newestFirst, nil
}

// ── shared sessions (reserved kind "__session") ──────────────────────────────

// fqSessionData is a session's stored form: the whole persistedSession plus a
// numeric _expires_unix so PurgeExpiredSessions can compare expiry with a pushed-
// down predicate (numeric comparison, unambiguous — unlike an RFC3339 string).
type fqSessionData struct {
	persistedSession
	ExpiresUnix int64 `json:"_expires_unix"`
}

func fqSessionAddr(sid string) string { return "__session:" + sid }

func (s *fqStore) LoadSession(sid string) (*persistedSession, bool, error) {
	n, found, err := s.c.getNode(context.Background(), fqSessionAddr(sid))
	if err != nil {
		return nil, false, fmt.Errorf("fqStore.LoadSession: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	var d fqSessionData
	if n.Data != "" {
		if err := json.Unmarshal([]byte(n.Data), &d); err != nil {
			return nil, false, fmt.Errorf("fqStore.LoadSession: decode %q: %w", sid, err)
		}
	}
	if d.State == nil {
		d.State = map[string]any{}
	}
	ps := d.persistedSession
	return &ps, true, nil
}

func (s *fqStore) SaveSession(sid string, ps *persistedSession) error {
	d := fqSessionData{persistedSession: *ps, ExpiresUnix: ps.Expires.Unix()}
	return s.reservedUpsert(context.Background(), "__session", fqSessionAddr(sid), d)
}

func (s *fqStore) DeleteSession(sid string) error {
	return s.c.deleteNode(context.Background(), fqSessionAddr(sid))
}

// PurgeExpiredSessions removes every expired session in ONE native delete_where
// op (AGENT_LOG §4b): the engine evaluates `item._expires_unix < now` over each
// __session node's data and tombstones the matches atomically — no N round-trips.
func (s *fqStore) PurgeExpiredSessions() error {
	pred := fqBin("<", fqGet("_expires_unix"), fqLitInt(time.Now().Unix()))
	return s.c.transaction(context.Background(), []fqTxOp{
		{Type: "delete_where", Kind: "__session", Where: pred},
	})
}

// ── durable job queue (reserved kinds "__job" / "__cron") ─────────────────────

// fqJobData is a job's stored form. run_at is kept as a unix int so ClaimJob can
// push down `run_at_unix <= now` and order by it.
type fqJobData struct {
	ID          int64  `json:"id"`
	Queue       string `json:"queue"`
	Action      string `json:"action"`
	Args        []any  `json:"args"`
	RunAtUnix   int64  `json:"run_at_unix"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	Status      string `json:"status"` // pending | running | done | dead
	LastError   string `json:"last_error"`
}

func fqJobAddr(id int64) string { return fmt.Sprintf("__job:%019d", id) }

func jobFromData(d fqJobData) *durableJob {
	return &durableJob{
		ID: d.ID, Queue: d.Queue, Action: d.Action, Args: d.Args,
		RunAt: time.Unix(d.RunAtUnix, 0), Attempts: d.Attempts,
		MaxAttempts: d.MaxAttempts, Status: d.Status, LastError: d.LastError,
	}
}

func (s *fqStore) EnqueueJob(j *durableJob) error {
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 5
	}
	if j.Queue == "" {
		j.Queue = "default"
	}
	if j.RunAt.IsZero() {
		j.RunAt = time.Now()
	}
	id := nextFQID()
	d := fqJobData{
		ID: id, Queue: j.Queue, Action: j.Action, Args: j.Args,
		RunAtUnix: j.RunAt.Unix(), Attempts: 0, MaxAttempts: j.MaxAttempts, Status: "pending",
	}
	return s.reservedUpsert(context.Background(), "__job", fqJobAddr(id), d)
}

// ClaimJob leases the next due, pending job to exactly one worker. It queries the
// due-and-pending jobs oldest-first, then atomically claims candidates via the
// verified POST /node/:address/claim primitive (the FOR-UPDATE-SKIP-LOCKED
// equivalent): the first candidate we win is ours; a candidate already leased by
// a racing worker returns won=false and we try the next. The claim (claimed_by)
// IS the lease — we deliberately do NOT upsert the node here, since upsert would
// replace it and clear the lease. Attempts are incremented on retry in FinishJob.
func (s *fqStore) ClaimJob(worker string) (*durableJob, error) {
	ctx := context.Background()
	now := time.Now().Unix()
	pred := fqBin("&&", fqBin("==", fqGet("status"), fqLitText("pending")),
		fqBin("<=", fqGet("run_at_unix"), fqLitInt(now)))
	nodes, _, err := s.c.query(ctx, fqQueryRequest{
		Kind: "__job", ItemVar: "item", Where: pred, Order: "run_at_unix", Desc: false, Limit: 50,
	})
	if err != nil {
		return nil, fmt.Errorf("fqStore.ClaimJob: %w", err)
	}
	for _, n := range nodes {
		won, err := s.c.claim(ctx, n.Address)
		if err != nil {
			return nil, fmt.Errorf("fqStore.ClaimJob: %w", err)
		}
		if !won {
			continue // leased by another worker, or already gone
		}
		var d fqJobData
		if err := json.Unmarshal([]byte(n.Data), &d); err != nil {
			return nil, fmt.Errorf("fqStore.ClaimJob: decode %q: %w", n.Address, err)
		}
		return jobFromData(d), nil
	}
	return nil, nil // nothing due to claim
}

// FinishJob records a claimed job's outcome. done => the node is deleted; dead =>
// marked (kept as a dead-letter, status "dead" so it is never re-claimed);
// pending (a retry) => re-enqueued via upsert, which replaces the node and thereby
// RELEASES the lease (clears claimed_by), so the rescheduled job is claimable
// again. Attempts is incremented on each retry.
func (s *fqStore) FinishJob(id int64, status, lastErr string, nextRun time.Time) error {
	ctx := context.Background()
	addr := fqJobAddr(id)
	if status == "done" {
		return s.c.deleteNode(ctx, addr)
	}
	n, found, err := s.c.getNode(ctx, addr)
	if err != nil {
		return fmt.Errorf("fqStore.FinishJob: %w", err)
	}
	if !found {
		return nil // already gone; nothing to record
	}
	var d fqJobData
	if err := json.Unmarshal([]byte(n.Data), &d); err != nil {
		return fmt.Errorf("fqStore.FinishJob: decode %q: %w", addr, err)
	}
	d.LastError = lastErr
	if status == "pending" {
		d.Status = "pending"
		d.RunAtUnix = nextRun.Unix()
		d.Attempts++
	} else {
		d.Status = status // "dead"
	}
	return s.reservedUpsert(ctx, "__job", addr, d)
}

// PendingJobs reports queue depth (count of pending __job nodes). This is a linear
// scan over the pending set for v1.
// TODO(fqStore): a native COUNT primitive on facetql would avoid paging all rows.
func (s *fqStore) PendingJobs() (int64, error) {
	ctx := context.Background()
	pred := fqBin("==", fqGet("status"), fqLitText("pending"))
	var total int64
	after := ""
	for {
		nodes, next, err := s.c.query(ctx, fqQueryRequest{
			Kind: "__job", ItemVar: "item", Where: pred, Order: "id", Desc: false, Limit: 500, After: after,
		})
		if err != nil {
			return 0, fmt.Errorf("fqStore.PendingJobs: %w", err)
		}
		total += int64(len(nodes))
		if next == "" || len(nodes) == 0 {
			break
		}
		after = next
	}
	return total, nil
}

// ReserveCron needs "advance a __cron:<name> node's next_run ONLY if it is absent
// or already due, and report whether this caller won" — an atomic conditional
// compare-and-set on a field value. FacetQL has no such primitive today: `claim`
// is claim-once (not conditional on a field, and not re-armable each tick), and
// upsert is an unconditional whole-node replace. Faking it with read-then-write
// would be a race (two instances both reading "due" and both winning the tick),
// which the no-racy-hacks rule forbids.
//
// TODO(fqStore) [AGENT_LOG §28]: needs a native facetql conditional-update / CAS
// tx op, e.g. `set_if { address, field, expect_le: <now>, set: {next_run} }`
// returning whether it applied. Deferred pending that primitive — coordinate the
// facetql work rather than shipping a racy cron.
func (s *fqStore) ReserveCron(name string, next time.Time) (bool, error) {
	return false, errFQTODO("ReserveCron")
}
