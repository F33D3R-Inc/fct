package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/ast"
	"facet/internal/compile"
	"facet/internal/parser"
	"facet/runtime"
)

// action_stmt.fct is the self-hosted port of parser.go's action-body
// grammar. Since the real grammar grew control flow (`for <range>:`,
// `if`/`else`, `let name = expr`, `let id = add …`), the port carries the
// same, and this pins the two to each other: every body below is parsed by
// the REAL parser.Parse and by action_stmt.fct's `parseActionBodySrc`, and
// the two statement trees are spelled the same way (describeActionStmt's
// own format, restated in Go over ast.Stmt) and compared.

func loadActionStmtApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("action_stmt.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/action_stmt.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// describeGoStmts spells an ast.Stmt list exactly as action_stmt.fct's
// describeActionStmts spells its own [ActionStmt] — the comparison target.
func describeGoStmts(body []ast.Stmt) string {
	var parts []string
	for _, s := range body {
		parts = append(parts, describeGoStmt(s))
	}
	return strings.Join(parts, ",")
}

func describeGoStmt(s ast.Stmt) string {
	switch st := s.(type) {
	case ast.Check:
		return "check(\"" + st.Msg + "\")"
	case ast.Add:
		if st.Bind != "" {
			return "letAdd(" + st.Bind + "=" + st.Entity + ")"
		}
		return "add(" + st.Entity + ")"
	case ast.Set:
		if st.Where != nil {
			return "setWhere(" + st.Entity + ")"
		}
		return "set(" + st.Entity + "." + st.Field + ")"
	case ast.Remove:
		if st.Where != nil {
			return "removeWhere(" + st.Entity + ")"
		}
		return "remove(" + st.Entity + ")"
	case ast.Clear:
		return "clear(" + st.Entity + ")"
	case ast.ServiceCall:
		if st.Bind != "" {
			return "letCall(" + st.Bind + "=" + st.Service + "." + st.Op + ")"
		}
		return "call(" + st.Service + "." + st.Op + ")"
	case ast.Do:
		if st.Bind != "" {
			return "letDo(" + st.Bind + "=" + st.Proc + ")"
		}
		return "do(" + st.Proc + ")"
	case ast.Let:
		return "let(" + st.Name + ")"
	case ast.Establish:
		return "establish"
	case ast.Assign:
		return "assign(" + st.Target + ")"
	case ast.ExprStmt:
		return "exprStmt"
	case ast.ForStmt:
		clauses := ""
		if st.Order != "" {
			clauses += " by " + st.Order
			if st.Desc {
				clauses += " desc"
			}
		}
		if st.Limit != nil {
			clauses += " limit"
		}
		return "for(" + st.Var + " in " + st.Coll + clauses + "){" + describeGoStmts(st.Body) + "}"
	case ast.IfStmt:
		out := "if{" + describeGoStmts(st.Then) + "}"
		if len(st.Else) > 0 {
			out += "else{" + describeGoStmts(st.Else) + "}"
		}
		return out
	}
	return "unsupported"
}

// goActionBody parses a dedented body through the real parser as the one
// action of a minimal app.
func goActionBody(t *testing.T, body string) ([]ast.Stmt, error) {
	t.Helper()
	var b strings.Builder
	b.WriteString("app A:\n    action doThing():\n")
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("        " + line + "\n")
	}
	app, err := parser.Parse(b.String())
	if err != nil {
		return nil, err
	}
	if len(app.Actions) != 1 {
		t.Fatalf("expected one action, got %d", len(app.Actions))
	}
	return app.Actions[0].Body, nil
}

const checkoutBody = `check cartLines > 0 "your cart is empty"
let started = now()
for l in CartLine where l.owner == actor by id:
    check Product(l.product).stock >= l.qty "sold out while you shopped"
    let total = l.qty * l.unitPrice
    set Product(l.product).stock = Product(l.product).stock - l.qty
    let oid = add Order { buyer: actor, product: l.product, total: total, placed: started }
    add OrderLine { order: oid, product: l.product }
    if total > 10000:
        lastOrder = oid
    else:
        if total > 100:
            lastOrder = 0
        else:
            lastOrder = 1
remove l in CartLine where l.owner == actor
`

const encounterBody = `let roll = rand(sum(e.weight in ZoneEncounter where e.zone == zoneId))
for e in ZoneEncounter where e.zone == zoneId && sum(x.weight in ZoneEncounter where x.zone == zoneId && x.id < e.id) <= roll && roll < sum(x.weight in ZoneEncounter where x.zone == zoneId && x.id <= e.id) limit 1:
    add Battle { player: actor, zone: zoneId, wildSpecies: e.species }
for p in Position where p.owner == actor by qty desc limit 1:
    picked = p.x
`

func TestActionStmtPortMatchesGoParserOnControlFlow(t *testing.T) {
	ts := loadActionStmtApp(t)
	// The real parser takes the range clauses in any order, so the port does too.
	outOfOrder := "for i in Item by n desc where i.n > 1 limit 2:\n    total = i.n\n"
	for name, body := range map[string]string{"checkout": checkoutBody, "encounter": encounterBody, "clauses out of order": outOfOrder} {
		t.Run(name, func(t *testing.T) {
			goBody, err := goActionBody(t, body)
			if err != nil {
				t.Fatalf("real parser refused the body: %v", err)
			}
			want := describeGoStmts(goBody)
			d := postExprJSON(t, ts, "runParseActionBody", body)
			got, _ := d["actDemoResult"].(string)
			if got != want {
				t.Errorf("selfhost port disagrees with the real parser:\n  got  %s\n  want %s", got, want)
			}
			if strings.Contains(got, "unsupported") {
				t.Errorf("the port refused a body the real parser accepts: %s", got)
			}
		})
	}
}

// What the real parser refuses, the port degrades to "unsupported" — never
// a silently different tree.
func TestActionStmtPortRefusesWhatGoRefuses(t *testing.T) {
	ts := loadActionStmtApp(t)
	cases := map[string]string{
		"for without its colon": "for i in Item\n    total = 1\n",
		"more is a view clause": "for i in Item limit 3 more go:\n    total = i.n\n",
		"let mut is proc-only":  "let mut a = 1\ntotal = a\n",
		"else without if":       "total = 1\nelse:\n    total = 2\n",
		"for with no body":      "for i in Item:\ntotal = 1\n",
		"where needs a value":   "for i in Item where:\n    total = 1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := goActionBody(t, body); err == nil {
				t.Fatalf("the real parser accepted a body this case expects it to refuse")
			}
			d := postExprJSON(t, ts, "runParseActionBody", body)
			got, _ := d["actDemoResult"].(string)
			if !strings.Contains(got, "unsupported") {
				t.Errorf("port should degrade to unsupported, got %s", got)
			}
		})
	}
}
