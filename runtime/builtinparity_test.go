package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"facet/internal/parser"
)

// clientBuiltinFuncs are the assets/facet.js functions evCall and its helpers
// are made of — the shipped source, extracted, never a copy.
var clientBuiltinFuncs = []string{
	"evCall", "isFloatNum", "roundAway", "runeSlice", "utf8Len", "mapCase", "goTrim",
	"fromJsonJS", "fromIsoJS", "ago", "isoJS", "compact", "commas", "money", "toMoney",
	"toFloatJS", "truthy", "toInt", "toStr", "numStr",
}

// clientBuiltinPrelude is the shipped client's builtin evaluator as a script
// node can run: the functions above plus the constants they read. A test
// defines ev (how evCall evaluates an argument) itself.
func clientBuiltinPrelude(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	src := string(raw)
	var b strings.Builder
	for _, name := range []string{"FA_NUMERIC", "GO_SPACE", "GO_TRIM"} {
		m := regexp.MustCompile(`(?m)^  const ` + name + ` = .*;$`).FindString(src)
		if m == "" {
			t.Fatalf("assets/facet.js has no const %s — the mirror this test checks has moved", name)
		}
		b.WriteString(strings.TrimSpace(m) + "\n")
	}
	for _, fn := range clientBuiltinFuncs {
		b.WriteString(extractFunction(t, src, fn) + "\n")
	}
	return b.String()
}

// Every builtin internal/parser's builtinSites lets a view, policy, derive or
// browser-placed action call must have a case in the browser's evCall — a
// missing one answers null after the first client re-render.
func TestClientImplementsEveryClientBuiltin(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	body := extractFunction(t, string(raw), "evCall")
	for _, name := range parser.ClientBuiltins() {
		if !strings.Contains(body, `case "`+name+`":`) {
			t.Errorf("builtin %s is SiteEverywhere (internal/parser/builtins.go) but assets/facet.js's evCall has no case for it — implement it there, or mark it SiteAuthority", name)
		}
	}
}

// parityCase is one builtin call, run by the server's callBuiltin and by the
// shipped facet.js evCall over the same arguments.
type parityCase struct {
	Name string `json:"name"`
	Args []any  `json:"args"`
}

// clientParityCases covers every client builtin (enforced below), with the
// edge cases where Go and JavaScript disagree by default: UTF-8 bytes vs UTF-16
// units, astral code points, out-of-range and negative indexes, empty
// separators, Unicode whitespace and case mapping, half-way rounding, and text
// that only one language's parser would call a number or a date.
var clientParityCases = []parityCase{
	{"abs", []any{-3}}, {"abs", []any{-2.5}}, {"abs", []any{"-7"}}, {"abs", []any{true}},
	{"min", []any{3, -1}}, {"min", []any{2.5, 3}}, {"max", []any{3, -1}}, {"max", []any{-2.5, -3}},
	{"floor", []any{7}}, {"floor", []any{-2.5}}, {"floor", []any{2.7}}, {"floor", []any{"3.9"}},
	{"round", []any{2.5}}, {"round", []any{-2.5}}, {"round", []any{0.49999999999999994}}, {"round", []any{-0.5}}, {"round", []any{4}},
	{"toFloat", []any{"1.5"}}, {"toFloat", []any{" 2e3 "}}, {"toFloat", []any{"inf"}}, {"toFloat", []any{"NaN"}}, {"toFloat", []any{"0x10"}},
	{"toFloat", []any{true}}, {"toFloat", []any{"\u00a01.25\u0085"}}, {"toFloat", []any{"\ufeff1"}}, {"toFloat", []any{3}},
	{"toInt", []any{"7"}}, {"toInt", []any{"-7.9"}}, {"toInt", []any{"1e3"}}, {"toInt", []any{"0x10"}}, {"toInt", []any{""}},
	{"toInt", []any{" 42 "}}, {"toInt", []any{"\u200042"}}, {"toInt", []any{"\ufeff42"}}, {"toInt", []any{"1e400"}}, {"toInt", []any{true}}, {"toInt", []any{nil}}, {"toInt", []any{-3.7}},
	{"toMoney", []any{"12.34"}}, {"toMoney", []any{"-0.005"}}, {"toMoney", []any{"0.125"}}, {"toMoney", []any{"$5"}}, {"toMoney", []any{"-5"}},
	{"money", []any{1234}}, {"money", []any{-5}}, {"money", []any{0}},
	{"len", []any{"héllo"}}, {"len", []any{"a😀b"}}, {"len", []any{""}}, {"len", []any{[]any{1, 2, 3}}},
	{"byteLen", []any{"hello"}}, {"byteLen", []any{"héllo"}}, {"byteLen", []any{"a😀b"}}, {"byteLen", []any{"日本"}}, {"byteLen", []any{""}}, {"byteLen", []any{42}},
	{"upper", []any{"straße"}}, {"upper", []any{"héllo ǆ"}}, {"upper", []any{"ŉ ΐ"}}, {"upper", []any{"ı"}},
	{"lower", []any{"İSTANBUL"}}, {"lower", []any{"ΣΑΣ"}}, {"lower", []any{"ẞ K"}}, {"lower", []any{"ÀB😀"}},
	{"trim", []any{"  hi  "}}, {"trim", []any{"\u0085hi\u00a0"}}, {"trim", []any{"\ufeffhi\ufeff"}}, {"trim", []any{"\u3000hi\u2028"}}, {"trim", []any{"\t\n\v\f\r"}},
	{"contains", []any{"hello", "ell"}}, {"contains", []any{"hello", ""}}, {"contains", []any{"", "x"}},
	{"take", []any{"héllo wörld", 5}}, {"take", []any{"abc", 10}}, {"take", []any{"abc", -2}}, {"take", []any{"😀😁😂", 2}},
	{"split", []any{"a,b,,c", ","}}, {"split", []any{"", ","}}, {"split", []any{"abc", ""}}, {"split", []any{"a😀b", ""}},
	{"split", []any{"", ""}}, {"split", []any{",a,", ","}}, {"split", []any{"x", "xy"}},
	{"join", []any{[]any{"a", 1, true, nil, 2.5}, "-"}}, {"join", []any{[]any{}, ","}}, {"join", []any{"notalist", ","}},
	{"slice", []any{"hello", 0, 3}}, {"slice", []any{"hello", 3, 1}}, {"slice", []any{"hello", -5, 99}}, {"slice", []any{"a😀bc", 1, 3}},
	{"slice", []any{"", 0, 1}}, {"slice", []any{"héllo", 1, 2}},
	{"charAt", []any{"hello", 1}}, {"charAt", []any{"hello", -1}}, {"charAt", []any{"hello", 5}}, {"charAt", []any{"a😀b", 1}},
	{"replace", []any{"a-b-c", "-", "+"}}, {"replace", []any{"abc", "", "-"}}, {"replace", []any{"", "", "-"}}, {"replace", []any{"a😀", "", "|"}}, {"replace", []any{"aaa", "aa", "b"}},
	{"slug", []any{"Hello, World!"}}, {"slug", []any{"İx"}}, {"slug", []any{"--Ünïcode  Straße--"}}, {"slug", []any{"KELVIN"}},
	{"year", []any{1788609600}}, {"month", []any{1788609600}}, {"day", []any{1788609600}}, {"year", []any{0}}, {"month", []any{-86400}},
	{"compact", []any{1500}}, {"compact", []any{-2500000}}, {"commas", []any{1234567}}, {"commas", []any{-1000}},
	{"iso", []any{1788609600}}, {"iso", []any{0}}, {"iso", []any{-1}},
	{"fromIso", []any{"2026-09-06T12:00:00Z"}}, {"fromIso", []any{"2026-09-06T12:00:00.123+02:00"}}, {"fromIso", []any{"2026-09-06T12:00:00-05:30"}},
	{"fromIso", []any{"2026-09-06T12:00"}}, {"fromIso", []any{"2026-09-06"}}, {"fromIso", []any{"2026-02-30T00:00:00Z"}}, {"fromIso", []any{"2024-02-29T23:59:59Z"}},
	{"fromIso", []any{"2026-09-06T24:00:00Z"}}, {"fromIso", []any{"2026-09-06t12:00:00z"}}, {"fromIso", []any{"0001-01-01T00:00:00Z"}}, {"fromIso", []any{""}},
	{"first", []any{[]any{"a", "b"}}}, {"first", []any{[]any{}}}, {"first", []any{"text"}},
	{"fromJson", []any{`{"a":[1,2.5,"x"],"b":null}`}}, {"fromJson", []any{"not json"}}, {"fromJson", []any{""}}, {"fromJson", []any{"1e400"}}, {"fromJson", []any{" 7 "}},
}

// The server and the browser must compute every client builtin identically:
// a view renders its first paint on the server and every later paint in
// facet.js, so a difference is a page that changes under the reader.
func TestClientBuiltinsMatchServer(t *testing.T) {
	covered := map[string]bool{"now": true, "rand": true, "ago": true} // nondeterministic; ago is TestClientFormattingMatchesTheServer's
	for _, c := range clientParityCases {
		covered[c.Name] = true
		if site, ok := parser.BuiltinSiteOf(c.Name); !ok || site != parser.SiteEverywhere {
			t.Errorf("parity case for %s, which is not a client builtin", c.Name)
		}
	}
	var missing []string
	for _, name := range parser.ClientBuiltins() {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("client builtins with no parity case: %v — add cases to clientParityCases", missing)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot run the client mirror")
	}
	in, _ := json.Marshal(clientParityCases)
	script := clientBuiltinPrelude(t) + `
const ev = (e) => e.val;
const cases = ` + string(in) + `;
console.log(JSON.stringify(cases.map((c) => {
  const v = evCall({ name: c.name, args: c.args.map((x) => ({ val: x })) }, {});
  return { v: v === undefined ? null : v, s: toStr(v) };
})));`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the client mirror: %v\n%s", err, out)
	}
	var client []struct {
		V any    `json:"v"`
		S string `json:"s"`
	}
	if err := json.Unmarshal(out, &client); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(client) != len(clientParityCases) {
		t.Fatalf("client answered %d cases, want %d", len(client), len(clientParityCases))
	}
	for i, c := range clientParityCases {
		server := callBuiltin(c.Name, c.Args)
		var sv any
		b, err := json.Marshal(server)
		if err != nil {
			t.Fatalf("%s%v: server value %v does not encode: %v", c.Name, c.Args, server, err)
		}
		json.Unmarshal(b, &sv)
		if !reflect.DeepEqual(sv, client[i].V) {
			t.Errorf("%s%q: server %#v, client %#v", c.Name, c.Args, sv, client[i].V)
		}
		if toStr(server) != client[i].S {
			t.Errorf("%s%q renders: server %q, client %q", c.Name, c.Args, toStr(server), client[i].S)
		}
	}
}

// The client's number rendering is the server's toStr: whole numbers as their
// digits, anything else as Go's shortest 'g' form.
func TestClientToStrMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot run the client mirror")
	}
	vals := []any{0, -7, 1.5, -0.25, 0.0001, 0.00001, 1234567.5, 123456.5, 1e20, 1e21, 9007199254740993.0, 1e-7, 3.14159, "text", true, false, nil,
		[]any{1, "a", []any{2, 3}}, map[string]any{"a": 1}}
	in, _ := json.Marshal(vals)
	script := clientBuiltinPrelude(t) + "console.log(JSON.stringify(" + string(in) + ".map(toStr)));"
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the client mirror: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	for i, v := range vals {
		// The client holds every number as a float64, as the server does for a
		// wire value; compare against the server's rendering of that.
		if n, ok := v.(int); ok {
			v = float64(n)
		}
		if want := toStr(v); got[i] != want {
			t.Errorf("toStr(%#v): client %q, server %q", v, got[i], want)
		}
	}
}
