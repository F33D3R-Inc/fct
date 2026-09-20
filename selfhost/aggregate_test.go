// This file verifies aggregate.fct's Accumulator fold/finish pipeline
// (runAggregateOverValues, wired through parseFieldValuesText and
// aggregateOverValues) against the real, current Rust implementation it is
// a port of (facetql/src/core/aggregate.rs), by compiling and running
// aggregate.fct through the same in-memory server the rest of this
// package's tests use, and re-running that Rust file's own #[cfg(test)]
// cases against the .fct port instead of only trusting its demo procs'
// fixed, hand-traced values.
//
// Not covered here: AggSpec::new/AggFunc::parse's own validation
// (aggregate.fct's newAggSpec/parseAggFunc) — this file's action surface
// only wires up the Accumulator fold/finish path (the numeric correctness
// this task cared about), not those two separately-testable procs. They
// already have fixed demo coverage (demoSpecValidation/demoParseUnknown)
// inside aggregate.fct itself.
package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// loadAggregateApp mirrors loadApp/loadExprApp's own compile-and-boot
// pattern for their own files, adapted to aggregate.fct (a single,
// import-free file, so compile.File — not compile.String — only because
// that's what this package's other single-file loaders already use).
func loadAggregateApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("aggregate.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/aggregate.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// encodeFieldValues builds the `kind:payload` pipe-joined text
// aggregate.fct's own parseFieldValuesText decodes, matching the format
// documented at that proc's call site.
func encodeFieldValues(specs ...string) string {
	return strings.Join(specs, "|")
}

func num(v float64) string  { return "num:" + strconv.FormatFloat(v, 'g', -1, 64) }
func txt(v string) string   { return "text:" + v }
func boolean(v bool) string { return "bool:" + strconv.FormatBool(v) }

const (
	absentSpec = "absent:"
	nullSpec   = "null:"
)

func runAgg(t *testing.T, ts *httptest.Server, fn, field string, valuesText string) map[string]any {
	t.Helper()
	return postJSON(t, ts, "runAggregateOverValues", fn, field, valuesText)
}

func TestSumOfIntegersIsFortyTwo(t *testing.T) {
	ts := loadAggregateApp(t)
	d := runAgg(t, ts, "sum", "f", encodeFieldValues(num(1), num(2), num(39)))
	assertAggOk(t, d)
	assertAggNum(t, d, 42)
}

func TestOneFloatMakesTheWholeSumAFloat(t *testing.T) {
	ts := loadAggregateApp(t)
	d := runAgg(t, ts, "sum", "f", encodeFieldValues(num(1), num(0.5)))
	assertAggOk(t, d)
	assertAggNum(t, d, 1.5)
}

func TestAnIntegerSumWiderThanI64IsStillRight(t *testing.T) {
	ts := loadAggregateApp(t)
	const i64Max = float64(9223372036854775807)
	d := runAgg(t, ts, "sum", "f", encodeFieldValues(num(i64Max), num(i64Max)))
	assertAggOk(t, d)
	assertAggNum(t, d, i64Max*2.0)
}

func TestTheEmptySumIsZeroAndTheEmptyAverageIsNull(t *testing.T) {
	ts := loadAggregateApp(t)

	d := runAgg(t, ts, "sum", "f", "")
	assertAggOk(t, d)
	assertAggNum(t, d, 0)

	d = runAgg(t, ts, "avg", "f", "")
	assertAggOk(t, d)
	assertAggKind(t, d, "null")

	d = runAgg(t, ts, "min", "f", "")
	assertAggOk(t, d)
	assertAggKind(t, d, "null")

	d = runAgg(t, ts, "max", "f", "")
	assertAggOk(t, d)
	assertAggKind(t, d, "null")

	d = runAgg(t, ts, "count", "f", "")
	assertAggOk(t, d)
	assertAggNum(t, d, 0)
}

func TestAnAverageDividesByTheRowsThatHadAValue(t *testing.T) {
	ts := loadAggregateApp(t)
	// Two rows matched but carry nothing at `f` — 3, not 1.5: the rows
	// with no rating are not zeroes.
	d := runAgg(t, ts, "avg", "f", encodeFieldValues(num(2), num(4), absentSpec, nullSpec))
	assertAggOk(t, d)
	assertAggNum(t, d, 3)
}

func TestCountCountsRowsWhateverTheFieldHolds(t *testing.T) {
	ts := loadAggregateApp(t)
	d := runAgg(t, ts, "count", "f", encodeFieldValues(absentSpec, nullSpec, txt("text")))
	assertAggOk(t, d)
	assertAggNum(t, d, 3)
	if rows, _ := d["aggRowsResult"].(float64); rows != 3 {
		t.Errorf("rows = %v, want 3", d["aggRowsResult"])
	}
}

func TestASumOverTextIsRefusedRatherThanSkipped(t *testing.T) {
	ts := loadAggregateApp(t)
	d := runAgg(t, ts, "sum", "f", encodeFieldValues(num(1), txt("12")))
	ok, _ := d["aggOkResult"].(bool)
	if ok {
		t.Fatalf("sum over a text value should be refused, got ok=true: %+v", d)
	}
	errMsg, _ := d["aggErrResult"].(string)
	if !strings.Contains(errMsg, "sum") {
		t.Errorf("error %q should mention sum", errMsg)
	}
	if !strings.Contains(errMsg, `"12"`) {
		t.Errorf("error %q should quote the offending text", errMsg)
	}
}

func TestMinAndMaxOrderTextAsTheIndexesDo(t *testing.T) {
	ts := loadAggregateApp(t)

	d := runAgg(t, ts, "min", "f", encodeFieldValues(txt("pear"), txt("apple"), txt("quince")))
	assertAggOk(t, d)
	assertAggText(t, d, "apple")

	d = runAgg(t, ts, "max", "f", encodeFieldValues(txt("pear"), txt("apple"), txt("quince")))
	assertAggOk(t, d)
	assertAggText(t, d, "quince")
}

func assertAggOk(t *testing.T, d map[string]any) {
	t.Helper()
	if ok, _ := d["aggOkResult"].(bool); !ok {
		t.Fatalf("expected ok=true, got %+v", d)
	}
}

func assertAggKind(t *testing.T, d map[string]any, want string) {
	t.Helper()
	if got, _ := d["aggResultKind"].(string); got != want {
		t.Errorf("result kind = %q, want %q (%+v)", got, want, d)
	}
}

func assertAggNum(t *testing.T, d map[string]any, want float64) {
	t.Helper()
	assertAggKind(t, d, "num")
	raw, _ := d["aggResultNum"].(string)
	got, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("aggResultNum %q did not parse as a float: %v", raw, err)
	}
	if got != want {
		t.Errorf("result num = %v, want %v", got, want)
	}
}

func assertAggText(t *testing.T, d map[string]any, want string) {
	t.Helper()
	assertAggKind(t, d, "text")
	if got, _ := d["aggResultText"].(string); got != want {
		t.Errorf("result text = %q, want %q", got, want)
	}
}
