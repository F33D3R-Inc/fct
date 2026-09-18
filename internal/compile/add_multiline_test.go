package compile

import (
	"reflect"
	"strings"
	"testing"

	"facet/internal/ir"
)

// A record literal spanning multiple lines is the normal way to write an
// `add` with more than a couple of fields — plain readability once the field
// list stops fitting on one line. The offside rule already nests continuation
// lines under a header exactly this way for `if`/`for`/`policy` bodies, so
// `add`'s record literal (fields, then the closing `}`) is written indented
// under the `add` line, not aligned with it — the same rule as everywhere
// else in the language, just applied to a literal instead of a block.
const multilineAddApp = `app RecordLit:
    entity Widget:
        id: int
        name: text
        qty: int
        price: int
    action make(name: text, qty: int, price: int):
        add Widget {
            name: name,
            qty: qty,
            price: price
            }
    view Home at "/":
        box:
            for w in Widget by id:
                text "{w.name}"
`

const singlelineAddApp = `app RecordLit:
    entity Widget:
        id: int
        name: text
        qty: int
        price: int
    action make(name: text, qty: int, price: int):
        add Widget { name: name, qty: qty, price: price }
    view Home at "/":
        box:
            for w in Widget by id:
                text "{w.name}"
`

// addStmtOf compiles src and returns the `add` statement inside action
// "make", failing the test if compilation fails or no such statement exists.
func addStmtOf(t *testing.T, src string) ir.Stmt {
	t.Helper()
	g, err := String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, a := range g.Actions {
		if a.Name != "make" {
			continue
		}
		for _, st := range a.Body {
			if st.Op == "add" {
				return st
			}
		}
	}
	t.Fatal("no add statement found in action \"make\"")
	return ir.Stmt{}
}

// (a) A multi-line record literal, with fields on separate lines the way
// anyone would naturally format more than a couple of them, must compile to
// exactly the same statement a single-line equivalent produces.
func TestMultilineAddRecordMatchesSingleLine(t *testing.T) {
	multi := addStmtOf(t, multilineAddApp)
	single := addStmtOf(t, singlelineAddApp)

	if multi.Entity != "Widget" {
		t.Fatalf("multi-line add: Entity = %q, want %q", multi.Entity, "Widget")
	}
	if !reflect.DeepEqual(multi, single) {
		t.Fatalf("multi-line add produced a different statement than single-line:\nmulti:  %#v\nsingle: %#v", multi, single)
	}
	wantFields := []string{"name", "qty", "price"}
	var gotFields []string
	for _, f := range multi.Fields {
		gotFields = append(gotFields, f.Name)
	}
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Fatalf("multi-line add field order = %v, want %v", gotFields, wantFields)
	}
}

// (b) Regression: the single-line form — the common case today — must keep
// compiling exactly as it always has.
func TestSinglelineAddRecordStillCompiles(t *testing.T) {
	st := addStmtOf(t, singlelineAddApp)
	if st.Entity != "Widget" {
		t.Fatalf("Entity = %q, want %q", st.Entity, "Widget")
	}
	if len(st.Fields) != 3 {
		t.Fatalf("want 3 fields, got %d: %v", len(st.Fields), st.Fields)
	}
}

// (c) A record literal that genuinely never closes must still fail to
// compile with a sensible error — not hang, and not get parsed as if the
// following, unrelated action were part of the record.
const unterminatedAddApp = `app RecordLit:
    entity Widget:
        id: int
        name: text
    action make(name: text):
        add Widget {
            name: name
    action other():
        add Widget { name: "x" }
    view Home at "/":
        box:
            for w in Widget by id:
                text "{w.name}"
`

func TestUnterminatedAddRecordFailsCleanly(t *testing.T) {
	_, err := String(unterminatedAddApp)
	if err == nil {
		t.Fatal("an add with an unterminated `{` compiled without error")
	}
	if !strings.Contains(err.Error(), "closing") && !strings.Contains(err.Error(), "record") {
		t.Fatalf("error should describe the unterminated record, got: %v", err)
	}
}
