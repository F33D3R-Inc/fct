package selfhost

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/internal/parser"
	"facet/runtime"
)

// action_lower.fct is the self-hosted port of internal/ir/build.go's
// `(e *env) action(...)` — the action lowering. This pins it to the real
// thing: for every small app below, the action under test is built by the
// REAL compiler (compile.String) and json.Marshal'ed, and by action_lower.fct's
// `runLowerAction` over the same declarations (the environment the real build
// hands action(): states and their placements, entities and their fields/
// derives, enums, policies and their arities, the already-lowered inline
// table, service ops, proc signatures, struct names — all read off the
// compiled graph of the declarations alone), and the two JSON strings must be
// byte for byte equal. Where the real compiler refuses the action, the port
// must answer with placement "" and the Go message in `reason`.

func loadActionLowerApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("action_lower.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/action_lower.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// alowDecls is the declaration prelude every case's action is appended to.
// It compiles on its own, which is what lets an action the real compiler
// REFUSES still have an environment to be lowered against.
const alowDecls = `app ALow:
    enum Status: active, closed

    entity Product:
        name: text
        stock: int
        status: Status

    entity Order:
        product: int
        qty: int
        buyer: text

    entity Line:
        order: int
        qty: int
        derive twice: int = qty * 2

    state counter: int = 0
    state draft: text = "" @client
    state seenAt: int = 0 @client
    state pid: text = "" @private

    derive lowStock: int = count(p in Product where p.stock < counter)

    policy admin:
        role == "admin"

    policy member:
        pid != ""

    policy owns(id: int):
        Order(id).buyer == actor

    service Mailer at "http://mailer:1":
        send(to: text, body: text)
        lookup(id: int) -> text

    proc double(n: int) -> int:
        return n * 2

    proc ping():
        let x = 1

    view Home at "/":
        box:
            text "{counter}"
`

type alowCase struct {
	name    string
	action  string // the `action …:` block as it sits in the app (4-space indented)
	wantErr string // "" = must compile; else a substring of the real BuildError
}

var alowCases = []alowCase{
	{name: "client only", action: `    action typeDraft(t: text):
        draft = t
`},
	{name: "every entity write", action: `    action restock(id: int, n: int):
        check n > 0 "need a positive amount"
        set Product(id).stock = Product(id).stock + n
        set p in Product where p.stock < 0:
            stock = 0
            status = Status.closed
        remove Order(id)
        remove o in Order where o.qty == 0
        clear Line
`},
	{name: "print", action: `    action dbg():
        print("hi")
`},
	{name: "impure server", action: `    action stamp():
        counter = now()
`},
	{name: "impure client exception", action: `    action seen():
        seenAt = now()
`},
	{name: "service call with bind", action: `    action notify(to: text):
        call Mailer.send(to, "x")
        let who = call Mailer.lookup(1)
        pid = who
`},
	{name: "proc call with bind", action: `    action calc(n: int):
        do ping()
        let d = do double(n)
        counter = d
`},
	{name: "establish with role", action: `    action login(handle: text):
        establish actor handle role "member"
`},
	{name: "authoritative state write", action: `    action bump():
        counter = counter + 1
`},
	{name: "derive inlined reads its state", action: `    action useDerive():
        let low = lowStock
        if low > 3:
            counter = low
        else:
            counter = 0
`},
	{name: "requires row policy with args, optimistic", action: `    action cancel(id: int) @optimistic:
        requires owns(id), admin
        remove Order(id)
`},
	{name: "let id = add", action: `    action order(pid2: int, n: int):
        let oid = add Order { product: pid2, qty: n, buyer: actor }
        add Line { order: oid, qty: n }
`},
	{name: "nested for and if with check in loop", action: `    action audit(top: int):
        for o in Order where o.qty > 0 by qty desc limit top:
            check o.qty < 100 "too big"
            if o.qty > 10:
                set Product(o.product).stock = Product(o.product).stock - o.qty
            else:
                let t = o.qty * 2
                add Line { order: o.id, qty: t }
`},
	{name: "error: client state read by a server action", action: `    action bad1():
        add Line { order: 1, qty: seenAt }
`, wantErr: `but reads client-only state "seenAt"`},
	{name: "error: requires on a client-placed action", action: `    action bad2(t: text):
        requires admin
        draft = t
`, wantErr: "has `requires` but is client-placed"},
	{name: "error: client state written by a server action", action: `    action bad3():
        clear Line
        draft = "x"
`, wantErr: `runs on the server but writes client-only state "draft"`},
	{name: "error: local shadows a state cell", action: `    action bad4():
        let counter = 1
        clear Line
`, wantErr: `would shadow the state cell of the same name`},
}

// alowBodySrc dedents an action block's body lines (everything after the
// header) by the 8 spaces the app nests them under.
func alowBodySrc(action string) string {
	lines := strings.Split(strings.TrimRight(action, "\n"), "\n")
	var body []string
	for _, l := range lines[1:] {
		body = append(body, strings.TrimPrefix(l, "        "))
	}
	return strings.Join(body, "\n")
}

// alowEnvArgs spells the compiled declarations as the newline/colon-separated
// text lists runLowerAction takes (see action_lower.fct's driver header).
func alowEnvArgs(t *testing.T, g *ir.IR) []any {
	t.Helper()
	var states, entities, fields, derives, enums, policies, inlineNames, inlineJSON, services, procs, structs []string
	for _, s := range g.States {
		states = append(states, s.Name+":"+s.Placement)
	}
	for _, e := range g.Entities {
		entities = append(entities, e.Name)
		for _, f := range e.Fields {
			fields = append(fields, e.Name+"."+f.Name)
		}
		for _, d := range e.Derives {
			derives = append(derives, e.Name+"."+d.Name)
		}
	}
	for _, e := range g.Enums {
		enums = append(enums, e.Name)
	}
	addInline := func(name string, e *ir.Expr) {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		inlineNames = append(inlineNames, name)
		inlineJSON = append(inlineJSON, string(b))
	}
	for _, p := range g.Policies {
		policies = append(policies, fmt.Sprintf("%s:%d", p.Name, len(p.Params)))
		if len(p.Params) == 0 {
			addInline(p.Name, p.Expr)
		}
	}
	for _, d := range g.Derives {
		addInline(d.Name, d.Expr)
	}
	for _, s := range g.Services {
		for _, op := range s.Ops {
			services = append(services, fmt.Sprintf("%s.%s:%d:%s:%v", s.Name, op.Name, len(op.Params), op.Ret, op.RetList))
		}
	}
	for _, p := range g.Procs {
		procs = append(procs, fmt.Sprintf("%s:%d:%s:%v", p.Name, len(p.Params), p.Ret, p.RetList))
	}
	for _, s := range g.Structs {
		structs = append(structs, s.Name)
	}
	j := func(xs []string) string { return strings.Join(xs, "\n") }
	return []any{j(states), j(entities), j(fields), j(derives), j(enums), j(policies), j(inlineNames), j(inlineJSON), j(services), j(procs), j(structs)}
}

func TestActionLowerMatchesCompiler(t *testing.T) {
	ts := loadActionLowerApp(t)
	envG, err := compile.String(alowDecls)
	if err != nil {
		t.Fatalf("compile alowDecls: %v", err)
	}
	envArgs := alowEnvArgs(t, envG)
	for _, c := range alowCases {
		t.Run(c.name, func(t *testing.T) {
			full := alowDecls + c.action
			app, err := parser.Parse(full)
			if err != nil {
				t.Fatalf("real parser refused the fixture: %v", err)
			}
			act := app.Actions[len(app.Actions)-1]
			var params []string
			for _, p := range act.Params {
				params = append(params, p.Name+":"+p.Type)
			}
			args := append([]any{act.Name, strings.Join(params, "\n"), act.Optimistic, alowBodySrc(c.action)}, envArgs...)
			d := postExprJSON(t, ts, "runLowerAction", args...)
			got, ok := d["actionLowerResult"].(string)
			if !ok {
				t.Fatalf("no actionLowerResult delta: %+v", d)
			}
			g, err := compile.String(full)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("real compiler: want an error containing %q, got %v", c.wantErr, err)
				}
				var a ir.Action
				if err := json.Unmarshal([]byte(got), &a); err != nil {
					t.Fatalf("port's output is not an ir.Action: %v\n%s", err, got)
				}
				if a.Placement != "" || !strings.Contains(a.Reason, c.wantErr) {
					t.Fatalf("port: want placement \"\" and a reason containing %q, got placement %q reason %q", c.wantErr, a.Placement, a.Reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("real compiler refused the fixture: %v", err)
			}
			var want string
			for _, a := range g.Actions {
				if a.Name == act.Name {
					b, err := json.Marshal(a)
					if err != nil {
						t.Fatal(err)
					}
					want = string(b)
				}
			}
			if want == "" {
				t.Fatalf("action %q missing from the compiled IR", act.Name)
			}
			if got != want {
				t.Errorf("action %q:\n  got  %s\n  want %s", act.Name, got, want)
			}
		})
	}
}
