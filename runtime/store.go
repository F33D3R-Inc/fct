package runtime

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"facet/internal/ir"

	_ "github.com/lib/pq" // postgres driver ("postgres")
)

// StoreDescription summarizes where this app's data will live, for the startup
// banner — read from FACET_DATABASE_URL.
func StoreDescription(app string) string {
	if url := os.Getenv("FACET_DATABASE_URL"); strings.HasPrefix(url, "postgres") {
		return "postgres"
	}
	return "postgres (FACET_DATABASE_URL not set)"
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
	// Delete removes one row by id. A relation's ON DELETE CASCADE drops the
	// rows that referenced it.
	Delete(entity string, id any) error
	// Clear empties an entity (cascading to its children).
	Clear(entity string) error
	// Query runs an indexed, paginated read pushed down to SQL, returning the page
	// of rows and an opaque cursor for the next page ("" if the page is the last).
	Query(query Query) ([]any, string, error)
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
	Close() error
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
	if url == "" {
		return nil, fmt.Errorf("FACET_DATABASE_URL is not set; point it at Postgres, e.g. postgres://user:pw@localhost:5432/dbname")
	}
	if !strings.HasPrefix(url, "postgres://") && !strings.HasPrefix(url, "postgresql://") {
		return nil, fmt.Errorf("FACET_DATABASE_URL %q is not a Postgres URL (expected postgres://…)", url)
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
