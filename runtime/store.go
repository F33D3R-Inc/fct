package runtime

import (
	"database/sql"
	"encoding/json"
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
// of truth across restarts. The backend is Postgres; you point at it with
// FACET_DATABASE_URL.
type Store interface {
	// Init ensures a table exists for every entity and returns all existing rows
	// (entity name -> rows), so the server can seed its in-memory working set.
	Init(entities []ir.Entity) (map[string][]any, error)
	// Save inserts or updates one row (keyed by its "id").
	Save(entity string, row map[string]any) error
	// Delete removes one row by id.
	Delete(entity string, id any) error
	// Clear empties an entity.
	Clear(entity string) error
	Close() error
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

// ── Postgres backend ──────────────────────────────────────────────────────────

// pgStore persists each entity as a small table of (id, JSON document) rows.
// Storing the row as a JSON document means adding or removing a field never needs
// a migration. (Indexed per-column querying is a later layer; today all reads are
// served from the in-memory working set.)
type pgStore struct {
	db *sql.DB
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
	return &pgStore{db: db}, nil
}

// table is the physical table name for an entity (entity names are validated
// identifiers, so this needs no escaping).
func table(entity string) string { return "facet_" + entity }

func (s *pgStore) Init(entities []ir.Entity) (map[string][]any, error) {
	out := map[string][]any{}
	for _, e := range entities {
		t := table(e.Name)
		if _, err := s.db.Exec(fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s (id BIGINT PRIMARY KEY, data TEXT NOT NULL)`, t)); err != nil {
			return nil, fmt.Errorf("create %s: %w", t, err)
		}
		rows, err := s.db.Query(fmt.Sprintf(`SELECT data FROM %s ORDER BY id`, t))
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", t, err)
		}
		list := []any{}
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return nil, err
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(data), &rec); err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, record(rec))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out[e.Name] = list
	}
	return out, nil
}

func (s *pgStore) Save(entity string, row map[string]any) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	q := fmt.Sprintf(
		`INSERT INTO %s (id, data) VALUES ($1, $2) ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`,
		table(entity))
	_, err = s.db.Exec(q, toInt(row["id"]), string(data))
	return err
}

func (s *pgStore) Delete(entity string, id any) error {
	_, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table(entity)), toInt(id))
	return err
}

func (s *pgStore) Clear(entity string) error {
	_, err := s.db.Exec(fmt.Sprintf(`DELETE FROM %s`, table(entity)))
	return err
}

func (s *pgStore) Close() error { return s.db.Close() }
