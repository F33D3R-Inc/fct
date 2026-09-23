package selfhost

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

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
