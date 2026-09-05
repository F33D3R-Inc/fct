package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// The other three aggregates.
//
// `count`/`exists` have resolved through the store since counts moved off the
// mirror; `sum`, `avg`, `min` and `max` did not, so every one of them read the
// in-memory working set — which is the reason the mirror had to hold every row
// of every entity, and the reason a total was wrong as soon as the rows stopped
// fitting in one page.
//
// What these tests hold down is not that the numbers are right in isolation. It
// is that they are the *same* numbers the interpreter produces, because a page
// can resolve one aggregate through the database and the next on the mirror, and
// a viewer must not be able to tell which.

const ledgerApp = `app Ledger:
    entity Order:
        id: int
        seller: text
        amount: int
    entity Line:
        id: int
        order: Order
        amount: int
    action addOrder(seller: text, amount: int):
        add Order { seller: seller, amount: amount }
    action addLine(order: Order, amount: int):
        add Line { order: order, amount: amount }
    view Home at "/":
        box:
            text "sum {sum(Order.amount)}"
            text "ana {sum(o.amount in Order where o.seller == "ana")}"
            text "avg {avg(Order.amount)}"
            text "min {min(Order.amount)}"
            text "max {max(Order.amount)}"
        box:
            for o in Order by id:
                text "{o.seller}={sum(l.amount in Line where l.order == o.id)}"
`

// aggSpy records the reductions a render asks the store for, alongside the
// counts spyStore already records.
type aggSpy struct {
	Store
	counts []Query
	// reductions and grouped record the entity alongside the spec, because the
	// spec alone cannot tell a page's top-level `sum(Order.amount)` from the
	// per-row `sum(l.amount in Line ...)` under a list — and the whole point of
	// the hoist is that the second one is not a request per row.
	reductions []aggAsk
	grouped    []aggAsk
}

type aggAsk struct {
	entity string
	spec   AggSpec
}

// asked reports how many of these reductions named one entity and function.
func asked(asks []aggAsk, entity, fn string) int {
	n := 0
	for _, a := range asks {
		if a.entity == entity && a.spec.Func == fn {
			n++
		}
	}
	return n
}

func (s *aggSpy) Count(q Query) (int, error) {
	s.counts = append(s.counts, q)
	return s.Store.Count(q)
}

func (s *aggSpy) CountBy(q Query, groupBy string, values []any) (map[string]int, error) {
	s.grouped = append(s.grouped, aggAsk{q.Entity, AggSpec{}})
	return s.Store.CountBy(q, groupBy, values)
}

func (s *aggSpy) Aggregate(q Query, spec AggSpec) (int, error) {
	s.reductions = append(s.reductions, aggAsk{q.Entity, spec})
	return s.Store.Aggregate(q, spec)
}

func (s *aggSpy) AggregateBy(q Query, spec AggSpec, groupBy string, values []any) (map[string]int, error) {
	s.grouped = append(s.grouped, aggAsk{q.Entity, spec})
	return s.Store.AggregateBy(q, spec, groupBy, values)
}

// ledgerServer seeds three orders (ana 10, bo 7, ana 100) and two lines against
// order 1, then hands back the running server and the spy wrapping its store.
func ledgerServer(t *testing.T) (*Server, *aggSpy) {
	t.Helper()
	g, err := compile.String(ledgerApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	for _, row := range [][]any{{"ana", 10}, {"bo", 7}, {"ana", 100}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addOrder"], row); status != 200 {
			t.Fatalf("seeding an order: %d %s", status, msg)
		}
	}
	for _, row := range [][]any{{1, 4}, {1, 6}, {2, 5}} {
		if _, status, msg := srv.runAction(systemSID, srv.byAction["addLine"], row); status != 200 {
			t.Fatalf("seeding a line: %d %s", status, msg)
		}
	}
	spy := &aggSpy{Store: srv.store}
	srv.store = spy
	return srv, spy
}

// renderLedger serves "/" and returns the HTML.
func renderLedger(t *testing.T, srv *Server) string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html, _ := getPage(t, jarClient(t), ts.URL+"/")
	return html
}

func TestReductionsResolveThroughTheStore(t *testing.T) {
	srv, spy := ledgerServer(t)

	// Empty the in-memory working set. Any number the page still renders came
	// from the store; anything that goes to zero was being read out of RAM.
	srv.mu.Lock()
	srv.entities["Order"] = []any{}
	srv.entities["Line"] = []any{}
	srv.mu.Unlock()

	html := renderLedger(t, srv)

	// Each aggregate renders inside its own binding span, so the value is
	// asserted rather than the label next to it.
	for label, want := range map[string]string{
		"sum":          "117", // 10 + 7 + 100
		"sum over ana": "110", // 10 + 100
		"avg":          "39",  // 117 / 3, integer division — the language has no float
		"min":          "7",
		"max":          "100",
	} {
		if !strings.Contains(html, ">"+want+"<") {
			t.Errorf("%s should render %s; the mirror was empty, so this number "+
				"had to come from the store:\n%s", label, want, html)
		}
	}

	if len(spy.reductions) == 0 {
		t.Fatal("no reduction reached the store; the aggregates were answered from the mirror")
	}
}

func TestAverageIsComposedFromSumAndCountRatherThanPushedDown(t *testing.T) {
	srv, spy := ledgerServer(t)

	srv.mu.Lock()
	srv.entities["Order"] = []any{}
	srv.entities["Line"] = []any{}
	srv.mu.Unlock()

	renderLedger(t, srv)

	// The language defines avg as sum ÷ count in integer arithmetic, so the
	// division has exactly one implementation and no backend can round it
	// differently. "avg" must therefore never be asked of a store.
	for _, a := range append(append([]aggAsk{}, spy.reductions...), spy.grouped...) {
		if a.spec.Func == "avg" {
			t.Fatalf("avg reached the store as a reduction; it must be composed "+
				"from sum and count above it, got %+v", a)
		}
	}

	// And it did resolve through the store: a sum over amount, plus a count.
	if asked(spy.reductions, "Order", "sum") == 0 {
		t.Error("avg should have asked the store for sum(amount)")
	}
	if len(spy.counts) == 0 {
		t.Error("avg should have asked the store for the row count it divides by")
	}
}

func TestPerRowReductionsAreHoistedIntoOneGroupedRequest(t *testing.T) {
	srv, spy := ledgerServer(t)
	renderLedger(t, srv)

	// Three rows each carrying `sum(l.amount in Line where l.order == o.id)`.
	// Issuing that per row is an N+1 across the network — the exact shape the
	// hoist exists to prevent — so it must appear as one grouped request.
	if n := asked(spy.grouped, "Line", "sum"); n != 1 {
		t.Fatalf("want exactly one grouped sum over Line for the whole list, got %d "+
			"(grouped=%+v)", n, spy.grouped)
	}
	// The top-level `sum(Order.amount)` bindings are legitimately their own
	// reductions; what must not appear is a per-row one over Line.
	if n := asked(spy.reductions, "Line", "sum"); n != 0 {
		t.Fatalf("%d per-row sums escaped the hoist and became their own requests: %+v",
			n, spy.reductions)
	}
}

// mirrorOnlyStore answers every aggregate with an error, so resolveAgg gives up
// and the interpreter folds the mirror instead. Reads are left alone: the point
// is to move the aggregates onto the other path, not to break the page.
type mirrorOnlyStore struct{ Store }

func (s mirrorOnlyStore) Count(Query) (int, error) {
	return 0, errNoAggregates
}

func (s mirrorOnlyStore) CountBy(Query, string, []any) (map[string]int, error) {
	return nil, errNoAggregates
}

func (s mirrorOnlyStore) Aggregate(Query, AggSpec) (int, error) {
	return 0, errNoAggregates
}

func (s mirrorOnlyStore) AggregateBy(Query, AggSpec, string, []any) (map[string]int, error) {
	return nil, errNoAggregates
}

var errNoAggregates = errors.New("aggregates disabled for this test")

func TestTheStoreAndTheMirrorAnswerIdentically(t *testing.T) {
	// The property the whole seam exists for. A rendered page resolves some
	// aggregates through the database and others on the mirror — an unhoistable
	// one inside a list stays on the mirror by design — so the two paths have to
	// produce the same number for the same rows. If they ever diverge, which one
	// a viewer sees depends on the shape of the page around it.
	//
	// The comparison is the materialized aggregate map rather than the whole
	// document, because that map IS the set of answers: every value the render
	// computed, addressed by where it computed it.
	throughTheStore, _ := ledgerServer(t)
	fromStore := pageState(t, renderLedger(t, throughTheStore))["@aggs"]

	viaMirror, _ := ledgerServer(t)
	viaMirror.store = mirrorOnlyStore{Store: viaMirror.store}
	fromMirror := pageState(t, renderLedger(t, viaMirror))["@aggs"]

	if !reflect.DeepEqual(fromStore, fromMirror) {
		t.Fatalf("the two paths disagree.\nstore:  %#v\nmirror: %#v", fromStore, fromMirror)
	}
	if fromStore == nil {
		t.Fatal("no aggregates were materialized; the test proved nothing")
	}
}

// ── the wire to FacetQL ─────────────────────────────────────────────────────

// fqAggServer stands in for FacetQL: it records the last request body and
// answers with whatever `reply` holds.
func fqAggServer(t *testing.T, reply string, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		*seen = got
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
}

func TestFQAggregateRequestEncoding(t *testing.T) {
	where := &ir.Expr{Kind: "bin", Op: "==",
		L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "o"}, Field: "seller"},
		R: &ir.Expr{Kind: "lit", Val: "ana", VType: "text"}}

	b, err := json.Marshal(fqAggregateRequest{
		Kind: "Order", Where: where, ItemVar: "o", Func: "sum", Field: "amount",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, sub := range []string{
		`"kind":"Order"`, `"item_var":"o"`, `"func":"sum"`, `"field":"amount"`, `"where":`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("aggregate request missing %q in:\n%s", sub, got)
		}
	}

	b, err = json.Marshal(fqAggregateByRequest{
		Kind: "Line", ItemVar: "l", GroupBy: "order", Values: []any{1, 2},
		Func: "sum", Field: "amount",
	})
	if err != nil {
		t.Fatal(err)
	}
	got = string(b)
	for _, sub := range []string{`"group_by":"order"`, `"values":[1,2]`, `"func":"sum"`} {
		if !strings.Contains(got, sub) {
			t.Errorf("grouped aggregate request missing %q in:\n%s", sub, got)
		}
	}
}

func TestAnEmptyExtremeArrivesAsZeroNotNull(t *testing.T) {
	// FacetQL answers `null` for a min/max over no rows, because at that layer
	// there is genuinely no smallest value. This language has no such hole — its
	// reducer returns 0 for every empty reduction — so the seam converts it. Left
	// unconverted, a page would show an empty cell where the mirror shows 0.
	var seen map[string]any
	ts := fqAggServer(t, `{"result":null}`, &seen)
	defer ts.Close()

	c := &fqClient{baseURL: ts.URL, http: ts.Client()}
	got, err := c.aggregate(t.Context(), fqAggregateRequest{
		Kind: "Order", ItemVar: "o", Func: "max", Field: "amount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("a null extreme must become 0, got %d", got)
	}
	if seen["func"] != "max" {
		t.Errorf("the request should carry the function, got %v", seen["func"])
	}
}

func TestEveryGroupedValueComesBackEvenWithNoRows(t *testing.T) {
	// The engine answers every value asked about; the map is pre-filled anyway,
	// so a caller can never read an absent key as "the store forgot" — the same
	// contract countBy holds, and the reason a page's empty rows render 0 rather
	// than nothing.
	var seen map[string]any
	ts := fqAggServer(t, `{"groups":[{"value":1,"result":10}]}`, &seen)
	defer ts.Close()

	c := &fqClient{baseURL: ts.URL, http: ts.Client()}
	got, err := c.aggregateBy(t.Context(), fqAggregateByRequest{
		Kind: "Line", ItemVar: "l", GroupBy: "order", Values: []any{1, 2},
		Func: "sum", Field: "amount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["1"] != 10 {
		t.Errorf("want the answered group, got %v", got)
	}
	if n, ok := got["2"]; !ok || n != 0 {
		t.Errorf("a value with no rows must come back as 0, got %v (present=%v)", n, ok)
	}
}
