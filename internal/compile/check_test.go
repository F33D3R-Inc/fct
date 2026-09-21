package compile

import (
	"strings"
	"testing"
)

// A `check` is a body statement in source order, so it can validate a value bound
// earlier by `let` (the gap that previously forced validation-via-brain-status).
const postBindApp = `app V:
    service Verity at "http://verity:8095":
        verify(handle: text, sig: text) -> text
    entity Account:
        id: int
        handle: text
        pid: text
    action enroll(handle: text, sig: text):
        let uuid = call Verity.verify(handle, sig)
        check uuid != "" "device signature rejected"
        add Account { handle: handle, pid: uuid }
    view Home at "/":
        box:
            text "{count(Account)}"
`

func TestCheckValidatesBoundResult(t *testing.T) {
	g, err := String(postBindApp)
	if err != nil {
		t.Fatalf("a check after a let should compile, got: %v", err)
	}
	// The body order is: call (bind), check, add — the check sits between them.
	var ops []string
	for _, a := range g.Actions {
		if a.Name == "enroll" {
			for _, st := range a.Body {
				ops = append(ops, st.Op)
			}
		}
	}
	want := []string{"call", "check", "add"}
	if strings.Join(ops, ",") != strings.Join(want, ",") {
		t.Fatalf("body op order = %v, want %v", ops, want)
	}
}

// A check no longer has to precede every mutation: the runtime rolls the whole
// action back when one fails (runtime/undo.go), so a guard written after a write
// — or inside a `for` body, next to the per-row write it guards — is a guard on a
// transaction, not a hole in one. Both shapes the old ordering rule refused now
// compile, and lower in the order they were written.
func TestValidationMayFollowMutation(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"check after a mutation",
			"add Account { handle: handle, pid: \"x\" }\n        check handle != \"\" \"bad\"",
			"add,check",
		},
		{
			"let after a mutation",
			"add Account { handle: handle, pid: \"x\" }\n        let uuid = call Verity.verify(handle, sig)",
			"add,call",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(postBindApp,
				"let uuid = call Verity.verify(handle, sig)\n        check uuid != \"\" \"device signature rejected\"\n        add Account { handle: handle, pid: uuid }",
				c.body, 1)
			g, err := String(src)
			if err != nil {
				t.Fatalf("should compile now that a failed check rolls the action back, got: %v", err)
			}
			var ops []string
			for _, a := range g.Actions {
				if a.Name == "enroll" {
					for _, st := range a.Body {
						ops = append(ops, st.Op)
					}
				}
			}
			if got := strings.Join(ops, ","); got != c.want {
				t.Fatalf("body op order = %s, want %s", got, c.want)
			}
		})
	}
}
