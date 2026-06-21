package compile

import (
	"strings"
	"testing"
)

// A brain returns a structured object; a `record` types it, and a `let` binds the
// whole thing so the action can read its fields. This is the fast-follow to
// request→response service calls — the reason records exist.
const recordApp = `app A:
    state lastScore: int = 0
    state notes: [text] = []
    record Verdict:
        score: int
        reasons: [text]
        ok: bool
    service Brain at "http://brain:9000":
        moderate(body: text) -> Verdict
    action post(body: text):
        let v = call Brain.moderate(body)
        check v.ok "rejected by the brain"
        lastScore = v.score
        notes = v.reasons
    view Home at "/":
        box:
            text "hi"
`

func TestRecordCompiles(t *testing.T) {
	g := mustCompile(t, recordApp)
	if len(g.Records) != 1 || g.Records[0].Name != "Verdict" {
		t.Fatalf("record missing from IR: %+v", g.Records)
	}
	if len(g.Records[0].Fields) != 3 {
		t.Fatalf("expected 3 record fields, got %+v", g.Records[0].Fields)
	}
	// The list field keeps its list-ness for runtime coercion.
	var reasons bool
	for _, f := range g.Records[0].Fields {
		if f.Name == "reasons" {
			reasons = f.List && f.Type == "text"
		}
	}
	if !reasons {
		t.Errorf("record field `reasons` should be a list of text: %+v", g.Records[0].Fields)
	}
	// The op carries the record name as its return type, so the runtime decodes it.
	if g.Services[0].Ops[0].Ret != "Verdict" {
		t.Errorf("service op should return the record type, got %q", g.Services[0].Ops[0].Ret)
	}
	// Reading a brain calls a service → the action is server-placed.
	for _, act := range g.Actions {
		if act.Name == "post" && act.Placement != "server" {
			t.Errorf("a record-binding action that calls a service must be server-placed, got %q", act.Placement)
		}
	}
}

func TestRecordUnknownFieldRejected(t *testing.T) {
	src := strings.Replace(recordApp, `check v.ok "rejected by the brain"`, `check v.nope "x"`, 1)
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "no field") {
		t.Fatalf("expected an unknown-record-field error, got %v", err)
	}
}

func TestServiceReturnUnknownTypeRejected(t *testing.T) {
	// A capitalized return type that names no record/enum is a compile error.
	src := `app A:
    service Brain at "http://b:9000":
        moderate(body: text) -> Nope
    action post(body: text):
        let v = call Brain.moderate(body)
        check v.ok "x"
    view H at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "unknown type") {
		t.Fatalf("expected an unknown-return-type error, got %v", err)
	}
}

func TestRecordNestedRejected(t *testing.T) {
	src := `app A:
    record Inner:
        x: int
    record Outer:
        inner: Inner
    service Brain at "http://b:9000":
        f() -> Outer
    action go():
        let v = call Brain.f()
        check v.x "x"
    view H at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "flat") {
		t.Fatalf("expected a flat-record error for a nested record field, got %v", err)
	}
}

func TestRecordListFieldAccessRejected(t *testing.T) {
	// `-> [Verdict]` binds a list; accessing a field on the whole list is an error.
	src := `app A:
    record Verdict:
        score: int
    service Brain at "http://b:9000":
        rank(body: text) -> [Verdict]
    action go(body: text):
        let vs = call Brain.rank(body)
        check vs.score "x"
    view H at "/":
        box:
            text "hi"
`
	if _, err := String(src); err == nil || !strings.Contains(err.Error(), "list of Verdict") {
		t.Fatalf("expected a list-of-record access error, got %v", err)
	}
}
