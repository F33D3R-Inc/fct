package runtime

import (
	"testing"

	"facet/internal/ir"
)

// A misconfigured publication must fail the safe way. The default being closed
// is what makes that possible: an entry naming an entity or a policy that does
// not exist cannot be honoured, and the only two things it could do instead are
// publish the entity anyway or leave it shut.
func TestAPIReadMisconfigurationStaysClosed(t *testing.T) {
	ents := []ir.Entity{{Name: "Post"}, {Name: "Comment"}}
	pols := map[string]*ir.Policy{
		"member": {Name: "member"},
		"owns":   {Name: "owns", Params: []ir.Param{{Name: "id", Type: "int"}}},
	}

	cases := []struct {
		name    string
		spec    string
		open    map[string]string // entity -> guard, for what should be published
		problem bool
	}{
		{name: "empty publishes nothing", spec: "", open: map[string]string{}},
		{name: "a plain name is public", spec: "Post", open: map[string]string{"Post": ""}},
		{name: "a guard is kept", spec: " Post : member ", open: map[string]string{"Post": "member"}},
		{name: "star publishes every declared entity", spec: "*", open: map[string]string{"Post": "", "Comment": ""}},
		{name: "an unknown entity stays closed", spec: "Psot", open: map[string]string{}, problem: true},
		{name: "an unknown policy closes its entity", spec: "Post:nope", open: map[string]string{}, problem: true},
		// A row-level policy would need the row as an argument, which an entity
		// LIST cannot supply — accepting it would evaluate the policy against an
		// unbound parameter, which is how a gate comes to pass for everyone.
		{name: "a row-level guard closes its entity", spec: "Post:owns", open: map[string]string{}, problem: true},
		{name: "one bad entry does not close a good one", spec: "Post,Psot", open: map[string]string{"Post": ""}, problem: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, problems := apiReadFromEnv(tc.spec, ents, pols)
			if len(rules) != len(tc.open) {
				t.Fatalf("published %v, want %v", rules, tc.open)
			}
			for ent, guard := range tc.open {
				got, ok := rules[ent]
				if !ok {
					t.Fatalf("%s was not published; got %v", ent, rules)
				}
				if got.guard != guard {
					t.Errorf("%s guard = %q, want %q", ent, got.guard, guard)
				}
			}
			if tc.problem != (len(problems) > 0) {
				t.Errorf("problems = %v, want any = %v", problems, tc.problem)
			}
		})
	}
}

// A reserved table (the credential store) is not routable, so it is not
// publishable either — not even by the wildcard.
func TestAPIReadNeverPublishesAReservedEntity(t *testing.T) {
	ents := []ir.Entity{{Name: "Post"}, {Name: reservedUserEntity}}
	for _, spec := range []string{"*", reservedUserEntity} {
		rules, _ := apiReadFromEnv(spec, ents, map[string]*ir.Policy{})
		if _, published := rules[reservedUserEntity]; published {
			t.Errorf("%s=%q published the credential store", apiReadEnv, spec)
		}
	}
}
