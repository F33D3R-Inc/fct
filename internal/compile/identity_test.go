package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// PIAL-style custom identity: a verify-action calls an identity brain
// (request→response), stores the returned UUID in a @private cell, and adopts the
// session with `establish actor <handle>`. The UUID keys policy but never renders.
const identityApp = `app P:
    service Elohim at "http://elohim:8093":
        verify(handle: text, sig: text) -> text
    state pid: text @private
    policy member:
        pid != ""
    action login(handle: text, sig: text):
        let uuid = call Elohim.verify(handle, sig)
        pid = uuid
        establish actor handle
    view Home at "/":
        box:
            text "signed in as {actor}"
`

func TestPrivateStateAndEstablish(t *testing.T) {
	g, err := String(identityApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// @private is authoritative (server placement) and flagged private.
	var pid ir.State
	for _, s := range g.States {
		if s.Name == "pid" {
			pid = s
		}
	}
	if pid.Placement != "server" || !pid.Private {
		t.Errorf("pid placement=%q private=%v, want server + private", pid.Placement, pid.Private)
	}
	// login establishes identity → server-placed, with an establish stmt in the body.
	var login ir.Action
	for _, a := range g.Actions {
		if a.Name == "login" {
			login = a
		}
	}
	if login.Placement != "server" {
		t.Errorf("an action that establishes identity must be server-placed, got %q", login.Placement)
	}
	var hasEstablish bool
	for _, st := range login.Body {
		if st.Op == "establish" {
			hasEstablish = true
		}
	}
	if !hasEstablish {
		t.Error("login lost its establish statement")
	}
}

func TestPrivateValueErrors(t *testing.T) {
	cases := []struct{ name, repl, with, want string }{
		{
			"render a private value",
			`text "signed in as {actor}"`, `text "id {pid}"`,
			"cannot be rendered",
		},
		{
			"establish actor from a private value",
			"establish actor handle", "establish actor pid",
			"cannot be a @private value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(identityApp, c.repl, c.with, 1)
			_, err := String(src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// A @private value is fine in a policy (it keys authorization) — only rendering
// and identity-assignment are barred. This must compile cleanly.
func TestPrivateAllowedInPolicy(t *testing.T) {
	if _, err := String(identityApp); err != nil {
		t.Fatalf("a @private value should be usable in a policy, got: %v", err)
	}
}
