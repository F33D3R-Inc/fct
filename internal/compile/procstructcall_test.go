package compile

import (
	"strings"
	"testing"
)

// A proc local bound from a call of another proc has that proc's declared
// return type: a struct result is a struct local whose fields can be written
// (`let mut q = empty(); q.n = 1`), and a field write of the wrong type or to
// a field the struct lacks is refused as it is for a literal-built local.
func TestProcCallResultIsATypedLocal(t *testing.T) {
	src := `app P:
    struct Q:
        n: int
        words: [text]
    proc empty() -> Q:
        return Q{n: 0, words: []}
    proc build(w: text) -> Q:
        let mut q = empty()
        q.n = q.n + 1
        q.words = append(q.words, w)
        return q
    action a(w: text) -> int:
        let q = build(w)
        return q.n
`
	if _, err := String(src); err != nil {
		t.Fatalf("a struct local from a proc call must take field writes: %v", err)
	}
	bad := strings.Replace(src, "q.n = q.n + 1", `q.n = "one"`, 1)
	if _, err := String(bad); err == nil {
		t.Error("a text written to an int field of a call-bound struct local must not compile")
	}
	missing := strings.Replace(src, "q.n = q.n + 1", "q.nope = 1", 1)
	if _, err := String(missing); err == nil || !strings.Contains(err.Error(), "no field") {
		t.Errorf("a write to a field the struct lacks must be refused, got %v", err)
	}
}
