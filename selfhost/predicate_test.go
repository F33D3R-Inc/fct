// This file verifies predicate.fct's evalKind/evalBinKind/evalBinBool/
// evalNum/evalText/valuesEqual pipeline (reached through its two public
// entry points, evalPredicateOk/evalPredicateResult) against the real,
// current Rust implementation it is a port of
// (facetql/src/core/predicate.rs's eval_at/eval_bin_op/truthy/values_equal),
// by compiling and running predicate.fct through the same in-memory server
// this package's other test files use, and driving its runPredicateScenario
// action across a battery of hand-built (tree, row) fixtures — one per real
// behavior the real Rust file's own logic defines. predicate.rs's own
// #[test]s (an_ordinary_predicate_passes and friends) test a different
// concern entirely (the MAX_PREDICATE_DEPTH/MAX_PREDICATE_STRING resource
// bounds around evaluation, not evaluation semantics itself), so this file
// does not port them; it independently derives its own cases from reading
// eval_at/eval_bin_op/truthy/values_equal directly.
package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func loadPredicateApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("predicate.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/predicate.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func runScenario(t *testing.T, ts *httptest.Server, name string) (ok, result bool) {
	t.Helper()
	d := postJSON(t, ts, "runPredicateScenario", name)
	ok, _ = d["scenarioOkResult"].(bool)
	result, _ = d["scenarioResultResult"].(bool)
	return ok, result
}

func TestPredicateScenarios(t *testing.T) {
	ts := loadPredicateApp(t)

	cases := []struct {
		name       string
		wantOk     bool
		wantResult bool
		note       string
	}{
		// values_equal: kind mismatch is always false, never coerced.
		{"typeMismatchNeverEqual", true, false, "1 == \"1\" (real Rust: Number != String structurally)"},

		// truthy: ONLY Bool and Null are special-cased — every other
		// successfully-evaluated kind (num, text, even a falsy-looking 0
		// or "") is true. This is the real Rust `truthy`'s actual rule,
		// not the "0/empty string is falsy" a reader might assume from
		// other languages.
		{"zeroIsTruthy", true, true, "bare literal 0 is truthy (not Bool/Null)"},
		{"emptyTextIsTruthy", true, true, "bare literal \"\" is truthy (not Bool/Null)"},
		{"nullIsFalsy", true, false, "null is the one non-bool falsy value"},

		// get: missing field reads as null (real Rust:
		// data.get(field).cloned().unwrap_or(Value::Null)).
		{"fieldAccessMissingIsNull", true, true, "item.missing == null when the row has no such field"},

		// &&/|| must never evaluate (let alone surface an error from) the
		// side short-circuiting skips.
		{"andShortCircuitsRight", true, false, "false && (1/0) must not divide by zero"},
		{"orShortCircuitsRight", true, true, "true || (1/0) must not divide by zero"},

		// Comparison operators require BOTH sides numeric.
		{"comparisonRequiresNumericBothSides", false, false, "\"5\" > 3 is refused, not string-coerced"},
		{"greaterThanNumeric", true, true, "5 > 3"},

		// Recursive evaluation of arithmetic/concat, checked by wrapping
		// in `== <expected>` so valuesEqual actually calls evalNum/
		// evalText on the sub-expression (evalPredicateResult alone only
		// reports truthiness of a non-bool value, never its payload).
		{"arithmeticPrecedenceViaEquality", true, true, "1 + 2 * 3 == 7"},
		{"stringConcatCorrectValue", true, true, "\"foo\" + \"bar\" == \"foobar\""},

		// Division/modulo by zero are errors, not infinities/NaN/zero.
		{"divisionByZeroIsError", false, false, "1 / 0"},
		{"moduloByZeroIsError", false, false, "1 %% 0"},

		// in / not in set membership.
		{"inSetMembership", true, true, "2 in {1,2,3}"},
		{"notInSetMembership", true, true, "5 not in {1,2,3}"},
		{"inSetSizeLimitExceeded", false, false, "a 1001-element set exceeds the 1000-element bound"},

		// String predicates.
		{"startsWith", true, true, "\"hello world\" starts_with \"hello\""},
		{"endsWith", true, true, "\"hello world\" ends_with \"world\""},
		{"contains", true, true, "\"hello world\" contains \"lo wo\""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, result := runScenario(t, ts, c.name)
			if ok != c.wantOk {
				t.Errorf("%s: ok = %v, want %v (%s)", c.name, ok, c.wantOk, c.note)
			}
			if ok && result != c.wantResult {
				t.Errorf("%s: result = %v, want %v (%s)", c.name, result, c.wantResult, c.note)
			}
		})
	}
}

// TestFieldAccessPresent covers the one scenario with a non-empty row,
// kept separate from the table above since runPredicateScenario's "row" is
// always empty except for this one name.
func TestFieldAccessPresent(t *testing.T) {
	ts := loadPredicateApp(t)
	ok, result := runScenario(t, ts, "fieldAccessPresent")
	if !ok {
		t.Fatalf("fieldAccessPresent: ok = false, want true")
	}
	if !result {
		t.Errorf("fieldAccessPresent: item.status == \"active\" should match when the row's status field is \"active\"")
	}
}
