package compile

import (
	"strings"
	"testing"
)

// A field gated by `@requires(policy)` lowers with its read policy recorded, so the
// runtime can strip it from the data projections.
const fieldAuthApp = `app Dir:
    auth
    policy admins:
        role == "admin"
    entity Person:
        id: int
        name: text
        salary: money @requires(admins)
    action add(name: text, salary: money):
        add Person { name: name, salary: salary }
    view Home at "/":
        box:
            text "{count(Person)}"
`

func TestFieldReadPolicyLowers(t *testing.T) {
	g, err := String(fieldAuthApp)
	if err != nil {
		t.Fatalf("a @requires field gate should compile, got: %v", err)
	}
	var got string
	for _, e := range g.Entities {
		if e.Name == "Person" {
			for _, f := range e.Fields {
				if f.Name == "salary" {
					got = f.ReadPolicy
				}
			}
		}
	}
	if got != "admins" {
		t.Fatalf("salary.ReadPolicy = %q, want admins", got)
	}
}

func TestFieldAuthErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"unknown policy",
			`app A:
    entity P:
        id: int
        x: text @requires(nope)
    view H at "/":
        box:
            text "y"`,
			"unknown policy",
		},
		{
			"row-level policy",
			`app A:
    policy owns(p: int):
        p == 1
    entity P:
        id: int
        x: text @requires(owns)
    view H at "/":
        box:
            text "y"`,
			"zero-argument policy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := String(tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
		})
	}
}
