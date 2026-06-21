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

func TestValidationMustPrecedeMutation(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{
			"check after a mutation",
			"add Account { handle: handle, pid: \"x\" }\n        check handle != \"\" \"bad\"",
			"must come before any mutation",
		},
		{
			"let after a mutation",
			"add Account { handle: handle, pid: \"x\" }\n        let uuid = call Verity.verify(handle, sig)",
			"must come before any mutation",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(postBindApp,
				"let uuid = call Verity.verify(handle, sig)\n        check uuid != \"\" \"device signature rejected\"\n        add Account { handle: handle, pid: uuid }",
				c.body, 1)
			_, err := String(src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
