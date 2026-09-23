package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// Credentials in the language: a `@password` field (stored as a one-way hash,
// readable only by verifyPassword), the TOTP builtins (totpSecret/totpValid),
// and `sessionToken` (the caller's signed session credential). Each handles a
// secret only the authority may hold, so each is refused anywhere a client
// could evaluate it and pins the action that uses it to the server.

const credentialApp = `app C:
    type TokenDTO:
        token: text
    entity Account:
        id: int
        handle: text
        password: text @password @min(8)
        totp: text @secret
    entity BackupCode:
        id: int
        account: Account
        code: text @password
        used: bool
    action signup(handle: text, password: text) -> TokenDTO:
        add Account { handle: handle, password: password, totp: totpSecret() }
        establish actor handle
        return TokenDTO{token: sessionToken}
    action login(handle: text, password: text, code: text) -> TokenDTO:
        let id = max(a.id in Account where a.handle == handle)
        check verifyPassword(Account(id).password, password) "wrong password" status 401
        if !totpValid(Account(id).totp, code):
            let unused = count(b in BackupCode where b.account == id && !b.used)
            for b in BackupCode where b.account == id && !b.used:
                if verifyPassword(b.code, code):
                    set BackupCode(b.id).used = true
            check count(b in BackupCode where b.account == id && !b.used) < unused "invalid code" status 401
        establish actor handle
        return TokenDTO{token: sessionToken}
    action checkOnly(id: int, password: text):
        check verifyPassword(Account(id).password, password) "wrong password"
    view Home at "/":
        for a in Account by id:
            text "{a.handle}"
`

func TestPasswordFieldCompiles(t *testing.T) {
	g, err := String(credentialApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var pw ir.Field
	for _, e := range g.Entities {
		if e.Name == "Account" {
			for _, f := range e.Fields {
				if f.Name == "password" {
					pw = f
				}
			}
		}
	}
	if !pw.Password || pw.Secret {
		t.Fatalf("Account.password = %+v, want Password set (and not Secret)", pw)
	}
	// A check that consults a secret is the only thing checkOnly does, and it is
	// still enough to pin the action to the server: the hash never leaves it.
	for _, name := range []string{"signup", "login", "checkOnly"} {
		if a := findAction(t, g, name); a.Placement != ir.Server {
			t.Errorf("%s placement = %s, want server", name, a.Placement)
		}
	}
	if a := findAction(t, g, "checkOnly"); !strings.Contains(a.Reason, "authentication secret") {
		t.Errorf("checkOnly placement reason = %q, want it to name the secret", a.Reason)
	}
}

// Every way to read a @password column other than verifyPassword's first
// argument is a compile error — the hash has no other use than to leak.
func TestPasswordFieldReadsRefused(t *testing.T) {
	head := `app C:
    type DTO:
        v: text
    entity Account:
        id: int
        handle: text
        password: text @password
        nick: text
`
	cases := []struct {
		name, body, want string
	}{
		{"returned", `    action leak(id: int) -> DTO:
        return DTO{v: Account(id).password}
`, "Account.password is @password"},
		{"for variable", `    action leak() -> DTO:
        for a in Account:
            let v = a.password
        return DTO{v: ""}
`, "Account.password is @password"},
		{"let row", `    action leak(id: int) -> DTO:
        let r = Account(id)
        return DTO{v: r.password}
`, "Account.password is @password"},
		{"compared", `    action leak(id: int, p: text):
        check Account(id).password == p "no"
`, "Account.password is @password"},
		{"aggregate", `    action leak() -> DTO:
        return DTO{v: max(a.password in Account)}
`, "Account.password is @password"},
		{"filtered", `    action leak(p: text) -> DTO:
        return DTO{v: "" + count(a in Account where a.password == p)}
`, "@password"},
		{"view", `    view V at "/":
        for a in Account by id:
            text "{a.password}"
`, "Account.password is @password"},
		{"verify a plain field", `    action leak(id: int, p: text):
        check verifyPassword(Account(id).nick, p) "no"
`, "verifyPassword's first argument must be a @password field"},
		{"verify in a view", `    view V at "/":
        for a in Account by id:
            text "{verifyPassword(a.password, a.nick)}"
`, "cannot call verifyPassword"},
		{"set from another row", `    action leak(id: int, other: int):
        set Account(id).password = Account(other).password
`, "Account.password is @password"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := head + c.body
			if !strings.Contains(c.body, "view V") {
				src += "    view V at \"/\":\n        text \"x\"\n"
			}
			_, err := String(src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestPasswordModifierRules(t *testing.T) {
	for _, c := range []struct{ field, want string }{
		{"password: text @password @secret", "cannot combine @password"},
		{"password: int @password", "must be text"},
		{"password: text @password @unique", "cannot be @unique"},
	} {
		src := "app C:\n    entity A:\n        id: int\n        " + c.field + "\n    view V at \"/\":\n        text \"x\"\n"
		if _, err := String(src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.field, err, c.want)
		}
	}
}

// The TOTP builtins and sessionToken are authority-only: refused where a
// client evaluates an expression, arity-checked where they are allowed.
func TestAuthoritySecretsRefusedOutsideActions(t *testing.T) {
	for _, c := range []struct{ name, decl, want string }{
		{"totpValid in a derive", `    derive ok: bool = totpValid("A", "1")`, "cannot call totpValid"},
		{"totpSecret in a policy", `    policy p:
        totpSecret() != ""`, "cannot call totpSecret"},
		{"sessionToken in a view", `    view W at "/w":
        text "{sessionToken}"`, `unknown reference "sessionToken"`},
		{"totpValid arity", `    action a(c: text):
        check totpValid(c) "no"`, "totpValid(secret, code) takes exactly two arguments"},
		{"totpSecret arity", `    action a(c: text):
        let s = totpSecret(c)`, "totpSecret() takes no arguments"},
	} {
		src := "app C:\n" + c.decl + "\n    view V at \"/\":\n        text \"x\"\n"
		if _, err := String(src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

// An action parameter may be a list of scalars — `ids: [int]` — decoded from
// a JSON array or a comma-separated query, and usable with `in`.
func TestActionListParam(t *testing.T) {
	src := `app L:
    type DTO:
        id: int
    entity Work:
        id: int
        title: text
    action getWorks(ids: [int], tags: [text]?) -> [DTO]:
        return list(DTO{id: w.id} in Work where w.id in ids by id)
    api GET "/api/v2/works" -> getWorks
    view V at "/":
        text "x"
`
	g, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	a := findAction(t, g, "getWorks")
	if len(a.Params) != 2 || !a.Params[0].List || a.Params[0].Type != "int" || !a.Params[1].List || !a.Params[1].Optional {
		t.Fatalf("params = %+v, want ids: [int] and tags: [text]?", a.Params)
	}

	for _, c := range []struct{ name, src, want string }{
		{"list of rows", `app L:
    entity Work:
        id: int
    action a(ws: [Work]):
        remove w in Work where w.id > 0
    view V at "/":
        text "x"
`, "a list parameter holds scalars"},
		{"list path parameter", `app L:
    entity Work:
        id: int
    action a(ids: [int]):
        remove w in Work where w.id in ids
    api DELETE "/api/works/{ids}" -> a status 204
    view V at "/":
        text "x"
`, "is a list"},
	} {
		if _, err := String(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

// `revoke` and randomToken change or mint a credential, so both pin their
// action to the server; randomToken is refused where a client evaluates.
func TestRevokeAndRandomToken(t *testing.T) {
	g, err := String(`app R:
    type DTO:
        v: text
    action signOut():
        revoke session
    action mint() -> DTO:
        return DTO{v: randomToken(16)}
    view V at "/":
        text "x"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, name := range []string{"signOut", "mint"} {
		if a := findAction(t, g, name); a.Placement != ir.Server {
			t.Errorf("%s placement = %s, want server", name, a.Placement)
		}
	}
	if a := findAction(t, g, "signOut"); len(a.Body) != 1 || a.Body[0].Op != "revoke" {
		t.Fatalf("signOut body = %+v, want one revoke", a.Body)
	}
	for _, c := range []struct{ decl, want string }{
		{`    derive t: text = randomToken(4)`, "cannot call randomToken"},
		{`    action a():
        let t = randomToken()`, "randomToken(n) takes exactly one argument"},
		{`    action a():
        revoke nosuch`, `unknown reference "nosuch"`},
		{`    proc p() -> int:
        revoke "x"
        return 1`, `can't use "revoke"`},
	} {
		src := "app C:\n" + c.decl + "\n    view V at \"/\":\n        text \"x\"\n"
		if _, err := String(src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want it to contain %q", c.decl, err, c.want)
		}
	}
}
