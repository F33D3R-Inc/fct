package runtime

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A step's `as` applies to an expectation as it does to an action: a
// visibility rule is a rule about someone, and "what does bob see" has to be
// writable as a step evaluated as bob.
func TestSidecarExpectHonoursStepIdentity(t *testing.T) {
	g, err := compile.String(`
app Vis:
    entity Post:
        id: int
        author: text
    derive mine: bool = exists(p in Post where p.author == actor)
    action write:
        add Post { author: actor }
    view M:
        text "{count(Post)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	suite := `{"tests":[{"name":"per-step as on expect","as":{"actor":"ada","role":"member"},"steps":[
	  {"run":"write"},
	  {"expect":"mine","equals":true},
	  {"as":{"actor":"bob","role":"member"},"expect":"mine","equals":false},
	  {"expect":"mine","equals":true}
	]}]}`
	var out bytes.Buffer
	pass, fail, err := RunTests(g, []byte(suite), &out)
	if err != nil {
		t.Fatal(err)
	}
	if pass != 1 || fail != 0 {
		t.Fatalf("pass=%d fail=%d:\n%s", pass, fail, out.String())
	}
	if !strings.Contains(out.String(), "ok — per-step as on expect") {
		t.Errorf("unexpected report:\n%s", out.String())
	}
}
