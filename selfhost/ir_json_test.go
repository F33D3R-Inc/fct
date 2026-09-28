package selfhost

import (
	"encoding/json"
	"fmt"
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

// The reader decodes every JSON string escape as encoding/json does — a
// \uXXXX of any code point to its UTF-8 (one to three bytes), a surrogate
// pair to the one character it spells (four bytes), a lone surrogate to
// U+FFFD, and \b \f \/ by name — so a document another encoder escaped
// differently still reads back to the same values.
func TestIRJSONReaderDecodesEveryEscape(t *testing.T) {
	ts := loadIRJSONApp(t)
	for _, lit := range []string{
		`"caf\u00e9"`, `"\u65e5\u672c"`, `"\ud83d\ude00 grin"`, `"lone \ud800 high"`,
		`"lone \udc00 low"`, `"bs\b ff\f sl\/"`, `"\u0041\u00df\u0800\uffff"`, `"sep\u2028\u2029"`,
	} {
		src := `{"kind":"lit","val":` + lit + `,"vtype":"text"}`
		var e ir.Expr
		if err := json.Unmarshal([]byte(src), &e); err != nil {
			t.Fatalf("fixture %s: %v", lit, err)
		}
		want, err := json.Marshal(&e)
		if err != nil {
			t.Fatal(err)
		}
		d := postExprJSON(t, ts, "runIRExprRoundTrip", src)
		if got, _ := d["irExprBack"].(string); got != string(want) {
			t.Errorf("%s:\n  got  %s\n  want %s", lit, got, want)
		}
	}
}

// The span API (jObjectSpans / jArraySpans / jSpanText) slices each value
// out of the document exactly as written: every top-level member, every
// page and every component of a real IR, compact and indented, is the same
// bytes encoding/json's RawMessage holds for it.
func TestIRJSONSpansSliceTheDocument(t *testing.T) {
	ts := loadIRJSONApp(t)
	for _, path := range []string{"../examples/chirp.fct", "../examples/layered/playground.fct", "../../facets/layered_demo.fct", "testdata/jsonescapes/app.fct"} {
		g, err := compile.File(path)
		if err != nil {
			t.Fatalf("compile %s: %v", path, err)
		}
		compact, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		indented, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range [][]byte{compact, indented} {
			var top map[string]json.RawMessage
			if err := json.Unmarshal(doc, &top); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			for k, v := range top {
				want[k] = string(v)
			}
			for _, arr := range []string{"pages", "components"} {
				var elems []json.RawMessage
				if raw, ok := top[arr]; ok && string(raw) != "null" {
					if err := json.Unmarshal(raw, &elems); err != nil {
						t.Fatal(err)
					}
				}
				for i, e := range elems {
					want[fmt.Sprintf("%s[%d]", arr, i)] = string(e)
				}
			}
			d := postExprJSON(t, ts, "runIRSpans", string(doc))
			report, _ := d["irSpans"].(string)
			var pairs [][2]string
			if err := json.Unmarshal([]byte(report), &pairs); err != nil {
				t.Fatalf("%s: report is not JSON: %v\n%.300s", path, err, report)
			}
			got := map[string]string{}
			for _, p := range pairs {
				got[p[0]] = p[1]
			}
			if len(got) != len(want) {
				t.Fatalf("%s: %d spans, want %d", path, len(got), len(want))
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s %s:\n  got  %.200s\n  want %.200s", path, k, got[k], w)
				}
			}
		}
	}
}
