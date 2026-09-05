package integration

import (
	"strings"
	"testing"
)

// `sum(<expr over the row> in Coll where …)` — the reduction over a value the
// row does not store.
//
// A cart subtotal is Σ qty × price. Until this form lowered, the only subtotal a
// program could write was one over a `lineTotal` column somebody had to remember
// to keep in step with `qty` and `unitPrice` — the denormalisation this deletes.
// The parser had accepted the syntax for a while; the builder dropped the
// expression on the floor and the check that followed reported that the entity
// had no field "".
//
// Three things have to agree for it to work, which is why this is an integration
// test rather than three unit tests: the compiler has to carry the expression
// into the IR, the server has to reduce it per row, and the shipped client has
// to reduce it *identically* — because the client re-renders the page the moment
// it hydrates, and a subtotal that changes under the cursor is worse than one
// that was never there.
const cartApp = `app Cart:
    entity Product:
        id: int
        name: text
        price: money
    entity Line:
        id: int
        owner: text
        qty: int
        unitPrice: money
        product: Product
    action addProduct(name: text, price: money):
        add Product { name: name, price: price }
    action add(owner: text, qty: int, unitPrice: money, product: Product):
        add Line { owner: owner, qty: qty, unitPrice: unitPrice, product: product }
    view Home at "/":
        box:
            text "subtotal {sum(l.qty * l.unitPrice in Line where l.owner == "ana")}"
            text "everything {sum(l.qty * l.unitPrice in Line)}"
            text "largest {max(l.qty * l.unitPrice in Line)}"
            text "units {sum(l.qty in Line where l.owner == "ana")}"
            text "catalogue {sum(l.qty * Product(l.product).price in Line where l.owner == "ana")}"
`

// seedCart fills the fixture: two products, and three lines whose unit prices
// deliberately differ from the catalogue price, so a reduction that read the
// wrong one would show it.
func seedCart(t *testing.T, a *app) {
	t.Helper()
	for _, p := range [][]any{{"widget", 1000}, {"gizmo", 7}} {
		if code, body := a.action("addProduct", p...); code != 200 {
			t.Fatalf("seeding a product: %d %s", code, body)
		}
	}
	// ana: 2x300 + 3x150 = 1050, largest line 600.
	// bo:  10x99 = 990, which is the largest line overall.
	for _, row := range [][]any{{"ana", 2, 300, 1}, {"ana", 3, 150, 2}, {"bo", 10, 99, 1}} {
		if code, body := a.action("add", row...); code != 200 {
			t.Fatalf("seeding a line: %d %s", code, body)
		}
	}
}

func TestAComputedSumIsReducedPerRow(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, cartApp)

	seedCart(t, a)

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	for label, want := range map[string]string{
		"subtotal":   "1050", // 2*300 + 3*150 — a number no column holds
		"everything": "2040", // 1050 + 990
		"largest":    "990",  // bo's single line, not ana's 600
		"units":      "5",    // the bare-column form still works beside it
		// 2 x widget(1000) + 3 x gizmo(7) = 2021 — a lookup nested inside the
		// reduced expression, resolved per row. It is also what numbers an
		// aggregate address inside a `sel`, so the two page walks have to agree
		// about it.
		"catalogue": "2021",
	} {
		if !strings.Contains(html, ">"+want+"<") {
			t.Errorf("%s should reduce to %s; got:\n%s", label, want, serverText(html))
		}
	}
}

func TestTheClientReducesAComputedSumTheSameWay(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, cartApp)

	seedCart(t, a)

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	nodes, clientText := renderClientText(t, html)
	if nodes <= 1 {
		t.Fatal("the page renders blank on the client")
	}

	// The real assertion. Two evaluators now reduce this expression — Go's
	// reduceAgg and the shipped facet.js — and they have to produce the same
	// characters. A disagreement here is not a crash: it is a subtotal that
	// changes when the page hydrates.
	want := serverText(html)
	if clientText != want {
		serverWindow, clientWindow := firstDifference(want, clientText)
		t.Errorf("the client reduced the computed sum differently than the server.\n"+
			"server: %s\nclient: %s", serverWindow, clientWindow)
	}
}

// A computed reduction reads columns the page never prints. clientColls has to
// ship those columns anyway, or the client re-renders the subtotal from rows
// with the multiplicands projected away — and totals zeros.
func TestTheColumnsAComputedSumReadsAreShippedToTheClient(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, cartApp)

	seedCart(t, a)

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	// Neither `qty` nor `unitPrice` appears anywhere in the rendered text; both
	// exist only inside the reduced expression.
	if strings.Contains(serverText(html), "300") {
		t.Fatal("this test assumes unitPrice is never printed; the fixture changed")
	}

	nodes, clientText := renderClientText(t, html)
	if nodes <= 1 {
		t.Fatal("the page renders blank on the client")
	}
	if !strings.Contains(clientText, "1050") {
		t.Errorf("the client lost the columns the reduction reads and totalled "+
			"zeros; got: %s", clientText)
	}
}

// The client's own reducer, reached.
//
// A computed reduction the server materialized is *read* by the client, not
// recomputed — which is why the fixture above cannot tell a correct client
// reducer from a missing one. The exception is a per-row context: an aggregate
// inside a `for`'s filter has no address of its own (the render path stands
// still while the row changes), so the server records nothing for it and the
// client has to reduce it itself, once per row.
//
// That is also the only place the two page walks have to agree about a `sel`:
// an aggregate nested inside a reduced expression is numbered by both sides, and
// a walk that skipped `sel` on one side would shift every address after it.
const perRowCartApp = `app PerRowCart:
    entity Product:
        id: int
        name: text
        price: money
    entity Line:
        id: int
        owner: text
        qty: int
        product: Product
    state floor: int = 1500 @client
    action addProduct(name: text, price: money):
        add Product { name: name, price: price }
    action add(owner: text, qty: int, product: Product):
        add Line { owner: owner, qty: qty, product: product }
    view Home at "/":
        box:
            for l in Line where sum(x.qty * Product(x.product).price in Line where x.owner == l.owner) > floor:
                text "big:{l.owner}"
`

func TestTheClientReducesAPerRowComputedSumItself(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, perRowCartApp)

	for _, p := range [][]any{{"widget", 1000}, {"gizmo", 7}} {
		if code, body := a.action("addProduct", p...); code != 200 {
			t.Fatalf("seeding a product: %d %s", code, body)
		}
	}
	// ana: 2 widgets = 2000, over the 1500 floor — two rows, both kept.
	// bo:  3 gizmos  =   21, under it — dropped.
	for _, row := range [][]any{{"ana", 1, 1}, {"ana", 1, 1}, {"bo", 3, 2}} {
		if code, body := a.action("add", row...); code != 200 {
			t.Fatalf("seeding a line: %d %s", code, body)
		}
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if strings.Contains(html, "big:bo") {
		t.Fatal("bo is under the floor; the fixture no longer discriminates")
	}
	if !strings.Contains(html, "big:ana") {
		t.Fatalf("the server should keep ana's rows:\n%s", serverText(html))
	}

	nodes, clientText := renderClientText(t, html)
	if nodes <= 1 {
		t.Fatal("the page renders blank on the client")
	}

	// The client reduced `qty * Product(product).price` per row, per owner, and
	// filtered on the result. If it read a bare column instead, or numbered the
	// nested lookup differently than the server, the row set changes.
	want := serverText(html)
	if clientText != want {
		serverWindow, clientWindow := firstDifference(want, clientText)
		t.Errorf("the client filtered on a different reduction than the server.\n"+
			"server: %s\nclient: %s", serverWindow, clientWindow)
	}
}

// The client's own reducer, reached.
//
// Everything above proves the server half. The client reads the value the render
// materialized rather than recomputing it, so those tests would pass with no
// client reducer at all — which is exactly what a red run showed.
//
// A repeat over a `[T]` state cell is the case the design routes to the client:
// the rows are filtered in the browser, once per row, so the predicate has no
// render address and no materialized answer to fall back on — and clientColls
// ships the collection an aggregate in that predicate reads (walkNodeExprs, the
// `n.Coll != "" && !isEnt[n.Coll]` branch) precisely so it can be evaluated
// there. So the client reduces `qty * unitPrice` itself, and the subtotal it
// computes decides which rows survive.
//
// The `Product(...)` lookup inside the reduced expression and the `count(Audit)`
// after it are there for the second half of the agreement: both page walks number
// every aggregate and lookup, `sel` included, and the count is over a collection
// nothing ships — so the client can only get it by reading the slot the server
// wrote. A walk that skipped `sel` would number the count one lower and read a
// slot that is not its own.
const clientReduceApp = `app ClientReduce:
    entity Product:
        id: int
        price: money
    entity Line:
        id: int
        qty: int
        product: Product
    entity Audit:
        id: int
        note: text
    state names: [text] = ["ana", "bo"] @client
    state floor: int = 1000 @client
    action addProduct(price: money):
        add Product { price: price }
    action add(qty: int, product: Product):
        add Line { qty: qty, product: product }
    action note(note: text):
        add Audit { note: note }
    view Home at "/":
        box:
            for n in names where sum(l.qty * Product(l.product).price in Line) > floor:
                text "kept:{n}"
            text "audits {count(Audit)}"
`

func TestTheClientReducesTheExpressionItselfInAPerRowFilter(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, clientReduceApp)

	// 2*300 + 3*150 = 1050, just over the 1000 floor. A client that reduced a
	// bare column instead would get 5 (qty) — under it, so every row would
	// vanish. A client that lost the rows entirely would get 0. The margin is
	// deliberately narrow so a wrong reduction cannot pass.
	for _, price := range []any{300, 150} {
		if code, body := a.action("addProduct", price); code != 200 {
			t.Fatalf("seeding a product: %d %s", code, body)
		}
	}
	for _, row := range [][]any{{2, 1}, {3, 2}} {
		if code, body := a.action("add", row...); code != 200 {
			t.Fatalf("seeding a line: %d %s", code, body)
		}
	}
	for _, note := range []any{"one", "two", "three"} {
		if code, body := a.action("note", note); code != 200 {
			t.Fatalf("seeding an audit: %d %s", code, body)
		}
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if !strings.Contains(html, "kept:ana") || !strings.Contains(html, "kept:bo") {
		t.Fatalf("the server should keep both rows (1050 > 1000):\n%s", serverText(html))
	}
	if !strings.Contains(html, ">3<") {
		t.Fatalf("the server should have rendered 3 audits:\n%s", serverText(html))
	}

	nodes, clientText := renderClientText(t, html)
	if nodes <= 1 {
		t.Fatal("the page renders blank on the client")
	}

	want := serverText(html)
	if clientText != want {
		serverWindow, clientWindow := firstDifference(want, clientText)
		t.Errorf("the client reduced the expression differently than the server, "+
			"so a different set of rows survived the filter.\nserver: %s\nclient: %s",
			serverWindow, clientWindow)
	}
}
