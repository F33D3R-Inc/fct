package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"facet/internal/ir"
)

// A fixed "now" so `ago` is deterministic: 2026-09-05 12:00:00 UTC.
const fixedNow = 1788609600

var agoCases = []struct {
	ts   int
	want string
}{
	{fixedNow - 5, "now"},
	{fixedNow - 59, "now"},
	{fixedNow - 60, "1m"},
	{fixedNow - 5*60 - 30, "5m"},
	{fixedNow - 3600, "1h"},
	{fixedNow - 20*3600 - 1, "20h"},
	{fixedNow - 86400, "Sep 4"},
	{fixedNow - 94*86400, "Jun 3"},
	{fixedNow - 400*86400, "Aug 1, 2025"},
}

var compactCases = []struct {
	n    int
	want string
}{
	{0, "0"}, {7, "7"}, {999, "999"},
	{1000, "1K"}, {1300, "1.3K"}, {1999, "1.9K"}, {79812, "79.8K"}, {604000, "604K"}, {999999, "999.9K"},
	{1000000, "1M"}, {240100000, "240.1M"}, {2000000000, "2B"},
	{-1300, "-1.3K"},
}

var commasCases = []struct {
	n    int
	want string
}{
	{0, "0"}, {999, "999"}, {1000, "1,000"}, {1352, "1,352"}, {240100000, "240,100,000"}, {-1234567, "-1,234,567"},
}

func TestFormattingBuiltins(t *testing.T) {
	for _, c := range agoCases {
		if got := ago(c.ts, fixedNow); got != c.want {
			t.Errorf("ago(%d) = %q, want %q", c.ts, got, c.want)
		}
	}
	for _, c := range compactCases {
		if got := compact(c.n); got != c.want {
			t.Errorf("compact(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	for _, c := range commasCases {
		if got := commas(c.n); got != c.want {
			t.Errorf("commas(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	// Through eval, with the clock pinned.
	prev := clock
	clock = func() time.Time { return time.Unix(fixedNow, 0) }
	defer func() { clock = prev }()
	call := func(name string, args ...any) any {
		e := &ir.Expr{Kind: "call", Name: name}
		for _, a := range args {
			e.Args = append(e.Args, &ir.Expr{Kind: "lit", Val: a})
		}
		return eval(e, map[string]any{})
	}
	if got := call("ago", fixedNow-20*3600); got != "20h" {
		t.Errorf("ago via eval = %v", got)
	}
	if got := call("take", "héllo wörld", 5); got != "héllo" {
		t.Errorf("take counts runes, got %v", got)
	}
	if got := call("take", "abc", 10); got != "abc" {
		t.Errorf("take past the end is the whole string, got %v", got)
	}
	if got := call("take", "abc", -2); got != "" {
		t.Errorf("take of a negative count is empty, got %v", got)
	}
}

// The client renders these too, and a count that differs after hydration is a
// page that changes under the reader. Run the shipped copies over the same cases.
func TestClientFormattingMatchesTheServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}
	src := string(raw)
	var script strings.Builder
	for _, fn := range []string{"ago", "compact", "commas", "toInt", "toStr", "evCall"} {
		script.WriteString(extractFunction(t, src, fn) + "\n")
	}
	type in struct {
		Ago     []int    `json:"ago"`
		Compact []int    `json:"compact"`
		Commas  []int    `json:"commas"`
		Take    [][2]any `json:"take"`
	}
	var ins in
	for _, c := range agoCases {
		ins.Ago = append(ins.Ago, c.ts)
	}
	for _, c := range compactCases {
		ins.Compact = append(ins.Compact, c.n)
	}
	for _, c := range commasCases {
		ins.Commas = append(ins.Commas, c.n)
	}
	ins.Take = [][2]any{{"héllo wörld", 5}, {"abc", 10}, {"abc", -2}}
	b, _ := json.Marshal(ins)
	script.WriteString("const ev = (e) => e.val;\n") // evCall evaluates its arguments through ev; literals here
	script.WriteString("const NOW = 1788609600;\n")
	script.WriteString("const ins = " + string(b) + ";\n")
	script.WriteString(`console.log(JSON.stringify({
  ago: ins.ago.map((ts) => ago(ts, NOW)),
  compact: ins.compact.map(compact),
  commas: ins.commas.map(commas),
  take: ins.take.map((p) => evCall({ name: "take", args: [{ val: p[0] }, { val: p[1] }] }, {})),
}));`)
	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("running the client mirror: %v\n%s", err, out)
	}
	var got struct {
		Ago, Compact, Commas, Take []string
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	for i, c := range agoCases {
		if got.Ago[i] != c.want {
			t.Errorf("ago(%d): client %q, server %q", c.ts, got.Ago[i], c.want)
		}
	}
	for i, c := range compactCases {
		if got.Compact[i] != c.want {
			t.Errorf("compact(%d): client %q, server %q", c.n, got.Compact[i], c.want)
		}
	}
	for i, c := range commasCases {
		if got.Commas[i] != c.want {
			t.Errorf("commas(%d): client %q, server %q", c.n, got.Commas[i], c.want)
		}
	}
	for i, want := range []string{"héllo", "abc", ""} {
		if got.Take[i] != want {
			t.Errorf("take case %d: client %q, server %q", i, got.Take[i], want)
		}
	}
}

// `Post(id)` with no field evaluates to the row itself, on the server as on the
// client (the client's copy is one branch in ev's "eget" case).
func TestBareLookupEvaluatesToTheRow(t *testing.T) {
	scope := map[string]any{"Post": []any{record{"id": 1, "body": "a"}, record{"id": 2, "body": "b"}}}
	row := &ir.Expr{Kind: "eget", Name: "Post", Key: &ir.Expr{Kind: "lit", Val: 2}}
	got, ok := eval(row, scope).(record)
	if !ok || got["body"] != "b" {
		t.Fatalf("Post(2) should be row 2, got %#v", eval(row, scope))
	}
	field := &ir.Expr{Kind: "eget", Name: "Post", Key: &ir.Expr{Kind: "lit", Val: 2}, Field: "body"}
	if eval(field, scope) != "b" {
		t.Errorf("Post(2).body should still be the field")
	}
	if eval(&ir.Expr{Kind: "eget", Name: "Post", Key: &ir.Expr{Kind: "lit", Val: 9}}, scope) != nil {
		t.Errorf("a missing row is nil")
	}
}
