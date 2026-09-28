package runtime

// A proc reads a field of a wire `type` value — one it built (a DTO
// literal is a record, the shape every wire value has) or one another
// proc returned — the way an action's `.field` reads it. It once failed
// at run time with "not a struct" though the compiler accepts it.

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

const procWireFieldApp = `app A:
    type ResultDTO:
        error: text
        score: int
    proc score(n: int) -> ResultDTO:
        if n < 0:
            return ResultDTO{error: "negative", score: 0}
        return ResultDTO{error: "", score: n * 2}
    proc run(n: int) -> text:
        let r = do score(n)
        let local = ResultDTO{error: "x", score: 1}
        return r.error + ":" + r.score + ":" + local.error
    state result: text = ""
    action run(n: int):
        let r = do run(n)
        result = r
    view Home at "/":
        box:
            text "{result}"
`

func TestProcReadsWireTypeFields(t *testing.T) {
	g, err := compile.String(procWireFieldApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for body, want := range map[string]string{`{"args":[21]}`: ":42:x", `{"args":[-1]}`: "negative:0:x"} {
		if got := postJSON(t, ts, "run", body)["result"]; got != want {
			t.Fatalf("%s: result = %v, want %s", body, got, want)
		}
	}
}
