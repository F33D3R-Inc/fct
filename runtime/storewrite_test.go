package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// ── a write that failed must not answer "ok" ────────────────────────────────

// failingTx is a transaction that buffers like any other and refuses at commit —
// the shape of every real rejection: a constraint the engine only evaluates when
// the batch lands.
type failingTx struct {
	Tx
	fail error
}

func (t *failingTx) Commit() error {
	if t.fail != nil {
		t.Tx.Rollback()
		return t.fail
	}
	return t.Tx.Commit()
}

// failingStore refuses every transaction commit once `fail` is set.
type failingStore struct {
	Store
	fail error
}

func (s *failingStore) Begin() (Tx, error) {
	tx, err := s.Store.Begin()
	if err != nil {
		return nil, err
	}
	return &failingTx{Tx: tx, fail: s.fail}, nil
}

const writeApp = `app Ledger:
    entity Entry:
        id: int
        memo: text
    action add(memo: text):
        add Entry { memo: memo }
    action retitle(id: int, memo: text):
        set Entry(id).memo = memo
    view Home at "/":
        box:
            for e in Entry by id:
                text "{e.memo}"
`

// A store write that fails must fail the request, and must leave nothing behind.
//
// This is the bug the journal app hit from the other side: `commit` logged the
// rejection and returned nothing, so `runActionLocked` went on to broadcast, to
// audit a success, and to answer 200 {"ok":true} for a row the database had
// refused. The in-memory working set kept serving the value until a restart made
// it vanish — so "the write failed" was information nobody had, on either side
// of the wire.
//
// Both halves are asserted, because either one alone is still a lie: the caller
// must be told, AND the working set must be back where it started.
func TestAFailedStoreWriteIsAFailedRequest(t *testing.T) {
	g, err := compile.String(writeApp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(apiReadEnv, "Entry")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) int {
		t.Helper()
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	rows := func() []any {
		t.Helper()
		out, _ := getJSON(t, ts.URL+"/api/Entry")["rows"].([]any)
		return out
	}
	// The working set is a second copy of the same data — the one the page, the
	// SSE deltas and every @unique check read — so it is asserted separately.
	// Surfacing the failure without undoing this copy would only make the lost
	// write loud: the request says 500 and the app goes on showing the edit.
	working := func() []record {
		t.Helper()
		srv.mu.Lock()
		defer srv.mu.Unlock()
		out := make([]record, 0, len(srv.entities["Entry"]))
		for _, r := range srv.entities["Entry"] {
			out = append(out, r.(record))
		}
		return out
	}

	// One row lands while the store is healthy.
	if code := post("/api/add", `{"args":["kept"]}`); code != 200 {
		t.Fatalf("a healthy write should succeed, got %d", code)
	}
	if got := len(rows()); got != 1 {
		t.Fatalf("want 1 row after the healthy write, got %d", got)
	}

	// Now the store refuses everything.
	boom := errors.New("the engine refused this batch")
	srv.store = &failingStore{Store: srv.store, fail: boom}

	t.Run("an add", func(t *testing.T) {
		if code := post("/api/add", `{"args":["lost"]}`); code == 200 {
			t.Error("a refused write answered 200; the caller was told a lie")
		}
		if got := len(rows()); got != 1 {
			t.Errorf("the refused row reached the store: %d rows, want 1", got)
		}
		if got := len(working()); got != 1 {
			t.Errorf("the refused row is still in the working set: %d rows, want 1", got)
		}
	})

	t.Run("a set", func(t *testing.T) {
		if code := post("/api/retitle", `{"args":[1,"overwritten"]}`); code == 200 {
			t.Error("a refused update answered 200; the caller was told a lie")
		}
		r := rows()
		if len(r) != 1 {
			t.Fatalf("want 1 row, got %d", len(r))
		}
		if memo := r[0].(map[string]any)["memo"]; memo != "kept" {
			t.Errorf("the refused edit reached the store: memo = %v, want \"kept\"", memo)
		}
		if w := working(); len(w) != 1 || w[0]["memo"] != "kept" {
			t.Errorf("the refused edit survived in the working set: %v", w)
		}
	})

	// With the store healthy again the id counter has not drifted: the refused add
	// gave its id back, so the next row is the second one and not the third.
	srv.store = srv.store.(*failingStore).Store
	if code := post("/api/add", `{"args":["next"]}`); code != 200 {
		t.Fatalf("a healthy write after a refused one should succeed, got %d", code)
	}
	r := rows()
	if len(r) != 2 {
		t.Fatalf("want 2 rows, got %d", len(r))
	}
	if id := toInt(r[1].(map[string]any)["id"]); id != 2 {
		t.Errorf("the refused add consumed an id: next row is %d, want 2", id)
	}
}

// ── two writes to one row are one write, in every Store ─────────────────────

const twoSetIR = `app Blog:
    entity Post:
        id: int
        title: text
        body: text
    entity Comment:
        id: int
        post: Post
        body: text
    action seed(title: text, body: text):
        add Post { title: title, body: body }
    action update(id: int, title: text, body: text):
        set Post(id).title = title
        set Post(id).body = body
    view Home at "/":
        box:
            for p in Post by id:
                text "{p.title}"
`

// memStore is the reference: two Saves of one row in one transaction leave one
// row carrying the last value written to each field. fqStore has to agree, and
// did not — it sent two `insert_node` ops at the same address and FacetQL threw
// the batch out on the unique id index that a referenced entity necessarily has.
func TestATransactionSavingOneRowTwiceLeavesOneRow(t *testing.T) {
	g, err := compile.String(twoSetIR)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	if _, err := st.Migrate(g.Entities, true); err != nil {
		t.Fatal(err)
	}

	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Save("Post", map[string]any{"id": 1, "title": "first", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save("Post", map[string]any{"id": 1, "title": "second", "body": "b2"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("memStore refused two saves of one row: %v", err)
	}

	rows, _, err := st.Query(Query{Entity: "Post", ItemVar: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %v", len(rows), rows)
	}
	got := rows[0].(record)
	if got["title"] != "second" || got["body"] != "b2" {
		t.Errorf("last write did not win: %v", got)
	}
}

// The same sequence against fqStore, read at the wire: the batch it would POST
// must carry ONE insert_node for the address, holding both fields.
func TestFQTransactionCollapsesRepeatedSavesOfOneRow(t *testing.T) {
	g, err := compile.String(twoSetIR)
	if err != nil {
		t.Fatal(err)
	}
	st := newTestFQStore(g.Entities)

	tx, _ := st.Begin()
	if err := tx.Save("Post", map[string]any{"id": 1, "title": "first", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Save("Post", map[string]any{"id": 1, "title": "second", "body": "b2"}); err != nil {
		t.Fatal(err)
	}
	// A different row is a different address and keeps its own op.
	if err := tx.Save("Post", map[string]any{"id": 2, "title": "other", "body": "x"}); err != nil {
		t.Fatal(err)
	}

	ops := tx.(*fqTx).ops
	if len(ops) != 2 {
		t.Fatalf("want 2 ops (one per row), got %d: %v", len(ops), ops)
	}
	if a := fqAddress("Post", 1); ops[0].Address != a {
		t.Fatalf("first op should still speak for %s, got %s", a, ops[0].Address)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(ops[0].Data), &data); err != nil {
		t.Fatal(err)
	}
	if data["title"] != "second" || data["body"] != "b2" {
		t.Errorf("the collapsed op does not carry the last write to each field: %v", data)
	}
}

// A delete or a clear stands between two saves of a row: the later save is a new
// fact and must land after them, not be folded into the op that precedes them.
func TestFQTransactionDoesNotCollapseAcrossADeleteOrClear(t *testing.T) {
	g, err := compile.String(twoSetIR)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": 1, "title": "t", "body": "b"}

	t.Run("delete", func(t *testing.T) {
		tx, _ := newTestFQStore(g.Entities).Begin()
		tx.Save("Post", row)
		tx.Delete("Post", 1)
		tx.Save("Post", row)
		if ops := tx.(*fqTx).ops; len(ops) != 3 || ops[2].Type != "insert_node" {
			t.Errorf("want insert/delete/insert, got %d ops ending in %v", len(ops), ops[len(ops)-1].Type)
		}
	})

	t.Run("clear", func(t *testing.T) {
		tx, _ := newTestFQStore(g.Entities).Begin()
		tx.Save("Post", row)
		tx.Clear("Post")
		tx.Save("Post", row)
		if ops := tx.(*fqTx).ops; len(ops) != 3 || ops[2].Type != "insert_node" {
			t.Errorf("want insert/clear/insert, got %d ops ending in %v", len(ops), ops[len(ops)-1].Type)
		}
	})

	t.Run("a clear of another kind does not break the collapse", func(t *testing.T) {
		tx, _ := newTestFQStore(g.Entities).Begin()
		tx.Save("Post", row)
		tx.Clear("Comment")
		tx.Save("Post", row)
		if ops := tx.(*fqTx).ops; len(ops) != 2 {
			t.Errorf("want 2 ops (the two Posts collapse), got %d", len(ops))
		}
	})
}

// newTestFQStore builds an fqStore with no client behind it: these tests read the
// buffered batch rather than sending it.
func newTestFQStore(ents []ir.Entity) *fqStore {
	byName := map[string]ir.Entity{}
	for _, e := range ents {
		byName[e.Name] = e
	}
	return &fqStore{ents: byName}
}
