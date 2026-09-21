package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// An action body has control flow: `for` over an entity's rows, `if`/`else`,
// `let` locals bound from any expression, and `let id = add …`. These pin the
// lowering — the ops, their nesting, what a `for` carries — and the scope rules
// the builder enforces around the names those statements bind.
const checkoutApp = `app Shop:
    entity Product:
        id: int
        stock: int
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
    action checkout():
        for l in CartLine where l.owner == actor by id limit 50:
            check Product(l.product).stock >= l.qty "not enough stock"
            let total = l.qty * l.unitPrice
            set Product(l.product).stock = Product(l.product).stock - l.qty
            let oid = add Order { buyer: actor, product: l.product, total: total }
            add OrderLine { order: oid, product: l.product }
            if total > 10000:
                lastOrder = oid
            else:
                lastOrder = 0
        remove l in CartLine where l.owner == actor
    view Home at "/":
        button "check out" -> checkout()
`

func actionNamed(t *testing.T, g *ir.IR, name string) ir.Action {
	t.Helper()
	for _, a := range g.Actions {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no action %q", name)
	return ir.Action{}
}

func TestActionForLowersToNestedBlock(t *testing.T) {
	g, err := String(checkoutApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	act := actionNamed(t, g, "checkout")
	if act.Placement != "server" {
		t.Fatalf("an action writing entities inside a loop is server-placed, got %q", act.Placement)
	}
	if len(act.Body) != 2 || act.Body[0].Op != "for" || act.Body[1].Op != "remove" {
		t.Fatalf("top-level ops = %v, want [for remove]", opsOf(act.Body))
	}
	loop := act.Body[0]
	if loop.Entity != "CartLine" || loop.Var != "l" || loop.Where == nil || loop.Order != "id" || loop.Limit == nil {
		t.Fatalf("for stmt = entity:%q var:%q where:%v order:%q limit:%v", loop.Entity, loop.Var, loop.Where != nil, loop.Order, loop.Limit != nil)
	}
	want := "check,let,set,add,add,if"
	if got := strings.Join(opsOf(loop.Body), ","); got != want {
		t.Fatalf("for body ops = %s, want %s", got, want)
	}
	if loop.Body[3].Bind != "oid" {
		t.Fatalf("bound add should carry its bind, got %q", loop.Body[3].Bind)
	}
	branch := loop.Body[5]
	if len(branch.Body) != 1 || branch.Body[0].Op != "assign" || len(branch.Else) != 1 || branch.Else[0].Op != "assign" {
		t.Fatalf("if should carry a then and an else, got then=%v else=%v", opsOf(branch.Body), opsOf(branch.Else))
	}
	if len(act.Writes) != 1 || act.Writes[0] != "lastOrder" {
		t.Fatalf("a write nested three blocks deep still counts: writes = %v", act.Writes)
	}
}

func opsOf(body []ir.Stmt) []string {
	var out []string
	for _, st := range body {
		out = append(out, st.Op)
	}
	return out
}

func TestActionBlockScopeRules(t *testing.T) {
	base := `app Scope:
    entity Item:
        id: int
        n: int
    state total: int = 0
    action go():
        BODY
    view Home at "/":
        button "go" -> go()
`
	cases := []struct{ name, body, want string }{
		{"for over a non-entity", "for x in total:\n            total = x", "is not an entity"},
		{"loop var shadows a state cell", "for total in Item:\n            total = 1", "would shadow the state cell"},
		{"let shadows an entity", "let Item = 1\n        total = Item", "would shadow the entity"},
		{"let redeclared", "let a = 1\n        let a = 2\n        total = a", "already in scope"},
		{"let mut is proc-only", "let mut a = 1\n        total = a", "proc-only"},
		{"more is a view clause", "for i in Item limit 3 more go:\n            total = i.n", "has no meaning in an action"},
		{"for body must be a block", "for i in Item\n        total = 1", "end the line with `:`"},
		{"else without if", "total = 1\n        else:\n            total = 2", "no matching `if`"},
		{"impure for filter", "for i in Item where i.n > rand(3):\n            total = i.n", "cannot use an effectful builtin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(strings.Replace(base, "BODY", c.body, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
	// A local declared inside a block does not leak out of it.
	_, err := String(strings.Replace(base, "BODY", "if total > 0:\n            let a = 1\n            total = a\n        total = a", 1))
	if err == nil || !strings.Contains(err.Error(), `unknown reference "a"`) {
		t.Fatalf("a block-local should be out of scope after its block, got %v", err)
	}
}

func TestClientActionMayBranchAndLoop(t *testing.T) {
	// Control flow alone does not force the server: an action that only writes
	// @client state stays in the browser, whatever shape its body has.
	src := `app UI:
    entity Item:
        id: int
        n: int
    state picked: int = 0 @client
    action pick(min: int):
        for i in Item where i.n >= min by n limit 1:
            if i.n > 10:
                picked = i.id
            else:
                picked = 0
    view Home at "/":
        button "pick" -> pick(1)
`
	g, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if act := actionNamed(t, g, "pick"); act.Placement != "client" {
		t.Fatalf("placement = %q (%s), want client", act.Placement, act.Reason)
	}
}
