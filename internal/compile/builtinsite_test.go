package compile

import (
	"strings"
	"testing"
)

// Where a builtin may run comes from one table (internal/parser/builtins.go):
// a client builtin works in a view and in a browser-placed action; a
// server-only one is refused in a view and pins an action to the authority; a
// proc-only one is refused anywhere but a proc.
func TestBuiltinSitesDecidePlacement(t *testing.T) {
	g, err := String(`app D:
    state s: text = "hello" @client
    state n: int = 0 @client
    state m: int = 0 @server
    action grow():
        s = s + "!" + slice(s, 0, 1) + charAt(s, 1) + join(split(s, "l"), "")
        n = byteLen(s) + toInt("7")
    action stamp():
        m = floatBits(1.5)
    action probe():
        let z = zoneValid("UTC")
        check z "no zone database"
    view Home at "/":
        text "{slice(s, 0, 3)}|{toInt("7") + 1}|{byteLen(s)}|{charAt(s, 1)}|{toFloat("1.5")}"
        button "grow" -> grow
        button "stamp" -> stamp
`)
	if err != nil {
		t.Fatalf("a view and a client action calling client builtins must compile: %v", err)
	}
	for _, a := range g.Actions {
		switch a.Name {
		case "grow":
			if a.Placement != "client" {
				t.Errorf("grow uses only client builtins on @client state, placed %s: %s", a.Placement, a.Reason)
			}
		case "probe":
			if a.Placement != "server" || !strings.Contains(a.Reason, "zoneValid") {
				t.Errorf("probe's only reason to run on the authority is zoneValid, placed %s: %s", a.Placement, a.Reason)
			}
		case "stamp":
			if a.Placement != "server" {
				t.Errorf("stamp calls a server-only builtin, placed %s: %s", a.Placement, a.Reason)
			}
		}
	}

	for _, tc := range []struct{ expr, want string }{
		{`floatBits(1.5)`, "runs only on the authority"},
		{`fromLocal("2026-01-01T00:00", "UTC")`, "runs only on the authority"},
		{`len(append([1], 2))`, "only available inside a proc body"},
		{`len(textToBytes(s))`, "only available inside a proc body"},
		{`bytesToText([104])`, "only available inside a proc body"},
		{`len(bytes(2))`, "only available inside a proc body"},
		{`monoMs()`, "only available inside a proc body"},
	} {
		src := "app D:\n    state s: text = \"hello\" @client\n    view Home at \"/\":\n        text \"{" + tc.expr + "}\"\n"
		_, err := String(src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s in a view: err = %v, want %q", tc.expr, err, tc.want)
		}
	}
}
