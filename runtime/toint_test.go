package runtime

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The cases both interpreters must agree on. Every one of these crosses a real
// boundary: a route parameter, a form field, an `<input>`'s .value, a JSON API
// argument. Before numeric text was parsed, every one of them was 0.
var tointCases = []struct {
	in   string
	want int
}{
	{"1", 1},
	{"42", 42},
	{" 42 ", 42},
	{"-7", -7},
	{"+7", 7},
	{"42.7", 42}, // truncates toward zero, like float64 -> int
	{"-3.9", -3}, // ...in both directions
	{"1e3", 1000},
	{"0", 0},

	// Not numbers. These stay 0 here because toInt is total; the boundary that
	// can refuse is coerceParam.
	{"abc", 0},
	{"", 0},
	{"   ", 0},
	{"12abc", 0},
	{"0x10", 0},     // Number("0x10") is 16 in JS — must not be
	{"Infinity", 0}, // finite-looking in JS, "inf" parses in Go — must not be
	{"NaN", 0},
	{".", 0},
	{"-", 0},
}

func TestToIntParsesNumericText(t *testing.T) {
	for _, c := range tointCases {
		if got := toInt(c.in); got != c.want {
			t.Errorf("toInt(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// coerceParam is the boundary that refuses, so an unparseable argument becomes a
// 400 rather than a silent write of the wrong row.
func TestCoerceParamRefusesNonNumericText(t *testing.T) {
	for _, typ := range []string{"int", "money", "date"} {
		if v, ok := coerceParam("12", typ); !ok || toInt(v) != 12 {
			t.Errorf("coerceParam(%q, %q) = %v, %v; want 12, true", "12", typ, v, ok)
		}
		if _, ok := coerceParam("abc", typ); ok {
			t.Errorf("coerceParam(%q, %q) accepted a non-number", "abc", typ)
		}
		// An empty argument is an omitted one, not a malformed one — refusing it
		// would reject every unfilled optional form field.
		if _, ok := coerceParam("", typ); !ok {
			t.Errorf("coerceParam(%q, %q) refused an empty argument", "", typ)
		}
	}
	// Text params are unaffected.
	if v, ok := coerceParam("abc", "text"); !ok || v != "abc" {
		t.Errorf(`coerceParam("abc", "text") = %v, %v`, v, ok)
	}
}

// The refusal is by shape, not only by text. `{"args":[true]}` for an `int`
// parameter reached `toInt(true)` and wrote row 1; `{"args":[{}]}` wrote row 0 —
// the same silent write of the wrong row the string case was fixed for, entered
// through a different JSON type.
func TestCoerceParamRefusesNonNumericShapes(t *testing.T) {
	for _, typ := range []string{"int", "money", "date"} {
		for _, bad := range []any{
			true, false, // a flag is not a number, however confidently toInt reads one
			map[string]any{}, map[string]any{"id": 1},
			[]any{}, []any{1},
		} {
			if v, ok := coerceParam(bad, typ); ok {
				t.Errorf("coerceParam(%#v, %q) = %v, accepted; want refused", bad, typ, v)
			}
		}
		// The shapes a number legitimately arrives in still pass: a JSON number
		// decodes to float64, an in-process caller passes int, a driver hands back
		// the digits as bytes, and an absent argument is nil.
		for _, good := range []struct {
			in   any
			want int
		}{
			{float64(12), 12}, {float64(12.7), 12}, {int(12), 12}, {int64(12), 12},
			{[]byte("12"), 12}, {nil, 0}, {[]byte("  "), 0},
		} {
			if v, ok := coerceParam(good.in, typ); !ok || toInt(v) != good.want {
				t.Errorf("coerceParam(%#v, %q) = %v, %v; want %d, true", good.in, typ, v, ok, good.want)
			}
		}
		// Text that is bytes is refused on the same terms as the string it holds.
		if _, ok := coerceParam([]byte("abc"), typ); ok {
			t.Errorf("coerceParam([]byte(%q), %q) accepted a non-number", "abc", typ)
		}
	}

	// bool and text refuse a structured argument too — no parameter can be
	// declared record- or list-typed, so an object or array is malformed for any
	// of them, and `truthy` calls every map true while `toStr` renders it "".
	for _, typ := range []string{"bool", "text"} {
		for _, bad := range []any{map[string]any{"a": 1}, []any{"a", "b"}} {
			if v, ok := coerceParam(bad, typ); ok {
				t.Errorf("coerceParam(%#v, %q) = %v, accepted; want refused", bad, typ, v)
			}
		}
	}

	// ...but the scalar readings both interpreters share are still accepted.
	if v, ok := coerceParam(true, "bool"); !ok || v != true {
		t.Errorf(`coerceParam(true, "bool") = %v, %v`, v, ok)
	}
	if v, ok := coerceParam("", "bool"); !ok || v != false {
		t.Errorf(`coerceParam("", "bool") = %v, %v`, v, ok)
	}
	if v, ok := coerceParam(nil, "bool"); !ok || v != false {
		t.Errorf(`coerceParam(nil, "bool") = %v, %v`, v, ok)
	}
	if v, ok := coerceParam(float64(12), "text"); !ok || v != "12" {
		t.Errorf(`coerceParam(12, "text") = %v, %v`, v, ok)
	}
	if v, ok := coerceParam(nil, "text"); !ok || v != "" {
		t.Errorf(`coerceParam(nil, "text") = %v, %v`, v, ok)
	}

	// An enum or entity parameter passes through untouched: this gate is not
	// handed the IR, so it cannot tell text-valued enums from record-valued
	// entities, and refusing by shape would reject one of them wrongly.
	row := map[string]any{"id": 1}
	if v, ok := coerceParam(row, "Person"); !ok || v == nil {
		t.Errorf(`coerceParam(row, "Person") = %v, %v; want the row, true`, v, ok)
	}
}

// The two interpreters must agree. eval.go and assets/facet.js each implement
// toInt, and a value that reads as one number on the server and another on the
// client renders one way on first paint and changes the instant the client takes
// over — the exact class of bug that made this worth a cross-language test
// rather than two independent ones.
func TestClientToIntMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
const FA_NUMERIC = /^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/;
function toInt(v) {
  if (typeof v === "number") return Math.trunc(v);
  if (v === true) return 1;
  if (typeof v === "string") {
    const t = v.trim();
    if (!FA_NUMERIC.test(t)) return 0;
    return Math.trunc(Number(t));
  }
  return 0;
}
const cases = `)
	script.WriteString("[")
	for i, c := range tointCases {
		if i > 0 {
			script.WriteString(",")
		}
		script.WriteString(strconv.Quote(c.in))
	}
	script.WriteString("];\nconsole.log(cases.map(toInt).join(\",\"));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	got := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(got) != len(tointCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(tointCases))
	}
	for i, c := range tointCases {
		if got[i] != strconv.Itoa(c.want) {
			t.Errorf("client toInt(%q) = %s, server = %d", c.in, got[i], c.want)
		}
	}
}

// The literal source of the client's regex and toInt must be the ones this test
// checks — a copy that drifts from assets/facet.js proves nothing.
func TestClientMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	src := string(raw)
	for _, want := range []string{
		`const FA_NUMERIC = /^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/;`,
		`if (!FA_NUMERIC.test(t)) return 0;`,
		`return Math.trunc(Number(t));`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("assets/facet.js no longer contains %q — the mirror checked by\n"+
				"TestClientToIntMatchesServer has drifted from the shipped client", want)
		}
	}
}
