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
// The whole Store surface is implemented natively: rows (Save/Delete/Clear/Load/
// Query/Count), transactions, and the reserved kinds behind audit, sessions,
// jobs and cron. Nothing here emulates a primitive the engine should own — each
// method is one FacetQL request against a native op (clear_kind, delete_where,
// claim, set_if, the keyset cursor), which is why there are no stubs left.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
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

// fqAddressWidth is how many digits a row id is padded to inside an address.
//
// 19 is the digits in math.MaxInt64, so every id an int64 can hold pads to the
// same width and none is ever truncated.
const fqAddressWidth = 19

// fqAddress is the node address for a row: "<Entity>:<zero-padded id>".
//
// # Why the padding
//
// The address is FacetQL's primary key and its native ordering, and that
// ordering is over *strings*. A row id is an integer. Unpadded, the mapping
// between them does not preserve order — `Tweet:9` sorts after `Tweet:12` —
// so "the newest posts" silently returned the wrong ones: twelve posts, ask
// for the newest five by id, and the answer was 9, 8, 7, 6, 5. Not an error,
// not an empty page; the wrong five, with a 200.
//
// The reasoning that let this through is a Postgres assumption that leaked
// into the FacetQL path: identity is already the store's primary order, so
// ordering by id needs nothing extra. True of an integer primary key. False
// of a string address, which is what identity is here.
//
// Padding restores the property, and the codebase already knew that — `Audit`
// writes `__audit:%019d` for exactly this reason. Entity rows just never got
// it. It costs nothing at read time: address order is FacetQL's native order,
// so `by=id` needs no index and no sort.
//
// The id is read back from the node's `data`, never parsed out of the address
// (see nodeRecord), so the padding is invisible above this function.
//
// MIGRATION: this changes the address of every existing row. A database
// written before this change has rows at `Tweet:9` that will not be found at
// `Tweet:0000000000000000009`. Data written by an earlier build must be
// re-imported.
func fqAddress(entity string, id any) string {
	return fmt.Sprintf("%s:%0*d", entity, fqAddressWidth, toInt(id))
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
//
// It pages with the opaque keyset cursor (POST /nodes/query, `after` -> `next`),
// not with an offset: offset paging re-reads and discards every row before the
// page, so its cost grows with the page number, and FacetQL bounds it at
// FACETQL_MAX_QUERY_OFFSET (10 000) — which made booting an app with more rows
// than that in one entity fail outright. The cursor costs the same at any depth
// and has no ceiling. Order "id" is the base (address) order, the same order
// pgStore.loadAll reads its table in.
func (s *fqStore) loadAll(e ir.Entity) ([]any, error) {
	ctx := context.Background()
	out := []any{}
	after := ""
	for {
		nodes, next, err := s.c.query(ctx, fqQueryRequest{
			Kind: e.Name, ItemVar: "item", Order: "id", Desc: false, Limit: fqPageSize, After: after,
		})
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
		if next == "" || len(nodes) == 0 {
			break
		}
		after = next
	}
	return out, nil
}

// ── green methods ───────────────────────────────────────────────────────────

func (s *fqStore) Init(entities []ir.Entity) (map[string][]any, error) {
	// FacetQL is schemaless, so "bring the schema up to date" is remembering the
	// entity definitions and declaring the rules this app depends on — the access
	// paths its reads need and the references its deletes cascade along. That is
	// the same Migrate the CLI runs, exactly as pgStore.Init applies its own DDL.
	//
	// Declaring them is best-effort, and deliberately so. Those endpoints are
	// admin-only, and an app is not an operator. Booting must not start requiring
	// a privilege booting never required — before indexes existed, Init touched no
	// admin endpoint at all — so an identity that may not reconcile them starts
	// against whatever the operator has already declared, saying so once, rather
	// than refusing to start.
	//
	// That trade is easy for an index (slower, not wrong) and genuinely uneasy for
	// a reference (an undeclared one orphans rows). It is still the right one, for
	// a reason that is about what this identity can *observe*, not about severity:
	// GET /admin/references is admin-only too, so a non-admin app cannot tell "the
	// operator declared every rule and handed me an unprivileged token" — the
	// deployment the gate is designed for — from "nothing is declared". Refusing
	// to boot would refuse the correctly-configured case, using evidence it does
	// not have. So it boots, and the notice names the integrity consequence
	// instead of only the performance one. `facet migrate`, where an operator
	// asked for the reconcile explicitly, still fails hard.
	//
	// An undeclared reference — a cascade rule FacetQL still holds over one of
	// this app's kinds that the app no longer declares — is tolerated here on the
	// same terms and for a different reason. It is not a privilege problem: the
	// reconcile saw the rule and could describe it exactly. It is that the only
	// two things to do about it are both worse than saying so. Dropping it would
	// make an app deploy silently delete a data-integrity rule the app cannot
	// prove it authored (see errFQUndeclaredReference); refusing to boot would
	// take down an app that was serving a minute ago, over a rule that has been
	// there since before this release and that the app has no privilege to fix.
	// So Init warns — every boot, until it is gone — and `facet migrate`, where an
	// operator asked for the reconcile explicitly and can act on the answer, fails
	// hard with the exact rules and the command that drops each. The reconcile has
	// already applied everything this app declares by the time this error is
	// returned, so booting past it is booting on a fully-declared set.
	//
	// Every other Migrate failure is still fatal: it means the reconcile itself
	// broke — or the data already violates a rule the app is declaring — and
	// booting on a half-declared set would hide that.
	if _, err := s.Migrate(entities, true); err != nil {
		switch {
		case errors.Is(err, errFQAdminOnly):
			fqWarnAdminOnly(err)
		case errors.Is(err, errFQUndeclaredReference):
			fqWarnUndeclaredReference(err)
		default:
			return nil, err
		}
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

func (s *fqStore) Save(entity string, row map[string]any) error {
	e := s.ents[entity]
	n, err := rowNode(e, row)
	if err != nil {
		return err
	}
	return s.c.upsert(context.Background(), n)
}

// Delete removes one row by id — one request, and nothing else.
//
// The rows that referenced it go with it, and this function does not mention them
// because it must not: the cascade is a rule the engine holds (declared by
// Migrate as a FacetQL reference), expanded into the same frame as this delete, so
// a crash cannot land between the parent and its children. Deleting the children
// from here would be the other thing — two transactions and a window — and it is
// what "emulate ON DELETE CASCADE in the adapter" would have meant.
func (s *fqStore) Delete(entity string, id any) error {
	return s.c.deleteNode(context.Background(), fqAddress(entity, id))
}

// Clear removes every node of a kind in ONE request.
//
// It used to page the kind and issue a DELETE per node: N round trips, and not
// atomic — a failure partway left the kind half-emptied with no way to tell how
// far it got. FacetQL has a native `clear_kind` transaction op for exactly this,
// with the same WAL framing and all-or-nothing semantics as any other batch, and
// `fqTx.Clear` has always used it. This is the last caller that did not, which
// made "clear a kind" mean two different things depending on whether a
// transaction happened to be open.
//
// Authorization is the engine's and is unchanged by the switch: a non-admin
// clears only the nodes it owns, an admin clears the kind. Nodes it may not
// write are skipped, not an error.
func (s *fqStore) Clear(entity string) error {
	return s.c.transaction(context.Background(), []fqTxOp{
		{Type: "clear_kind", Kind: entity},
	})
}

// errFQAdminOnly is the one FacetQL refusal that is not this app's to fix: its
// identity is not an admin, and the endpoints that declare what this app needs —
// /admin/indexes for access paths, /admin/references for cascade rules — are both
// admin-only. It says what to change and where, because "HTTP 403: admin only"
// out of a boot path names neither.
//
// References are gated for a sharper reason than indexes: a referential action
// runs with the authority of the declaration, not of the caller, so an
// application able to declare its own could arrange for another owner's rows to
// be deleted (facetql storage/reference.rs).
//
// It is reached by classification, not by matching a message: the client returns
// a typed fqHTTPError carrying the status, fqAdminError turns a 403 from those
// endpoints into this, and callers use errors.Is.
var errFQAdminOnly = errors.New(
	"declaring FacetQL indexes and references needs an admin identity, and this token is not one " +
		"(FACETQL_TOKEN, or the token in FACET_DATABASE_URL): /admin/indexes and /admin/references are admin-only")

// fqAdminError classifies a failure from FacetQL's admin endpoints. A 403 there
// means exactly one thing — this identity is not an admin — so it becomes
// errFQAdminOnly, which a caller can act on; anything else stays the failure it
// already was, wrapped with what was being attempted.
func fqAdminError(what string, err error) error {
	if fqStatus(err) == http.StatusForbidden {
		return fmt.Errorf("%s: %w (%v)", what, errFQAdminOnly, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// fqAdminOnce keeps the notice below to one line per process: Init runs on every
// boot and on every dev reload, and a warning repeated on a loop is a warning
// nobody reads.
var fqAdminOnce sync.Once

// fqWarnAdminOnly reports, once, that the app booted without reconciling what it
// declares — what each half costs, and what to run to fix it. The two halves cost
// differently and the notice says so: an app with no index is slower, an app with
// no reference rule is wrong, and an operator has to be able to tell which risk
// they just accepted.
func fqWarnAdminOnly(err error) {
	fqAdminOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"facet: FacetQL indexes were not reconciled and neither were this app's reference rules — %v. "+
				"Starting with whatever FacetQL already has: a query that orders or filters on an "+
				"undeclared field scans the whole kind and fails past FACETQL_MAX_SCAN_ROWS, and — the "+
				"one that is wrong rather than slow — deleting a row whose reference is undeclared "+
				"leaves the rows that pointed at it behind, so a restart brings them back. Run "+
				"`facet migrate <app.fct>` with an admin FACETQL_TOKEN to declare them.\n", err)
	})
}

// errFQUndeclaredReference is the other refusal an operator can act on: FacetQL
// holds a referential rule over a kind this app owns that this app no longer
// declares. It is a value rather than a message for the same reason
// errFQAdminOnly is — Init and `facet migrate` have to answer it differently,
// and they must tell it apart from a reconcile that broke.
//
// WHY IT IS NEVER DROPPED AUTOMATICALLY. A reference is a data-integrity rule,
// and this app cannot see who declared it: /admin/references reports the rule,
// not its author, so "the relation I removed last week" and "the operator's own
// rule over a field my schema does not model" arrive as the same JSON. Dropping
// on that evidence would turn an app deploy into a silent, unannounced weakening
// of the engine's integrity guarantees — the failure mode migrateIndexes already
// refuses ("an index or rule this app did not ask for may be an operator's, so
// extras are reported, never removed"), and the one dropIndex's comment states
// outright. Deleting a rule is an operator decision.
//
// WHY SILENCE IS NOT AN OPTION EITHER, WHICH IS THE PART THAT WAS MISSING. An
// undeclared *index* is inert: it costs disk and nothing else, so a plan line is
// a complete answer. An undeclared *reference* is not inert — it keeps cascading
// deletes through a relation the application has removed, so a `delete` the
// author believes touches one row destroys the rows that used to point at it. A
// storefront lost a live order to exactly this: `Order.product` was changed from
// a relation to an `int`, migrate reported only a stray index, and
// fk_Order_product went on cascading. The rule outlived the schema that created
// it, and nothing said so.
//
// So: reported, loudly, always — and refused where a refusal is the honest
// answer. See fqUndeclaredReferenceError for the message, and Init for the one
// caller that downgrades it.
var errFQUndeclaredReference = errors.New(
	"FacetQL holds a referential rule this app does not declare, so a delete still cascades " +
		"through a relation the schema has removed")

// fqUndeclaredOnce keeps the boot notice below to one line per process, for the
// same reason fqAdminOnce does.
var fqUndeclaredOnce sync.Once

// fqWarnUndeclaredReference reports, once, that the app booted with a cascade
// rule its schema no longer declares. It is a warning and not a refusal because
// of the asymmetry Init already documents: booting is not the moment an operator
// asked for a reconcile, refusing to start would break an app that was running a
// minute ago, and the app cannot repair the rule itself even if it wanted to
// (/admin/references is admin-only). What it can do is make the window visible
// on every boot until someone closes it, naming the exact command that closes it.
func fqWarnUndeclaredReference(err error) {
	fqUndeclaredOnce.Do(func() {
		fmt.Fprintf(os.Stderr,
			"facet: %v\nStarting anyway — the rule is FacetQL's and dropping one is an operator "+
				"decision, not a consequence of running an app — but until it is dropped, deleting a "+
				"parent row DELETES the rows that reference it through a relation this app no longer "+
				"has, and nothing in this app will mention them. Run `facet migrate <app.fct>` with an "+
				"admin FACETQL_TOKEN to see the exact rules and the command that drops each.\n", err)
	})
}

// fqReservedIndexes are the access paths fqStore's own reserved kinds need, in
// addition to the app's. They are the FacetQL twins of the operational indexes
// pgStore.Migrate creates for facet_jobs/facet_sessions: ClaimJob pins `status`
// to one value and orders by `run_at_unix`, and without an index each of those
// is a scan of every job that ever existed. (__audit needs none — it orders by
// address, FacetQL's native order — and the session purge compares `<`, which
// the engine does not narrow with an index.)
var fqReservedIndexes = []ir.Index{
	{Entity: "__job", Field: "status"},
	{Entity: "__job", Field: "run_at_unix"},
}

// fqIndexWant is one index this app needs and whether it has to be unique.
//
// Uniqueness is not a second kind of object: FacetQL declares it on the index,
// because the check *is* a prefix scan of that index. fct needs exactly one
// unique index, over a referenced entity's `id` — see fqWantedIndexes.
type fqIndexWant struct {
	ir.Index
	Unique bool
}

// fqRelationKeyField is the field on a parent node that a child's relation value
// matches: the row's own id, which fct writes into `data` and derives the node's
// address from. A FacetQL reference defaults to matching the parent's *address*
// and needs nothing declared for it; fct cannot use that default, because a
// relation field holds the bare integer id (`{"tweet": 12}`), not the address
// ("Tweet:0000000000000000012"). Referencing the id field instead is the shape
// the engine documents for exactly this case, and it is what makes the FacetQL
// rule agree with the Postgres one, which is also `REFERENCES facet_X(id)`.
const fqRelationKeyField = "id"

// fqWantedIndexes is every index the app needs, deduplicated by (kind, field):
// the compiler's derived set, fqStore's own operational ones, and — the reason
// this is a function rather than a concatenation — a UNIQUE index over the `id`
// of every entity something references.
//
// That last group exists because of the rule it enables, not for reads. FacetQL
// refuses a reference by a data field unless that field carries a unique index:
// a reference has to name exactly one node, and a value two nodes can hold names
// neither. It is also the only reason fct ever indexes an `id` — ir.Indexes
// deliberately never does, since identity is already the store's primary order.
func fqWantedIndexes(entities []ir.Entity) []fqIndexWant {
	var out []fqIndexWant
	at := map[ir.Index]int{}
	add := func(ix ir.Index, unique bool) {
		if i, seen := at[ix]; seen {
			out[i].Unique = out[i].Unique || unique // two wants, one index, strictest wins
			return
		}
		at[ix] = len(out)
		out = append(out, fqIndexWant{Index: ix, Unique: unique})
	}
	for _, ix := range ir.Indexes(entities) {
		add(ix, false)
	}
	for _, ix := range fqReservedIndexes {
		add(ix, false)
	}
	for _, r := range ir.References(entities) {
		add(ir.Index{Entity: r.Parent, Field: fqRelationKeyField}, true)
	}
	return out
}

// Migrate declares to FacetQL the two things about this app's data that the
// engine cannot infer from schemaless JSON: which fields need an access path, and
// which fields are references whose parent's deletion takes the row with it.
//
// There is still no DDL to plan (AGENT_LOG §2, "What gets EASIER") — fields live
// in the node's `data`. What is left is not shape, it is rules:
//
//   - an index, because ordering or filtering a query by a data field
//     materializes and sorts the whole kind without one, and fails outright past
//     FACETQL_MAX_SCAN_ROWS. The compiler already derived that set (ir.Indexes:
//     every ordered, filtered, or relation field).
//   - a reference, because a relation field is a foreign key with ON DELETE
//     CASCADE (ir.Field) and the engine is the only place that rule can be
//     enforced atomically. The compiler already derived that set too
//     (ir.References). This is what pgStore's addFKSQL states in SQL.
//
// Both reconciles are keyed on (kind, field), not on the object's name: covering
// a field is what matters, and an operator's own index under their own name
// already satisfies us. Nothing is ever dropped — an index or rule this app did
// not ask for may be an operator's, so extras are reported, never removed — and
// re-declaring an identical one is a successful no-op on the engine, which is
// what makes this safe to run on every boot. A rule that *contradicts* one this
// app needs is the exception: it is an error naming what to drop, because
// silently accepting it would silently change whether a delete removes rows.
//
// The two halves report an extra differently, and the difference is what the
// extra COSTS. An index this app does not declare is inert, so it is a plan line
// and migrate still succeeds. A reference this app does not declare is not inert:
// it goes on cascading deletes through a relation the schema has removed, which
// is a live data-loss window, so it is a plan line AND a refusal — see
// errFQUndeclaredReference. Neither is ever removed for the app's convenience.
//
// The endpoints it uses are admin-only, so this needs an admin identity. When the
// token is not one, the reconcile fails with errFQAdminOnly and nothing is
// declared: `facet migrate` reports that as the actionable failure it is, while
// Init treats it as non-fatal and boots anyway (see Init).
func (s *fqStore) Migrate(entities []ir.Entity, apply bool) ([]string, error) {
	if apply {
		s.setEntities(entities)
	}
	ctx := context.Background()
	plan, err := s.migrateIndexes(ctx, entities, apply)
	if err != nil {
		return plan, err
	}
	// References come second and cannot be reordered: the engine refuses a
	// reference whose access paths are not already declared, and one of them is
	// the unique index over the parent's id that the pass above creates.
	return s.migrateReferences(ctx, entities, apply, plan)
}

// migrateIndexes reconciles the declared secondary indexes with fqWantedIndexes.
func (s *fqStore) migrateIndexes(ctx context.Context, entities []ir.Entity, apply bool) ([]string, error) {
	have, err := s.c.listIndexes(ctx)
	if err != nil {
		return nil, fqAdminError("list FacetQL indexes", err)
	}
	covered := map[ir.Index]fqIndexDef{}
	for _, d := range have {
		covered[ir.Index{Entity: d.Kind, Field: d.Field}] = d
	}
	var plan []string
	wanted := map[ir.Index]bool{}
	for _, w := range fqWantedIndexes(entities) {
		wanted[w.Index] = true
		what := fmt.Sprintf("index on %s.%s", w.Entity, w.Field)
		if w.Unique {
			what = "unique " + what
		}
		if d, ok := covered[w.Index]; ok {
			// An index that covers the field but does not make it unique cannot
			// be upgraded in place, and this app never drops an index it did not
			// declare — so say what has to happen instead of failing later, with
			// the engine's refusal, in the middle of declaring a reference.
			if w.Unique && !d.Unique {
				return plan, fmt.Errorf(
					"%s: index %q already covers %s.%s but does not make it unique, and a reference to %s "+
						"needs it to (a value two rows can hold names neither of them). Drop it "+
						"(`facetql index drop %s`) and re-run migrate",
					what, d.Name, d.Kind, d.Field, w.Entity, d.Name)
			}
			continue
		}
		def := fqIndexDef{Name: fqIndexName(w.Entity, w.Field), Kind: w.Entity, Field: w.Field, Unique: w.Unique}
		covered[w.Index] = def // two wants for one field are still one index
		plan = append(plan, "create "+what)
		if !apply {
			continue
		}
		if err := s.c.createIndex(ctx, def); err != nil {
			return plan, fqAdminError("create "+what, err)
		}
	}
	for _, d := range have {
		if !wanted[ir.Index{Entity: d.Kind, Field: d.Field}] {
			plan = append(plan, fmt.Sprintf("index %q on %s.%s is not declared by this app — left in place",
				d.Name, d.Kind, d.Field))
		}
	}
	return plan, nil
}

// migrateReferences declares this app's relations to FacetQL as durable
// referential rules, so the ENGINE cascades a delete to the rows that referenced
// it — the same rule pgStore states as `REFERENCES … ON DELETE CASCADE`.
//
// This is the half of "delete a row and its children go with it" that fct cannot
// implement correctly from outside, and until the engine grew references it did
// not have: the children have to go in the SAME frame as the parent, or a crash
// between the two requests leaves rows nothing can reach; and finding them at all
// means either an index the engine owns or a copy of the graph in the app's
// memory. The runtime does keep the reverse graph (Server.children) — but for
// pruning its own in-memory working set, which is a cache of the store, never to
// issue the child deletes. Those are the engine's, on every path that deletes:
// DELETE /node, delete_node, clear_kind and delete_where all lower through one
// site in FacetQL, so the rule cannot hold on one route and not another.
//
// The reconcile is keyed on the referencing (kind, field) — the side that carries
// the value — because that is what identifies the rule; a second rule over the
// same field would be a second cascade, not a re-declaration. A rule already
// there that says something *different* about that field is a contradiction this
// app will not silently accept, since it decides whether a delete removes rows.
func (s *fqStore) migrateReferences(ctx context.Context, entities []ir.Entity, apply bool, plan []string) ([]string, error) {
	refs := ir.References(entities)
	// The listing happens even when this app declares no relations at all. It used
	// to be skipped ("nothing to declare, and no admin call to make"), and that
	// skip is precisely how an app that removes its last relation stops being told
	// about the rule that outlived it — the reconcile has to look at what is there
	// before it can know there is nothing extra.
	have, err := s.c.listReferences(ctx)
	if err != nil {
		// An engine with no /admin/references at all is not a privilege problem
		// and is not tolerable the way a missing index is: this app has relations,
		// and an engine that cannot hold the rule cannot enforce it, so every
		// delete would leave the rows that pointed at it behind. Unlike the 403,
		// there is nothing ambiguous to respect here — the app can see that the
		// rule cannot exist — so it says which side is out of date and stops.
		if fqStatus(err) == http.StatusNotFound {
			if len(refs) == 0 {
				// An engine that cannot hold a rule also cannot be holding a stale
				// one, and this app needs none: nothing to declare, nothing to
				// reconcile away, no reason to fail.
				return plan, nil
			}
			return plan, fmt.Errorf(
				"this FacetQL has no /admin/references: it predates referential integrity, so it cannot "+
					"cascade a delete to the rows that referenced it and this app has %d relation(s) that "+
					"depend on that. Upgrade the engine (%w)", len(refs), err)
		}
		return plan, fqAdminError("list FacetQL references", err)
	}
	declared := map[ir.Index]fqReferenceDef{}
	for _, d := range have {
		declared[ir.Index{Entity: d.Kind, Field: d.Field}] = d
	}
	for _, r := range refs {
		def := fqReferenceDef{
			Name:        fqReferenceName(r.Entity, r.Field),
			Kind:        r.Entity,
			Field:       r.Field,
			ParentKind:  r.Parent,
			ParentField: fqRelationKeyField,
			OnDelete:    "cascade",
		}
		what := fmt.Sprintf("reference %s.%s -> %s.%s on delete cascade",
			r.Entity, r.Field, r.Parent, fqRelationKeyField)
		if d, ok := declared[ir.Index{Entity: r.Entity, Field: r.Field}]; ok {
			if d.ParentKind != def.ParentKind || d.ParentField != def.ParentField || d.OnDelete != def.OnDelete {
				return plan, fmt.Errorf(
					"declare %s: reference %q already governs %s.%s, but as %s.%s on delete %s — "+
						"one field cannot have two deletion rules. Drop it "+
						"(`facetql reference drop %s`) and re-run migrate",
					what, d.Name, d.Kind, d.Field, d.ParentKind, d.ParentField, d.OnDelete, d.Name)
			}
			continue
		}
		declared[ir.Index{Entity: r.Entity, Field: r.Field}] = def
		plan = append(plan, "declare "+what)
		if !apply {
			continue
		}
		if err := s.c.createReference(ctx, def); err != nil {
			return plan, fqAdminError("declare "+what, err)
		}
	}

	// Everything this app declares is now declared. What is left is the other
	// direction — a rule the engine holds that this app does not — and it is
	// checked last on purpose: the additive half of the reconcile is complete and
	// durable before the report below can stop the caller, so an operator who
	// fixes the rule and re-runs is not re-running work that already succeeded.
	stale := fqUndeclaredReferences(entities, refs, have)
	for _, d := range stale {
		plan = append(plan, fmt.Sprintf(
			"reference %q still governs %s.%s -> %s.%s on delete %s, which this app does not declare — "+
				"drop it with `facetql reference drop %s`",
			d.Name, d.Kind, d.Field, d.ParentKind, d.ParentField, d.OnDelete, d.Name))
	}
	if len(stale) > 0 {
		return plan, fqUndeclaredReferenceError(stale)
	}
	return plan, nil
}

// fqUndeclaredReferences is the set of referential rules FacetQL holds over kinds
// THIS APP OWNS that this app does not declare.
//
// The ownership test is the whole reason this is a function and not an inlined
// loop, because getting it wrong breaks the case it is meant to serve. One
// FacetQL can hold many applications; a rule over some other app's kind is not
// this app's to report, let alone to refuse over, and reporting it would make a
// shared engine impossible to migrate against. The referencing side (`Kind`) is
// what decides, for the same reason it is what identifies a rule: it names the
// kind whose rows the cascade deletes, and this app is the one that declares —
// and deletes — those rows. A rule pointing AT one of our kinds from someone
// else's child kind is theirs; its cascade removes their rows, not ours.
func fqUndeclaredReferences(entities []ir.Entity, refs []ir.Reference, have []fqReferenceDef) []fqReferenceDef {
	mine := make(map[string]bool, len(entities))
	for _, e := range entities {
		mine[e.Name] = true
	}
	wanted := make(map[ir.Index]bool, len(refs))
	for _, r := range refs {
		wanted[ir.Index{Entity: r.Entity, Field: r.Field}] = true
	}
	var stale []fqReferenceDef
	for _, d := range have {
		if mine[d.Kind] && !wanted[ir.Index{Entity: d.Kind, Field: d.Field}] {
			stale = append(stale, d)
		}
	}
	return stale
}

// fqUndeclaredReferenceError is what an operator sees. It names every stale rule,
// what each one still does to their data, and the exact command that removes it —
// because the reason this bug cost a live row is that the reference was never
// mentioned at all, and a report an operator cannot act on is barely better.
func fqUndeclaredReferenceError(stale []fqReferenceDef) error {
	var b strings.Builder
	for _, d := range stale {
		fmt.Fprintf(&b, "\n  reference %q: deleting a %s still deletes every %s whose %q matches it "+
			"(on delete %s), and this app declares no relation %s.%s. "+
			"If the relation was removed from the schema, drop the rule: `facetql reference drop %s`. "+
			"If the rule is the operator's and deliberate, the app has to declare the relation for "+
			"migrate to recognise it.",
			d.Name, d.ParentKind, d.Kind, d.Field, d.OnDelete, d.Kind, d.Field, d.Name)
	}
	// The sentinel goes through %w, so Init and `facet migrate` classify this by
	// value rather than by matching the text above.
	return fmt.Errorf("%w:%s", errFQUndeclaredReference, b.String())
}

// fqMaxIndexName is FacetQL's limit on a declared object's name: an index name
// becomes a filename and a reference name becomes a URL path segment, so the
// engine admits at most 64 bytes of [A-Za-z0-9_-] for either and rejects anything
// else (MAX_REFERENCE_NAME_LEN is the same 64, for the same reason).
const fqMaxIndexName = 64

// fqIndexName is the FacetQL name for the index over entity.field: the same base
// name pgStore gives the column's index (indexName), reduced to the engine's
// filename alphabet. A name that had to be changed, or that is too long, keeps
// its identity through a digest of the exact (entity, field) pair rather than
// colliding with its neighbours. It is a pure function of the pair — the same
// pair yields the same name on every boot and every instance, which is what lets
// the engine recognize a re-declaration as a no-op instead of a second index.
func fqIndexName(entity, field string) string {
	return fqSafeName(indexName(entity, field), entity, field)
}

// fqReferenceName is the FacetQL name for the reference over entity.field: the
// same base name pgStore gives the relation's foreign key (fkName), through the
// same folding. FacetQL bounds a reference name exactly as it bounds an index
// name, and for the same reason — it is a URL path segment.
func fqReferenceName(entity, field string) string {
	return fqSafeName(fkName(entity, field), entity, field)
}

// fqSafeName reduces raw to the engine's filename/path alphabet, keeping identity
// through a digest of the exact (entity, field) pair when it has to change or
// truncate. Callers pass a distinct base name per object kind, so an index and a
// reference over one pair never collide.
func fqSafeName(raw, entity, field string) string {
	safe := []byte(raw)
	for i, b := range safe {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
			safe[i] = '_'
		}
	}
	if string(safe) == raw && len(safe) <= fqMaxIndexName {
		return raw
	}
	sum := sha256.Sum256([]byte(entity + "\x00" + field))
	suffix := "-" + hex.EncodeToString(sum[:4])
	if len(safe) > fqMaxIndexName-len(suffix) {
		safe = safe[:fqMaxIndexName-len(suffix)]
	}
	return string(safe) + suffix
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
//
// SAVING THE SAME ROW TWICE IS ONE UPSERT. `live` tracks, per address, the
// buffered insert_node that currently speaks for it, and a second Save to that
// address replaces it instead of appending a second op. Without that, an action
// as ordinary as
//
//	set Post(id).title = title
//	set Post(id).body  = body
//
// buffered two insert_node ops at the same address and FacetQL refused the whole
// batch — `unique index "idx_Post_id": this batch gives both "Post:…2" and
// "Post:…2" the same "id"` — which is a rejection the entity earned precisely by
// being a relation TARGET, since that is what gives it a unique id index
// (fqWantedIndexes). So the entities a real application relates things to were
// exactly the ones a multi-field update could not be written for.
//
// Replacement is the whole collapse, and it is not a partial merge, because
// `rowNode` encodes EVERY column of the row on every call: the later node
// already carries both fields. That is also what makes the three Store
// implementations agree — pgStore executes each Save as it arrives (the second
// UPSERT overwrites the first) and memStore replays each Save in order (the last
// one wins), so all three now end a batch with one row holding the last value
// written to each field.
//
// A Delete or a Clear that could touch a buffered address drops it from `live`,
// so a later Save appends a fresh op AFTER them rather than reviving one that
// sits before them: the collapse only ever merges two writes with nothing in
// between that could have changed the answer.
type fqTx struct {
	s    *fqStore
	ops  []fqTxOp
	live map[string]int // node address -> index in ops of the insert that speaks for it
	err  error          // first encoding error; surfaced at Commit
}

func (s *fqStore) Begin() (Tx, error) { return &fqTx{s: s, live: map[string]int{}}, nil }

func (t *fqTx) Save(entity string, row map[string]any) error {
	if t.err != nil {
		return t.err
	}
	n, err := rowNode(t.s.ents[entity], row)
	if err != nil {
		t.err = err
		return err
	}
	op := fqTxOp{
		Type:    "insert_node",
		Address: n.Address,
		Kind:    n.Kind,
		X:       n.X,
		Y:       n.Y,
		Z:       n.Z,
		Q:       n.Q,
		Data:    n.Data,
		Public:  n.Public,
	}
	if i, buffered := t.live[n.Address]; buffered {
		t.ops[i] = op // same row, later write: it supersedes the earlier one in place
		return nil
	}
	t.live[n.Address] = len(t.ops)
	t.ops = append(t.ops, op)
	return nil
}

func (t *fqTx) Delete(entity string, id any) error {
	addr := fqAddress(entity, id)
	delete(t.live, addr) // a Save after this delete is a new fact, not an amendment
	t.ops = append(t.ops, fqTxOp{Type: "delete_node", Address: addr})
	return nil
}

// Clear buffers a single native clear_kind op (AGENT_LOG §4b): the FacetQL engine
// removes every node of the kind atomically within the transaction. This is the
// root solution — it is NOT expanded into N delete_node ops.
func (t *fqTx) Clear(entity string) error {
	for addr, i := range t.live {
		if t.ops[i].Kind == entity {
			delete(t.live, addr) // the clear stands between this row and any later Save
		}
	}
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
	t.live = map[string]int{}
	t.err = nil
	return nil
}

// ── query / audit / sessions / jobs ─────────────────────────────────────────

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
	// A predicate that pins the row's identity is a point lookup, not a filter.
	if rows, handled, err := s.byIdentity(e, query); handled {
		if query.After != "" {
			return nil, "", nil // there is one row and the previous page had it
		}
		return rows, "", err
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

// Count answers `count(...)`/`exists(...)` without materializing a page.
//
// It is not bounded by FACETQL_MAX_SCAN_ROWS the way a query is: that bound
// exists because a result set is held in memory, and a count holds one integer
// however many rows it visits. Its cost is time — a predicate an index covers is
// the length of a key range, one the planner cannot use is a scan — so the
// caller's job is to make the predicate sargable, not to page it.
func (s *fqStore) Count(query Query) (int, error) {
	if _, ok := s.ents[query.Entity]; !ok {
		return 0, fmt.Errorf("fqStore.Count: unknown entity %q", query.Entity)
	}
	if rows, handled, err := s.byIdentity(s.ents[query.Entity], query); handled {
		return len(rows), err // a pinned identity matches one row or none
	}
	n, err := s.c.count(context.Background(), fqCountRequest{
		Kind:    query.Entity,
		Where:   query.Where,
		ItemVar: query.ItemVar,
	})
	if err != nil {
		return 0, fmt.Errorf("fqStore.Count: %w", err)
	}
	return n, nil
}

// CountBy answers one predicate for many pinned values of a field in a single
// request — the shape a rendered page needs, where the same aggregate appears
// once per row with a different id pinned into it.
//
// Passing the values is the whole point. Measured on 50 000 Likes over 20 000
// tweets, filling in one 20-post page: twenty separate counts 13.7 ms in twenty
// round trips, grouping the whole kind 153 ms in one (it computes 20 000 answers
// to use 20), pinned values 0.93 ms in one. The engine caps `values` at 1000;
// past that the grouped form is what is really being asked for.
func (s *fqStore) CountBy(query Query, groupBy string, values []any) (map[string]int, error) {
	if _, ok := s.ents[query.Entity]; !ok {
		return nil, fmt.Errorf("fqStore.CountBy: unknown entity %q", query.Entity)
	}
	out, err := s.c.countBy(context.Background(), fqCountByRequest{
		Kind:    query.Entity,
		Where:   query.Where,
		ItemVar: query.ItemVar,
		GroupBy: groupBy,
		Values:  values,
	})
	if err != nil {
		return nil, fmt.Errorf("fqStore.CountBy: %w", err)
	}
	return out, nil
}

// ── identity is the address, not a data field ───────────────────────────────
//
// A row's `id` is not a column here. fct writes each row as a node whose
// *address* is "<Entity>:<id>" (fqAddress), and the address is FacetQL's primary
// key; `data.id` is a copy of it carried inside the JSON, with no index over it —
// correctly, since the compiler does not declare an index over identity.
//
// The consequence, before this: `for t in Tweet where t.id == id` — the shape
// every detail route has — compiled to a predicate over an unindexed data field
// and scanned the kind. Measured on 50 000 rows it cost 0.74 s to find one row,
// and `/post/:id` paid it twice once counts moved to the engine. Recognising that
// the equality *is* the address turns both into one GET.

// identityPin pulls an `item.id == <literal>` conjunct out of a predicate,
// returning the pinned id and whatever the predicate asks besides it.
func identityPin(where *ir.Expr, itemVar string) (id any, rest *ir.Expr, found bool) {
	for _, c := range splitConj(where) {
		if v, ok := pinnedValue(c, itemVar, "id"); ok && !found {
			id, found = litValue(v), true
			continue
		}
		rest = andExpr(rest, c)
	}
	return id, rest, found
}

// byIdentity answers a query that pins one row's identity with a single node
// fetch. The rest of the predicate is applied to that one row in memory — one
// record, so there is nothing to push down and nothing to scan. `handled` is
// false when the predicate pins no identity, and the caller queries normally.
func (s *fqStore) byIdentity(e ir.Entity, query Query) (rows []any, handled bool, err error) {
	id, rest, found := identityPin(query.Where, query.ItemVar)
	if !found {
		return nil, false, nil
	}
	n, ok, err := s.c.getNode(context.Background(), fqAddress(e.Name, id))
	if err != nil {
		return nil, true, fmt.Errorf("fqStore: identity lookup %s: %w", fqAddress(e.Name, id), err)
	}
	if !ok {
		return nil, true, nil
	}
	rec, err := nodeRecord(e, n)
	if err != nil {
		return nil, true, fmt.Errorf("fqStore: identity lookup %s: %w", fqAddress(e.Name, id), err)
	}
	if rest != nil && !truthy(eval(rest, map[string]any{query.ItemVar: rec})) {
		return nil, true, nil
	}
	return []any{rec}, true, nil
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

// fqCronData is a `__cron` node's data: the schedule's name (the address carries
// it too, but a node an operator lists should say what it is) and the unix second
// before which no instance may take the next tick. `next_run` is the field
// ReserveCron's compare-and-set is conditioned on, and the only one it rewrites.
type fqCronData struct {
	Name        string `json:"name"`
	NextRunUnix int64  `json:"next_run"`
}

// fqCronAddr is the node address of a scheduled tick's reservation.
func fqCronAddr(name string) string { return "__cron:" + name }

// ReserveCron claims the right to enqueue one scheduled tick: it advances
// `__cron:<name>`'s next_run only if the reservation is absent or already due,
// and reports whether THIS caller won. Exactly one instance wins each tick —
// which is the whole point, and the reason it is two native primitives rather
// than a read followed by a write.
//
// The steady state is one request. `set_if` is FacetQL's compare-and-set: it
// rewrites the node only if `next_run` is still at most now, as one all-or-nothing
// step, and the answer is the status — 200 means the condition held and the batch
// committed (this caller won), 412 means it did not and nothing was applied
// (someone else did). `set` is merged into the node's data, so `name` written at
// creation survives every later reservation.
//
// The first-ever tick is the case that needs care, because a set_if whose target
// does not exist is a failed precondition, not a create — the engine deliberately
// gives "the node isn't there" and "someone else took it" the same answer, since
// to a racer they are the same fact. It cannot be closed by inserting the node
// first: insert_node is an upsert, so racers seeding an empty node would each wipe
// the winner's next_run and then each win. It is closed instead by POST /node with
// `if_absent`, which checks for the address and inserts under one write lock —
// exactly one creator, everybody else 409. So: lose the CAS, then try to create.
// Created it => the reservation did not exist and this caller made it, holding the
// tick; 409 => it exists and the CAS already said it is not due, so this caller
// lost. Two requests, only on the losing/first path, and no window in either.
//
// This is pgStore.ReserveCron's `INSERT … ON CONFLICT DO UPDATE … WHERE next_run
// <= now()` with the two halves named separately, and it differs from it in one
// way worth knowing: the deadline is compared against the *caller's* clock, since
// the engine has no now() a condition can name. Exactly one caller still wins, so
// skew can move a tick earlier, never duplicate one.
func (s *fqStore) ReserveCron(name string, next time.Time) (bool, error) {
	ctx := context.Background()
	addr := fqCronAddr(name)
	err := s.c.transaction(ctx, []fqTxOp{{
		Type:    "set_if",
		Address: addr,
		Field:   "next_run",
		Expect:  fqExpectLE(float64(time.Now().Unix())),
		Set:     map[string]any{"next_run": next.Unix()},
	}})
	switch {
	case err == nil:
		return true, nil // the reservation was due and this caller took it
	case !errors.Is(err, errFQPrecondition):
		return false, fmt.Errorf("fqStore.ReserveCron %q: %w", name, err)
	}
	// Not due, or never created. Only one caller can create it.
	data, err := json.Marshal(fqCronData{Name: name, NextRunUnix: next.Unix()})
	if err != nil {
		return false, fmt.Errorf("fqStore.ReserveCron %q: encode __cron node: %w", name, err)
	}
	created, err := s.c.createIfAbsent(ctx, fqNode{
		Address: addr, Kind: "__cron", Data: string(data),
	})
	if err != nil {
		return false, fmt.Errorf("fqStore.ReserveCron %q: %w", name, err)
	}
	return created, nil
}
