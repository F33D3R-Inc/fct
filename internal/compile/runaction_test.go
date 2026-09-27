package compile

import (
	"strings"
	"testing"
)

// `run name(args)` in an action invokes another server action inside the
// caller's transaction; the compiler checks the target, its arity, a bind's
// return type, and that no chain of runs comes back round.
func TestRunActionCompiles(t *testing.T) {
	base := `app R:
    entity N:
        id: int
        v: int
    state flag: bool = false @client
    action add1(v: int) -> int:
        let id = add N { v: v }
        return id
    action twice(v: int):
        run add1(v)
        let second = run add1(v + 1)
        check second > 0 "bound"
    action toggle:
        flag = true
    %s
    view Home at "/":
        text "hi"
`
	if _, err := String(strings.Replace(base, "%s", "", 1)); err != nil {
		t.Fatalf("valid runs must compile: %v", err)
	}
	cases := []struct{ decl, want string }{
		{"action bad:\n        run nope(1)", `run names unknown action "nope"`},
		{"action bad:\n        run add1(1, 2)", `action "add1" expects 1 argument(s), got 2`},
		{"action bad:\n        let x = run twice(1)\n        check x > 0 \"x\"", `action "twice" returns nothing`},
		{"action bad(v: int):\n        run bad(v)", "cannot run itself"},
		{"action pa:\n        run pb\n    action pb:\n        run pa", "run each other in a cycle"},
		{"action bad:\n        run toggle", "client-placed"},
	}
	for _, c := range cases {
		_, err := String(strings.Replace(base, "%s", c.decl, 1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s\n  err = %v, want %q", c.decl, err, c.want)
		}
	}
}
