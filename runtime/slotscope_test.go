package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A `use` block is written by the caller, so every name in it resolves in the
// caller's scope — exactly as if the block had been written inline at the `use`.
// The component it is spliced into renders in a scope layered on the caller's, so
// a parameter or loop variable of the *callee* spelled the same way used to shadow
// the caller's name and silently re-point the child at a value its author never
// saw (a `checkbox label "{label}"` handed to a `Field("", …)` rendered no words at
// all). internal/ir.specialize now renames the callee's colliding binders apart.
const slotScopeApp = `app Scope:
    state callerRows: [text] = ["CALLER-ROW"] @client
    state calleeRows: [text] = ["CALLEE-ROW"] @client
    component Inner(b: text):
        box:
            text "INNER={b}"
            slot
    component Mid(b: text):
        use Inner("INNER-ARG"):
            text "MID-KID={b}"
            slot
    component Outer(b: text):
        use Mid("MID-ARG"):
            text "OUTER-KID={b}"
    component Loop():
        for row in calleeRows:
            box:
                slot
    view H at "/":
        box:
            use Outer("OUTER")
            for row in callerRows:
                use Loop():
                    text "LOOP-KID={row}"
`

func TestSlotChildrenResolveInTheCallersScope(t *testing.T) {
	g, err := compile.String(slotScopeApp)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	html := string(httpGetBytes(t, ts.URL+"/"))
	for _, want := range []string{
		// Three levels of children, each reading the `b` of the component that
		// wrote it: the callee's own body, its caller's, and its caller's caller's.
		"INNER=INNER-ARG",
		"MID-KID=MID-ARG",
		"OUTER-KID=OUTER",
		// A loop variable is a binder like a parameter: the child reads the row of
		// the `for` it was written in, not the one it renders inside.
		"LOOP-KID=CALLER-ROW",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in rendered page:\n%s", want, html)
		}
	}
}
