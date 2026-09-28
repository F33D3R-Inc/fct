package selfhost

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"facet/internal/ir"
	"facet/internal/parser"
)

// action_lower.fct's alowServerOnlyBuiltins is the port of every builtin
// internal/parser/builtins.go does not mark SiteEverywhere; the two lists must
// be the same set, or the ported compiler places an action differently.
func TestServerOnlyBuiltinsMatchParser(t *testing.T) {
	raw, err := os.ReadFile("action_lower.fct")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)proc alowServerOnlyBuiltins\(\) -> \[text\]:\s*return \[(.*?)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("action_lower.fct has no alowServerOnlyBuiltins list")
	}
	var ported []string
	for _, q := range regexp.MustCompile(`"(\w+)"`).FindAllSubmatch(m[1], -1) {
		ported = append(ported, string(q[1]))
	}
	var want []string
	for _, n := range parser.Builtins() {
		if s, _ := parser.BuiltinSiteOf(n); s != parser.SiteEverywhere {
			want = append(want, n)
		}
	}
	sort.Strings(ported)
	if strings.Join(ported, " ") != strings.Join(want, " ") {
		t.Errorf("alowServerOnlyBuiltins drifted from internal/parser/builtins.go:\n ported %v\n parser %v", ported, want)
	}
}

// fctTextList is the quoted names of proc name's `return [...]` in file.
func fctTextList(t *testing.T, file, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)proc ` + name + `\(\) -> \[text\]:\s*return \[(.*?)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s has no %s list", file, name)
	}
	var out []string
	for _, q := range regexp.MustCompile(`"(\w+)"`).FindAllSubmatch(m[1], -1) {
		out = append(out, string(q[1]))
	}
	sort.Strings(out)
	return out
}

// check.fct's callSiteDiag refuses a proc-only builtin outside a proc the
// way checkBuiltins does: procSiteBuiltins is parser.SiteProc, and
// ownBarrierBuiltins the ones build.go leaves to their own barriers.
func TestProcSiteBuiltinsMatchParser(t *testing.T) {
	var want []string
	for _, n := range parser.Builtins() {
		if s, _ := parser.BuiltinSiteOf(n); s == parser.SiteProc {
			want = append(want, n)
		}
	}
	sort.Strings(want)
	if got := fctTextList(t, "check.fct", "procSiteBuiltins"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("procSiteBuiltins drifted from internal/parser/builtins.go:\n ported %v\n parser %v", got, want)
	}
	if got, want := fctTextList(t, "check.fct", "ownBarrierBuiltins"), ir.OwnBarrierBuiltins(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("ownBarrierBuiltins drifted from internal/ir/build.go:\n ported %v\n build.go %v", got, want)
	}
}
