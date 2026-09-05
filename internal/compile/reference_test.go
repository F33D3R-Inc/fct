package compile

import (
	"strings"
	"testing"
)

// The refusals that go with reference parameters, component children, and a
// computed link destination. Each one is a place the language now accepts more,
// so each one needs a stated boundary — and the first is the bug that made the
// feature dangerous rather than merely absent: a block written under a `use` was
// parsed and then thrown away, so a wrapper that rendered nothing looked correct.
func TestTheBoundariesOfComponentReferences(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a block handed to a component with no slot is not silently discarded",
		src: `app A:
    component C(t: text):
        text "{t}"
    view V at "/":
        use C("x"):
            text "these children had nowhere to go"
`,
		want: "takes no children",
	}, {
		name: "a reference argument must be a name, not an expression",
		src: `app A:
    state d: text @client
    component C(v: cell text):
        input bind v
    view V at "/":
        use C(d + "x")
`,
		want: "pass the name of a state cell",
	}, {
		name: "a cell argument must have the parameter's type",
		src: `app A:
    state n: int @client
    component C(v: cell text):
        input bind v
    view V at "/":
        use C(n)
`,
		want: `is ` + "`cell text`" + `, but "n" is int`,
	}, {
		name: "the placement rule still applies to the cell the call site named",
		src: `app A:
    state d: text @server
    component C(v: cell text):
        input bind v
    view V at "/":
        use C(d)
`,
		want: "requires a @client state",
	}, {
		name: "a template is checked even when nothing uses it",
		src: `app A:
    component C(v: cell text):
        input bind v
        text "{nosuch}"
    view V at "/":
        text "hi"
`,
		want: `unknown reference "nosuch"`,
	}, {
		name: "a destination is a path the compiler can check or a whole route, not half of each",
		src: `app A:
    component C(h: text):
        link "x" -> "{h}/edit"
    view V at "/":
        use C("/a")
`,
		want: "starts with an interpolation but is not one",
	}, {
		name: "a literal destination is still route-checked",
		src: `app A:
    component C(t: text):
        link "{t}" -> "/nope"
    view V at "/":
        use C("x")
`,
		want: "no view serves that route",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("expected a compile error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a compile error containing %q, got: %v", c.want, err)
			}
		})
	}
}
