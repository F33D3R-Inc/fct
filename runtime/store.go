package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"facet/internal/ir"

	_ "github.com/lib/pq" // postgres driver ("postgres")
)

// StoreDescription summarizes where this app's data will live, for the startup
// banner — read from FACET_DATABASE_URL.
func StoreDescription(app string) string {
	url := os.Getenv("FACET_DATABASE_URL")
	switch {
	case strings.HasPrefix(url, "facetql://"):
		return "facetql"
	case strings.HasPrefix(url, "postgres"):
		return "postgres"
	}
	return "facetql (default: facetql://localhost:8080)"
}

// AggSpec names the reduction a store is being asked for: the function, and the
// column it reduces.
//
// Only the three irreducible functions appear here. `count` has its own typed
// call (it reduces rows, not a column, and is by far the most asked), and `avg`
// is composed from `sum` and `count` above the store so that its integer
// division has exactly one implementation.
//
// Field is always a declared numeric column — `int` or `money`. Pushdown is
// refused for anything else rather than guessed at, because the language's
// reducer is integer arithmetic and a text or date column has no meaning under
// it that a database would agree with.
type AggSpec struct {
	Func  string // sum | min | max
	Field string
}

// pushableAgg reports whether an aggregate op is one a store can be asked for
// directly. `avg` is absent on purpose: it is composed, not pushed.
func pushableAgg(op string) bool {
	switch op {
	case "sum", "min", "max":
		return true
	}
	return false
}

// numericField reports whether a column is one the language's reducer can fold —
// integer arithmetic, so `int` and `money` and nothing else.
func numericField(f ir.Field) bool {
	return f.Type == "int" || f.Type == "money"
}

// Store is the durable home of an app's entity data — the database. The runtime
// keeps the live working set in memory (so rendering and the reactive engine
// stay fast) and writes every change through to the Store, which is the source
// of truth across restarts. Entity fields are real typed, indexed columns and
// relations are foreign keys, so reads can be pushed down (Query) and stay
// sub-linear as a table grows past what fits in memory. The backend is Postgres;
// you point at it with FACET_DATABASE_URL.
type Store interface {
	// Init brings the schema up to date (the same additive migration Migrate
	// applies) and returns every existing row (entity name -> rows) to seed the
	// in-memory working set.
	Init(entities []ir.Entity) (map[string][]any, error)
	// Save inserts or updates one row (keyed by its "id").
	Save(entity string, row map[string]any) error
	// Delete removes one row by id, and with it every row that referenced it
	// (ir.References, recursively). The cascade is the store's, not the caller's:
	// pgStore declares it as ON DELETE CASCADE, fqStore declares it to FacetQL as
	// a reference the engine expands into the same transaction as the delete, and
	// memStore applies it to its own rows — so the children can never be left
	// behind by a crash between two requests.
	Delete(entity string, id any) error
	// Clear empties an entity (cascading to its children).
	Clear(entity string) error
	// Query runs an indexed, paginated read pushed down to SQL, returning the page
	// of rows and an opaque cursor for the next page ("" if the page is the last).
	Query(query Query) ([]any, string, error)
	// Count answers how many rows match a predicate without materializing any of
	// them, so a `count(...)`/`exists(...)` costs one integer rather than a table
	// in memory. It reads Entity/Where/ItemVar only: a count has no page, and
	// accepting an order or a limit here would invite a caller to believe it had
	// counted one.
	Count(query Query) (int, error)
	// CountBy answers one predicate for many pinned values of a field at once —
	// the shape a rendered page needs, where the same aggregate appears once per
	// row with a different id pinned into it (`count(l in Like where l.tweet ==
	// t.id)`). Values are the pinned values to answer for; every one of them comes
	// back, zero included, because an absent key is indistinguishable from an
	// answer the store forgot. The result is keyed by the value's text form, which
	// is unambiguous because one column has one type.
	//
	// This exists so a page of twenty rows costs one round trip per aggregate
	// rather than twenty: issuing Count per row would be an N+1 across the
	// network, and grouping the whole table to fill in twenty answers is slower
	// still (measured: 20 counts 13.7 ms, whole-kind grouping 153 ms, pinned
	// values 0.93 ms).
	CountBy(query Query, groupBy string, values []any) (map[string]int, error)
	// Aggregate reduces one numeric column over the rows a predicate selects —
	// `sum(o.amount in Order where o.seller == actor)` — without any of them
	// crossing the wire. It is Count's sibling for the other three reductions,
	// and it exists for the same reason: the alternative is paging the rows into
	// this process to add them up, which costs a table to learn one integer and
	// is wrong the moment the rows outgrow a page.
	//
	// `avg` is deliberately NOT one of the functions a store implements. It is
	// defined by the language as sum ÷ count in integer arithmetic (see
	// reduceAgg), so it is composed from those two above the store — where the
	// rounding is written once instead of once per backend.
	//
	// The empty reduction is 0, never an error and never a null. Every store
	// must return the same number reduceAgg would over the same rows, because a
	// page can resolve one aggregate here and the next on the mirror.
	Aggregate(query Query, spec AggSpec) (int, error)
	// AggregateBy is to Aggregate what CountBy is to Count: one predicate
	// reduced for many pinned values of a field, so a page showing a total per
	// row costs one request rather than one per row. Every requested value comes
	// back, 0 included.
	AggregateBy(query Query, spec AggSpec, groupBy string, values []any) (map[string]int, error)
	// Begin starts a transaction so a multi-statement action's writes commit
	// atomically (all or nothing).
	Begin() (Tx, error)
	// Migrate diffs the live schema against the entities and returns the ordered,
	// additive DDL that reconciles them; with apply, it runs and records each
	// statement (versioned in facet_migrations) before returning.
	Migrate(entities []ir.Entity, apply bool) ([]string, error)
	// Audit appends one entry to the durable audit log.
	Audit(e auditEntry) error
	// RecentAudit returns up to limit recent audit entries, oldest first (to seed
	// the in-memory ring at startup).
	RecentAudit(limit int) ([]auditEntry, error)

	// ── Phase 3: operations ──
	// Ping verifies the database is reachable (the readiness probe).
	Ping(ctx context.Context) error
	// Load reads one entity's full table (used to refresh the working set when a
	// peer instance announces a change over pub/sub).
	Load(entity string) ([]any, error)

	// Notify publishes a payload on the cross-instance event channel (Postgres
	// LISTEN/NOTIFY), so every instance's live clients converge.
	Notify(payload string) error

	// Shared session store (stateless servers): a session lives in the database so
	// any instance can serve any request.
	LoadSession(sid string) (*persistedSession, bool, error)
	SaveSession(sid string, ps *persistedSession) error
	DeleteSession(sid string) error
	PurgeExpiredSessions() error

	// Durable job queue: enqueue persists a unit of work; ClaimJob atomically
	// leases the next due job to exactly one worker (FOR UPDATE SKIP LOCKED);
	// FinishJob records the outcome (done, retry with backoff, or dead-letter);
	// PendingJobs reports queue depth; ReserveCron lets one instance win the right
	// to enqueue a scheduled tick.
	EnqueueJob(j *durableJob) error
	ClaimJob(worker string) (*durableJob, error)
	FinishJob(id int64, status, lastErr string, nextRun time.Time) error
	PendingJobs() (int64, error)
	ReserveCron(name string, next time.Time) (bool, error)

	Close() error
}

// persistedSession is a session as stored in the shared session table, so any
// stateless instance can rehydrate it.
type persistedSession struct {
	Actor      string         `json:"actor"`
	Role       string         `json:"role"`
	Verified   bool           `json:"verified"`
	PendingMFA string         `json:"pendingMFA"`
	State      map[string]any `json:"state"`
	Expires    time.Time      `json:"expires"`
}

// durableJob is one persisted unit of background work, retried with backoff and
// dead-lettered when it exhausts its attempts.
type durableJob struct {
	ID          int64
	Queue       string
	Action      string
	Args        []any
	RunAt       time.Time
	Attempts    int
	MaxAttempts int
	Status      string // pending | running | done | dead
	LastError   string
}

// Tx is a single atomic unit of durable writes. The runtime applies every
// statement of one action through a Tx and commits once; any failure rolls the
// whole action's persistence back, so the database never holds a half-applied
// action.
type Tx interface {
	Save(entity string, row map[string]any) error
	Delete(entity string, id any) error
	Clear(entity string) error
	Commit() error
	Rollback() error
}

// openStore connects to Postgres from FACET_DATABASE_URL. It is required: there
// is no other backend.
//
//	FACET_DATABASE_URL=postgres://user:pw@host:5432/dbname
func openStore(url string) (Store, error) {
	// FacetQL is the default backend: an unset FACET_DATABASE_URL points at a local
	// FacetQL instead of erroring. Postgres remains reachable only via an explicit
	// postgres:// URL.
	if url == "" {
		url = "facetql://localhost:8080"
	}
	// Native FacetQL backend — the replacement for Postgres (AGENT_LOG §2).
	if strings.HasPrefix(url, "facetql://") {
		return openFacetQL(url)
	}
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return nil, fmt.Errorf("FACET_DATABASE_URL %q is not a FacetQL (facetql://…) or Postgres (postgres://…) URL", url)
	}
	return openPostgres(url)
}

// Migrate reconciles the database schema with an application's entities. With
// apply=false it returns the additive DDL plan and touches nothing (for
// `facet migrate --plan`); with apply=true it runs and records each statement.
// It opens its own connection from FACET_DATABASE_URL and closes it before
// returning, so it is safe to call from the CLI without a running server.
func Migrate(graph *ir.IR, apply bool) ([]string, error) {
	store, err := openStore(os.Getenv("FACET_DATABASE_URL"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.Migrate(graph.Entities, apply)
}

// ── Postgres backend ──────────────────────────────────────────────────────────

// pgStore persists each entity as a real table: one typed, indexed column per
// field, relations as foreign keys with ON DELETE CASCADE. It remembers the
// entity definitions so Save/Query/Tx can build column-aware SQL.
type pgStore struct {
	db   *sql.DB
	ents map[string]ir.Entity
}

func openPostgres(dsn string) (Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &pgStore{db: db, ents: map[string]ir.Entity{}}, nil
}

func (s *pgStore) setEntities(entities []ir.Entity) {
	s.ents = make(map[string]ir.Entity, len(entities))
	for _, e := range entities {
		s.ents[e.Name] = e
	}
}

func (s *pgStore) Init(entities []ir.Entity) (map[string][]any, error) {
	if _, err := s.Migrate(entities, true); err != nil {
		return nil, err
	}
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

// loadAll reads an entity's full table into records (used once, at startup, to
// seed the in-memory working set). Large, paginated reads go through Query.
func (s *pgStore) loadAll(e ir.Entity) ([]any, error) {
	var qc []string
	for _, c := range columns(e) {
		qc = append(qc, q(c))
	}
	rows, err := s.db.Query(fmt.Sprintf("SELECT %s FROM %s ORDER BY %s",
		strings.Join(qc, ", "), q(table(e.Name)), q("id")))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows, e)
}

func (s *pgStore) Save(entity string, row map[string]any) error {
	e := s.ents[entity]
	_, err := s.db.Exec(upsertSQL(e), rowArgs(e, row)...)
	return err
}

func (s *pgStore) Delete(entity string, id any) error {
	_, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s = $1", q(table(entity)), q("id")), int64(toInt(id)))
	return err
}

func (s *pgStore) Clear(entity string) error {
	_, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s", q(table(entity))))
	return err
}

// Count compiles the predicate to the same pushed-down WHERE a Query would use
// and asks the database for the cardinality, so no row crosses the wire.
func (s *pgStore) Count(query Query) (int, error) {
	e := s.ents[query.Entity]
	sqlText, args, err := countSQL(query, e)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(sqlText, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountBy answers the same predicate for many pinned values in one statement: a
// GROUP BY over the rows whose grouped column is in the requested set. Values
// absent from the result are filled in as zero, because the caller asked about
// them and "no rows" is an answer.
func (s *pgStore) CountBy(query Query, groupBy string, values []any) (map[string]int, error) {
	e := s.ents[query.Entity]
	sqlText, args, err := countBySQL(query, e, groupBy, values)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(values))
	for _, v := range values {
		out[toStr(v)] = 0
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key any
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[toStr(key)] = n
	}
	return out, rows.Err()
}

// Aggregate compiles the reduction to SQL and lets the database do it, so no row
// crosses the wire. COALESCE is what makes the empty reduction 0 rather than
// NULL — the language types these as the column's own numeric type, and has no
// hole to put a NULL in.
//
// The result is scanned into `any` rather than an int: `sum` over a bigint
// column is NUMERIC in Postgres and arrives as []byte, which is exactly the
// conversion `toInt` was fixed to handle.
func (s *pgStore) Aggregate(query Query, spec AggSpec) (int, error) {
	e := s.ents[query.Entity]
	sqlText, args, err := aggregateSQL(query, e, spec)
	if err != nil {
		return 0, err
	}
	var v any
	if err := s.db.QueryRow(sqlText, args...).Scan(&v); err != nil {
		return 0, err
	}
	return toInt(v), nil
}

// AggregateBy is the grouped form: one GROUP BY over the rows whose grouped
// column is in the requested set. Values absent from the result are filled in as
// 0, because the caller asked about them and "no rows" is an answer — the same
// contract CountBy holds.
func (s *pgStore) AggregateBy(query Query, spec AggSpec, groupBy string, values []any) (map[string]int, error) {
	e := s.ents[query.Entity]
	sqlText, args, err := aggregateBySQL(query, e, spec, groupBy, values)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(values))
	for _, v := range values {
		out[toStr(v)] = 0
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, v any
		if err := rows.Scan(&key, &v); err != nil {
			return nil, err
		}
		out[toStr(key)] = toInt(v)
	}
	return out, rows.Err()
}

func (s *pgStore) Query(query Query) ([]any, string, error) {
	e := s.ents[query.Entity]
	if query.Limit <= 0 {
		query.Limit = defaultPageSize
	}
	sqlText, args, err := selectSQL(query, e)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, "", err
	}
	recs, err := scanRows(rows, e)
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	// We asked for one row past the page. If it came back, there is a next page;
	// trim it and mint a cursor from the last row we return.
	next := ""
	if len(recs) > query.Limit {
		recs = recs[:query.Limit]
		last := recs[len(recs)-1].(record)
		order := query.Order
		if order == "" {
			order = "id"
		}
		next = encodeCursor(last[order], toInt(last["id"]))
	}
	return recs, next, nil
}

func (s *pgStore) Begin() (Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &pgTx{tx: tx, store: s}, nil
}

func (s *pgStore) Close() error { return s.db.Close() }

// ── Phase 3: operations ───────────────────────────────────────────────────────

func (s *pgStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Load reads one entity's full table, used to refresh the in-memory working set
// when a peer instance announces a change.
func (s *pgStore) Load(entity string) ([]any, error) {
	e, ok := s.ents[entity]
	if !ok {
		return nil, fmt.Errorf("unknown entity %q", entity)
	}
	return s.loadAll(e)
}

// Notify publishes on the cross-instance event channel.
func (s *pgStore) Notify(payload string) error {
	_, err := s.db.Exec(`SELECT pg_notify($1, $2)`, clusterChannel, payload)
	return err
}

// ── shared sessions ────────────────────────────────────────────────────────────

func (s *pgStore) LoadSession(sid string) (*persistedSession, bool, error) {
	var (
		ps        persistedSession
		stateJSON []byte
	)
	err := s.db.QueryRow(
		`SELECT actor, role, verified, pending_mfa, state, expires FROM facet_sessions WHERE sid = $1`, sid).
		Scan(&ps.Actor, &ps.Role, &ps.Verified, &ps.PendingMFA, &stateJSON, &ps.Expires)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	ps.State = map[string]any{}
	if len(stateJSON) > 0 {
		_ = json.Unmarshal(stateJSON, &ps.State)
	}
	return &ps, true, nil
}

func (s *pgStore) SaveSession(sid string, ps *persistedSession) error {
	stateJSON, err := json.Marshal(ps.State)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO facet_sessions (sid, actor, role, verified, pending_mfa, state, expires) `+
			`VALUES ($1, $2, $3, $4, $5, $6, $7) `+
			`ON CONFLICT (sid) DO UPDATE SET actor = $2, role = $3, verified = $4, `+
			`pending_mfa = $5, state = $6, expires = $7`,
		sid, ps.Actor, ps.Role, ps.Verified, ps.PendingMFA, stateJSON, ps.Expires)
	return err
}

func (s *pgStore) DeleteSession(sid string) error {
	_, err := s.db.Exec(`DELETE FROM facet_sessions WHERE sid = $1`, sid)
	return err
}

func (s *pgStore) PurgeExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM facet_sessions WHERE expires < now()`)
	return err
}

// ── durable jobs ───────────────────────────────────────────────────────────────

func (s *pgStore) EnqueueJob(j *durableJob) error {
	args, err := json.Marshal(j.Args)
	if err != nil {
		return err
	}
	if j.MaxAttempts <= 0 {
		j.MaxAttempts = 5
	}
	if j.Queue == "" {
		j.Queue = "default"
	}
	if j.RunAt.IsZero() {
		j.RunAt = time.Now()
	}
	_, err = s.db.Exec(
		`INSERT INTO facet_jobs (queue, action, args, run_at, max_attempts, status) `+
			`VALUES ($1, $2, $3, $4, $5, 'pending')`,
		j.Queue, j.Action, args, j.RunAt, j.MaxAttempts)
	return err
}

// ClaimJob atomically leases the next due, pending job to one worker. FOR UPDATE
// SKIP LOCKED means two instances racing for work never collide — each takes a
// different row, or none.
func (s *pgStore) ClaimJob(worker string) (*durableJob, error) {
	var (
		j    durableJob
		args []byte
	)
	err := s.db.QueryRow(
		`UPDATE facet_jobs SET status = 'running', attempts = attempts + 1, `+
			`locked_by = $1, locked_at = now() `+
			`WHERE id = (SELECT id FROM facet_jobs WHERE status = 'pending' AND run_at <= now() `+
			`ORDER BY run_at FOR UPDATE SKIP LOCKED LIMIT 1) `+
			`RETURNING id, queue, action, args, attempts, max_attempts`, worker).
		Scan(&j.ID, &j.Queue, &j.Action, &args, &j.Attempts, &j.MaxAttempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &j.Args)
	}
	return &j, nil
}

// FinishJob records a claimed job's outcome: done (success), pending (a retry,
// rescheduled to nextRun), or dead (dead-lettered after exhausting attempts).
func (s *pgStore) FinishJob(id int64, status, lastErr string, nextRun time.Time) error {
	if status == "pending" {
		_, err := s.db.Exec(
			`UPDATE facet_jobs SET status = 'pending', run_at = $2, last_error = $3, `+
				`locked_by = '', locked_at = NULL WHERE id = $1`, id, nextRun, lastErr)
		return err
	}
	_, err := s.db.Exec(
		`UPDATE facet_jobs SET status = $2, last_error = $3, locked_by = '', locked_at = NULL WHERE id = $1`,
		id, status, lastErr)
	return err
}

func (s *pgStore) PendingJobs() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT count(*) FROM facet_jobs WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// ReserveCron atomically claims the right to enqueue a scheduled tick: it
// advances the job's next_run only if the row is absent or already due, and
// reports whether this caller won. Exactly one instance wins each tick.
func (s *pgStore) ReserveCron(name string, next time.Time) (bool, error) {
	res, err := s.db.Exec(
		`INSERT INTO facet_cron (name, next_run) VALUES ($1, $2) `+
			`ON CONFLICT (name) DO UPDATE SET next_run = $2 WHERE facet_cron.next_run <= now()`,
		name, next)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── audit log ─────────────────────────────────────────────────────────────────

func (s *pgStore) Audit(e auditEntry) error {
	_, err := s.db.Exec(
		`INSERT INTO facet_audit (at, actor, action, allowed, detail) VALUES ($1, $2, $3, $4, $5)`,
		e.Time, e.Actor, e.Action, e.Allowed, e.Detail)
	return err
}

func (s *pgStore) RecentAudit(limit int) ([]auditEntry, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(
		`SELECT at, actor, action, allowed, detail FROM facet_audit ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.Time, &e.Actor, &e.Action, &e.Allowed, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	// reverse to oldest-first so the ring seeds in chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ── transactions ──────────────────────────────────────────────────────────────

type pgTx struct {
	tx    *sql.Tx
	store *pgStore
}

func (t *pgTx) Save(entity string, row map[string]any) error {
	e := t.store.ents[entity]
	_, err := t.tx.Exec(upsertSQL(e), rowArgs(e, row)...)
	return err
}

func (t *pgTx) Delete(entity string, id any) error {
	_, err := t.tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s = $1", q(table(entity)), q("id")), int64(toInt(id)))
	return err
}

func (t *pgTx) Clear(entity string) error {
	_, err := t.tx.Exec(fmt.Sprintf("DELETE FROM %s", q(table(entity))))
	return err
}

func (t *pgTx) Commit() error   { return t.tx.Commit() }
func (t *pgTx) Rollback() error { return t.tx.Rollback() }

// ── migrations ────────────────────────────────────────────────────────────────

func (s *pgStore) Migrate(entities []ir.Entity, apply bool) ([]string, error) {
	s.setEntities(entities)
	if apply {
		if _, err := s.db.Exec(
			`CREATE TABLE IF NOT EXISTS facet_migrations (` +
				`version BIGSERIAL PRIMARY KEY, statement TEXT NOT NULL, ` +
				`applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			return nil, fmt.Errorf("create migrations table: %w", err)
		}
		if _, err := s.db.Exec(
			`CREATE TABLE IF NOT EXISTS facet_audit (` +
				`id BIGSERIAL PRIMARY KEY, at BIGINT NOT NULL, actor TEXT NOT NULL, ` +
				`action TEXT NOT NULL, allowed BOOLEAN NOT NULL, detail TEXT NOT NULL DEFAULT '')`); err != nil {
			return nil, fmt.Errorf("create audit table: %w", err)
		}
		// Phase 3 operational tables: shared sessions, the durable job queue, and the
		// cron reservation table. All idempotent so startup and `facet migrate` agree.
		for _, ddl := range []string{
			`CREATE TABLE IF NOT EXISTS facet_sessions (` +
				`sid TEXT PRIMARY KEY, actor TEXT NOT NULL DEFAULT 'guest', ` +
				`role TEXT NOT NULL DEFAULT 'guest', verified BOOLEAN NOT NULL DEFAULT false, ` +
				`pending_mfa TEXT NOT NULL DEFAULT '', state JSONB NOT NULL DEFAULT '{}', ` +
				`expires TIMESTAMPTZ NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS facet_sessions_expires_idx ON facet_sessions (expires)`,
			`CREATE TABLE IF NOT EXISTS facet_jobs (` +
				`id BIGSERIAL PRIMARY KEY, queue TEXT NOT NULL DEFAULT 'default', ` +
				`action TEXT NOT NULL, args JSONB NOT NULL DEFAULT '[]', ` +
				`run_at TIMESTAMPTZ NOT NULL DEFAULT now(), attempts INT NOT NULL DEFAULT 0, ` +
				`max_attempts INT NOT NULL DEFAULT 5, status TEXT NOT NULL DEFAULT 'pending', ` +
				`last_error TEXT NOT NULL DEFAULT '', locked_by TEXT NOT NULL DEFAULT '', ` +
				`locked_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
			`CREATE INDEX IF NOT EXISTS facet_jobs_due_idx ON facet_jobs (status, run_at)`,
			`CREATE TABLE IF NOT EXISTS facet_cron (` +
				`name TEXT PRIMARY KEY, next_run TIMESTAMPTZ NOT NULL)`,
		} {
			if _, err := s.db.Exec(ddl); err != nil {
				return nil, fmt.Errorf("create operational table: %w", err)
			}
		}
	}
	sc, err := s.introspect()
	if err != nil {
		return nil, err
	}
	plan := planMigration(sc, entities)
	if !apply {
		return plan, nil
	}
	for _, stmt := range plan {
		if _, err := s.db.Exec(stmt); err != nil {
			return plan, fmt.Errorf("apply %q: %w", stmt, err)
		}
		if _, err := s.db.Exec(`INSERT INTO facet_migrations (statement) VALUES ($1)`, stmt); err != nil {
			return plan, fmt.Errorf("record migration: %w", err)
		}
	}
	return plan, nil
}

// introspect snapshots the live schema: every facet_ table's columns, plus the
// names of existing indexes and constraints (so the planner knows what it has
// already built).
func (s *pgStore) introspect() (schema, error) {
	sc := schema{cols: map[string]map[string]bool{}, indexes: map[string]bool{}}

	colRows, err := s.db.Query(
		`SELECT table_name, column_name FROM information_schema.columns ` +
			`WHERE table_schema = 'public' AND table_name LIKE 'facet\_%'`)
	if err != nil {
		return sc, fmt.Errorf("introspect columns: %w", err)
	}
	for colRows.Next() {
		var t, c string
		if err := colRows.Scan(&t, &c); err != nil {
			colRows.Close()
			return sc, err
		}
		if sc.cols[t] == nil {
			sc.cols[t] = map[string]bool{}
		}
		sc.cols[t][c] = true
	}
	colRows.Close()
	if err := colRows.Err(); err != nil {
		return sc, err
	}

	// indexes (covers CREATE INDEX names) and constraints (covers FK names).
	for _, qy := range []string{
		`SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename LIKE 'facet\_%'`,
		`SELECT conname FROM pg_constraint`,
	} {
		r, err := s.db.Query(qy)
		if err != nil {
			return sc, fmt.Errorf("introspect indexes: %w", err)
		}
		for r.Next() {
			var name string
			if err := r.Scan(&name); err != nil {
				r.Close()
				return sc, err
			}
			sc.indexes[name] = true
		}
		r.Close()
		if err := r.Err(); err != nil {
			return sc, err
		}
	}
	return sc, nil
}

// ── row scanning ──────────────────────────────────────────────────────────────

// scanRows reads SQL rows into Facet records, normalizing each column to the Go
// type the evaluator expects (int, string, bool) so a row from the database is
// indistinguishable from one just built in memory.
func scanRows(rows *sql.Rows, e ir.Entity) ([]any, error) {
	cols := columns(e)
	fb := fieldByName(e)
	out := []any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := record{}
		for i, c := range cols {
			rec[c] = normalize(vals[i], fb[c])
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// normalize coerces a scanned database value to the Go representation the rest of
// the runtime uses; a NULL becomes the column type's zero value.
func normalize(v any, f ir.Field) any {
	switch t := v.(type) {
	case nil:
		return zeroFor(f)
	case int64:
		return int(t)
	case float64:
		return int(t)
	case []byte:
		return decryptIf(f, string(t))
	case string:
		return decryptIf(f, t)
	default:
		return v
	}
}

// decryptIf decrypts a scanned value when its column is @secret, so the working
// set holds plaintext while the database holds ciphertext.
func decryptIf(f ir.Field, s string) string {
	if f.Secret {
		return decryptSecret(s)
	}
	return s
}

func zeroFor(f ir.Field) any {
	switch {
	case f.IsRelation() || f.Type == "int":
		return 0
	case f.Type == "bool":
		return false
	default:
		return ""
	}
}
