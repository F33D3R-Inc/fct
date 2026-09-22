package runtime

import (
	"strings"
	"testing"

	"facet/internal/compile"
)

// Three shapes every self-host port had to work around: a mutable local bound
// straight from a proc call, a mutable local reassigned from a proc call
// (including one declared outside the loop that reassigns it), and a struct
// field written in place.
const procAssignApp = `app P:
    struct Pt:
        x: int
        y: int
        tag: text
    proc double(n: int) -> int:
        return n * 2
    proc build(n: int) -> [int]:
        let mut out = []
        let mut i = 0
        loop i < n:
            out = append(out, i)
            i = i + 1
        return out
    proc run() -> text:
        let mut acc = do double(1)
        let mut i = 0
        loop i < 3:
            acc = do double(acc)
            i = i + 1
        let mut xs = do build(2)
        xs = do build(4)
        let mut p = Pt{x: 1, y: 2, tag: "a"}
        p.x = acc
        p.tag = p.tag + "b"
        let q = p
        p.y = 99
        return acc + " " + len(xs) + " " + p.x + "," + p.y + "," + p.tag + " " + q.y
    state got: text = ""
    action go():
        let r = do run()
        got = r
    view Home at "/":
        text "{got}"
`

func TestProcMutableBindsAndFieldWrites(t *testing.T) {
	g, err := compile.String(procAssignApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "go", nil); err != nil {
		t.Fatal(err)
	}
	// acc: 2 → 4 → 8 → 16; xs rebuilt to 4 elements; p.x = 16, p.tag "ab",
	// p.y 99 after q copied it at 2.
	if got, want := srv.StateValue("got"), "16 4 16,99,ab 2"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestProcAssignmentGates(t *testing.T) {
	base := `app P:
    struct Pt:
        x: int
        tag: text
    proc one() -> int:
        return 1
    proc none():
        let a = 1
    proc run() -> int:
        BODY
    view Home at "/":
        text "x"
`
	cases := []struct{ name, body, want string }{
		{"reassign an immutable local from a call", "let a = do one()\n        a = do one()\n        return a", "is not mutable"},
		{"reassign an undeclared local from a call", "a = do one()\n        return a", "is not declared"},
		{"assign a no-return proc", "let mut a = 1\n        a = do none()\n        return a", "returns nothing"},
		{"field write on an immutable struct", "let p = Pt{x: 1, tag: \"a\"}\n        p.x = 2\n        return p.x", "is not mutable"},
		{"field write on a non-struct", "let mut n = 1\n        n.x = 2\n        return n", "is not a struct"},
		{"unknown field", "let mut p = Pt{x: 1, tag: \"a\"}\n        p.z = 2\n        return p.x", "has no field"},
		{"wrong field type", "let mut p = Pt{x: 1, tag: \"a\"}\n        p.x = \"no\"\n        return p.x", "cannot assign text to field"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compile.String(strings.Replace(base, "BODY", c.body, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}
