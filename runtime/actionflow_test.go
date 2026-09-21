package runtime

import (
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// Control flow in an action body, run for real: a `for` that writes per row and
// binds the id of what it adds, a `check` inside the loop that rolls the whole
// action back, `by`/`limit` picking one row, and nested `if`/`else`. Every
// assertion reads the working set back through the same evaluator a view uses.
const flowApp = `app Flow:
    entity Product:
        id: int
        stock: int @min(0)
    entity CartLine:
        id: int
        owner: text
        product: int
        qty: int
        unitPrice: money
    entity Order:
        id: int
        buyer: text
        product: int
        total: money
    entity OrderLine:
        id: int
        order: int
        product: int
    state lastOrder: int = 0
    state picked: int = 0
    action seed():
        add Product { stock: 5 }
        add Product { stock: 1 }
    action addLine(pid: int, n: int, price: money):
        add CartLine { owner: actor, product: pid, qty: n, unitPrice: price }
    action checkout():
        for l in CartLine where l.owner == actor by id:
            check Product(l.product).stock >= l.qty "not enough stock"
            let total = l.qty * l.unitPrice
            set Product(l.product).stock = Product(l.product).stock - l.qty
            let oid = add Order { buyer: actor, product: l.product, total: total }
            add OrderLine { order: oid, product: l.product }
            lastOrder = oid
        remove l in CartLine where l.owner == actor
    action pickBiggest():
        for l in CartLine where l.owner == actor by qty desc limit 1:
            picked = l.qty
    action grade(n: int):
        if n > 5:
            picked = 100
        else:
            if n > 2:
                picked = 50
            else:
                picked = 1
    view Home at "/":
        button "x" -> checkout()
`

type flowT struct {
	t   *testing.T
	srv *Server
	g   *ir.IR
}

func newFlow(t *testing.T) *flowT {
	t.Helper()
	g, err := compile.String(flowApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return &flowT{t, srv, g}
}

func (f *flowT) run(action string, args ...any) error {
	_, err := f.srv.Run("ada", "member", true, action, args)
	return err
}

func (f *flowT) eval(src string) any {
	f.t.Helper()
	e, err := ir.CompileExpr(f.g, src)
	if err != nil {
		f.t.Fatalf("expr %q: %v", src, err)
	}
	return f.srv.EvalExpr(e, "ada", "member", true)
}

func (f *flowT) want(src string, want any) {
	f.t.Helper()
	if got := f.eval(src); !equal(got, want) {
		f.t.Fatalf("%s = %v, want %v", src, got, want)
	}
}

func TestActionForWritesPerRowAndBindsIds(t *testing.T) {
	f := newFlow(t)
	for _, step := range []struct {
		action string
		args   []any
	}{{"seed", nil}, {"addLine", []any{1, 2, 300}}, {"addLine", []any{2, 1, 700}}, {"checkout", nil}} {
		if err := f.run(step.action, step.args...); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
	}
	f.want("count(Order)", 2)
	f.want("count(OrderLine)", 2)
	f.want("Order(1).total", 600)
	f.want("Order(2).total", 700)
	f.want("OrderLine(2).order", 2) // the bound id of the row the loop just added
	f.want("Product(1).stock", 3)
	f.want("Product(2).stock", 0)
	f.want("count(CartLine)", 0)
	f.want("lastOrder", 2)
}

func TestCheckInsideLoopRollsBackTheWholeAction(t *testing.T) {
	f := newFlow(t)
	f.run("seed")
	f.run("addLine", 1, 2, 300)
	f.run("addLine", 2, 5, 700) // more than product 2 has
	err := f.run("checkout")
	if err == nil || !strings.Contains(err.Error(), "not enough stock") {
		t.Fatalf("want the loop's check to fail the action, got %v", err)
	}
	// Line one's writes (a stock decrement, an Order, an OrderLine, a state
	// assign) all happened before line two's check failed — and are all gone.
	f.want("count(Order)", 0)
	f.want("count(OrderLine)", 0)
	f.want("Product(1).stock", 5)
	f.want("count(CartLine)", 2)
	f.want("lastOrder", 0)
}

func TestActionForOrderAndLimitPickOneRow(t *testing.T) {
	f := newFlow(t)
	f.run("seed")
	f.run("addLine", 1, 2, 300)
	f.run("addLine", 2, 9, 700)
	f.run("addLine", 1, 4, 300)
	if err := f.run("pickBiggest"); err != nil {
		t.Fatal(err)
	}
	f.want("picked", 9)
}

func TestActionIfElseNests(t *testing.T) {
	f := newFlow(t)
	for n, want := range map[int]int{9: 100, 3: 50, 0: 1} {
		if err := f.run("grade", n); err != nil {
			t.Fatal(err)
		}
		f.want("picked", want)
	}
}

func TestReplaceAndSlugBuiltins(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Hello, World!", "hello-world"},
		{"  Hello, World! -- Ünïcode 42 ", "hello-world-n-code-42"},
		{"---", ""},
		{"already-a-slug", "already-a-slug"},
	} {
		if got := slug(c.in); got != c.want {
			t.Errorf("slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := callBuiltin("replace", []any{"a-b-c", "-", "+"}); got != "a+b+c" {
		t.Errorf("replace = %v", got)
	}
	if got := callBuiltin("replace", []any{"abc", "", "-"}); got != "-a-b-c-" {
		t.Errorf("replace with an empty pattern must match Go and the client's replaceAll: %v", got)
	}
}
