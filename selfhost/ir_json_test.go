package selfhost

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/runtime"
)

// ir_json.fct reads the IR the real compiler emits. For every expression and
// statement shape the compiler can produce, the JSON encoding/json writes
// must survive parse → IRExpr/IRStmt → irExprJSON/irStmtJSON unchanged.
const irJSONApp = `app Shapes:
    enum Status:
        active
        closed
    entity Product:
        id: int
        name: text
        stock: int
        price: money
    entity CartLine:
        id: int
        owner: text
        product: Product
        qty: int
        unitPrice: money
    entity Order:
        id: int
        buyer: text
        total: money
    service Brain at "http://brain:1":
        answer(q: text) -> int
    proc twice(n: int) -> int:
        return n * 2
    state lastOrder: int = 0
    state status: text = "active"
    state x: int = 0
    state a: int = 0
    state b: int = 0
    state done: bool = false
    state y: text = ""
    state name: text = ""
    derive cartTotal: money = sum(l.qty * l.unitPrice in CartLine where l.owner == actor)
    policy member:
        actor != "guest"
    action checkout(q: text):
        requires member
        check cartTotal > 0 "empty"
        let ans = call Brain.answer(q)
        let d = do twice(ans)
        for l in CartLine where l.owner == actor by qty desc limit 5:
            check Product(l.product).stock >= l.qty "sold out"
            set Product(l.product).stock = Product(l.product).stock - l.qty
            let oid = add Order { buyer: actor, total: l.qty * l.unitPrice }
            if l.qty > d:
                lastOrder = oid
            else:
                lastOrder = 0
        set p in Product where p.stock < 0:
            stock = 0
        remove l in CartLine where l.owner == actor
        clear Order
        establish actor "x" role "member"
        print("done")
    view Home at "/":
        text "{lastOrder}"
`

func loadIRJSONApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("ir_json.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/ir_json.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestIRJSONRoundTripsCompilerExpressions(t *testing.T) {
	g, err := compile.String(irJSONApp)
	if err != nil {
		t.Fatalf("compile the shapes app: %v", err)
	}
	ts := loadIRJSONApp(t)
	for _, src := range []string{
		`1 + 2 * 3`, `-(x - 1) >= 0 && !done || y != "a\"b"`, `"tab\there" + 3`,
		`Product(x).stock - x`, `count(Product)`, `sum(Product.price)`,
		`exists(l in CartLine where l.owner == actor && l.qty > 0)`,
		`sum(l.qty * l.unitPrice in CartLine where l.owner == actor)`,
		`Status.active`, `cartTotal / 100`, `dirty(status)`, `touched(name)`,
		`money(abs(-5)) + upper(trim(name))`, `min(a, b) < max(1, 2)`, `true`,
	} {
		e, err := ir.CompileExpr(g, src)
		if err != nil {
			t.Fatalf("real compiler refused %q: %v", src, err)
		}
		want, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRExprRoundTrip", string(want))
		if got, _ := d["irExprBack"].(string); got != string(want) {
			t.Errorf("%s:\n  got  %s\n  want %s", src, got, want)
		}
	}
}

func TestIRJSONRoundTripsCompilerStatements(t *testing.T) {
	g, err := compile.String(irJSONApp)
	if err != nil {
		t.Fatalf("compile the shapes app: %v", err)
	}
	ts := loadIRJSONApp(t)
	var body []ir.Stmt
	for _, a := range g.Actions {
		if a.Name == "checkout" {
			body = a.Body
		}
	}
	if len(body) < 9 {
		t.Fatalf("expected the checkout body's nine statements, got %d", len(body))
	}
	for i, st := range body {
		want, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRStmtRoundTrip", string(want))
		if got, _ := d["irStmtBack"].(string); got != string(want) {
			t.Errorf("statement %d (%s):\n  got  %s\n  want %s", i, st.Op, got, want)
		}
	}
}
