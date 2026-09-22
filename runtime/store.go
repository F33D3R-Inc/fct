package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"facet/internal/ir"
)

// StoreDescription summarizes where this app's data will live, for the startup
// banner — read from FACET_DATABASE_URL.
func StoreDescription(app string) string {
	if strings.HasPrefix(os.Getenv("FACET_DATABASE_URL"), "facetql://") {
		return "facetql"
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
// sub-linear as a table grows past what fits in memory. The backend is FacetQL,
// the stack's native database (AGENT_LOG §2); you point at it with
// FACET_DATABASE_URL (facetql://…). Postgres support was excised once FacetQL
// reached parity — see AGENT_LOG.md, "Changelog — 2026-09-06 (evening)".
type Store interface {
	// Init brings the schema up to date (the same additive migration Migrate
	// applies) and returns every existing row (entity name -> rows) to seed the
	// in-memory working set.
	Init(entities []ir.Entity) (map[string][]any, error)
	// Save inserts or updates one row (keyed by its "id").
	Save(entity string, row map[string]any) error
	// Delete removes one row by id, and with it every row that referenced it
	// (ir.References, recursively). The cascade is the store's, not the caller's:
	// fqStore declares it to FacetQL as a reference the engine expands into the
	// same transaction as the delete, and memStore applies it to its own rows —
	// so the children can never be left behind by a crash between two requests.
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

	// Notify publishes a payload on the cross-instance event channel (FacetQL's
	// POST /publish / GET /events feed — see cluster.go), so every instance's
	// live clients converge.
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
	Visitor    string         `json:"visitor,omitempty"` // the stable `session` key (sessionState.visitor)
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

// openStore connects to FacetQL from FACET_DATABASE_URL. An unset URL points
// at a local FacetQL instead of erroring, so `facet dev`/`facet run` work
// against a freshly-installed FacetQL with no configuration at all.
//
//	FACET_DATABASE_URL=facetql://[token@]host:port
func openStore(url string) (Store, error) {
	if url == "" {
		url = "facetql://localhost:8080"
	}
	if !strings.HasPrefix(url, "facetql://") {
		return nil, fmt.Errorf("FACET_DATABASE_URL %q is not a FacetQL (facetql://…) URL", url)
	}
	return openFacetQL(url)
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

// normalize coerces a value read back from the store to the Go representation
// the rest of the runtime uses; an absent value becomes the column type's
// zero value. fqStore's nodeRecord is this function's only caller now that
// the store that once scanned SQL rows through it is gone.
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

// decryptIf decrypts a value when its column is @secret, so the working set
// holds plaintext while the store holds ciphertext.
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
