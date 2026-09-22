// Package selfhost holds work-in-progress `.fct` ports of pieces of fct's own
// Go compiler, per ROADMAP.md's "Decision superseded: full self-hosting".
// This file verifies source.fct's procs against the real, current Go
// implementation they are porting (internal/source.Parse) by compiling and
// running source.fct through the same in-memory server the rest of the
// project's runtime tests use, and comparing results line-for-line and
// tree-shape-for-tree-shape against internal/source's own output for the
// same inputs. It imports internal/source and runtime read-only — no .go
// file anywhere in the module is modified by this package.
package selfhost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/source"
	"facet/runtime"
)

func loadApp(t *testing.T) *httptest.Server {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("source.fct"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := compile.String(string(src))
	if err != nil {
		t.Fatalf("compile selfhost/source.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, ts *httptest.Server, action string, args ...any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/api/"+action, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s returned %d: %s", action, resp.StatusCode, b)
	}
	var out struct {
		OK     bool           `json:"ok"`
		Deltas map[string]any `json:"deltas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("%s did not report ok: %+v", action, out)
	}
	return out.Deltas
}

// goIndent mirrors internal/source/source.go's Parse loop exactly, isolated
// to just the one line's indent computation, so this test asks the identical
// question the real compiler stage asks: len(raw) - len(TrimLeft(raw, " ")).
func goIndent(raw string) int {
	return len(raw) - len(strings.TrimLeft(raw, " "))
}

// goHasLeadingTab mirrors source.go's own tab-rejection check verbatim (the
// `if strings.ContainsRune(raw, '\t') ...` block in Parse), isolated to
// reporting whether IT would reject this line, rather than reproducing its
// error text.
func goHasLeadingTab(raw string) bool {
	if !strings.ContainsRune(raw, '\t') || strings.TrimSpace(raw) == "" {
		return false
	}
	lead := raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]
	return strings.ContainsRune(lead, '\t')
}

func goIsBlankOrComment(raw string) bool {
	text := strings.TrimSpace(raw)
	return text == "" || strings.HasPrefix(text, "#")
}

func goLineText(raw string) string {
	return strings.TrimSpace(raw)
}

// realLines is pulled from real .fct source: LANGUAGE.md's own worked
// examples plus a hand-written line exercising leading tabs, so this isn't
// tested only against lines convenient to the .fct port.
var realLines = []string{
	`proc classify(x: int, limit: int) -> text:`,
	`        return "high"`,
	`    else:`,
	``,
	`   # a full-line comment`,
	`app A:`,
	`    proc sumTo(n: int) -> int:`,
	"\tindented with a raw tab",
	"  \tspaces then a tab",
	`            total = total + i     # trailing comment after code`,
}

// wireDeltas only ever carries state that actually CHANGED from its current
// value (runtime/server.go's own diff optimization) — so a call that leaves a
// cell at its zero value reports no key for it at all. These helpers fall
// back to the type's zero value exactly when the key is absent, which is the
// only way to read a proc's result reliably across arbitrary inputs
// (including ones whose real answer happens to equal the zero value) without
// spinning up a brand new server per single assertion.
func intOrZero(d map[string]any, key string) int {
	v, ok := d[key]
	if !ok {
		return 0
	}
	return int(v.(float64))
}

func boolOrFalse(d map[string]any, key string) bool {
	v, ok := d[key]
	if !ok {
		return false
	}
	return v.(bool)
}

func textOrEmpty(d map[string]any, key string) string {
	v, ok := d[key]
	if !ok {
		return ""
	}
	return v.(string)
}

// TestLinePrimitivesMatchGoSource is the per-line half of this port's
// verification: for each real source line, source.fct's indentOf/
// hasLeadingTab/isBlankOrComment/lineText procs, invoked live over HTTP, must
// agree exactly with internal/source/source.go's own equivalent computation
// for that same line. Each line gets a fresh server so one call's leftover
// state (a prior line's non-zero result) can never masquerade as this line's
// answer when this line's own true answer is the zero value.
func TestLinePrimitivesMatchGoSource(t *testing.T) {
	for _, line := range realLines {
		wantIndent := goIndent(line)
		wantTab := goHasLeadingTab(line)
		wantBlank := goIsBlankOrComment(line)
		wantText := goLineText(line)

		ts := loadApp(t)
		d := postJSON(t, ts, "runIndentOf", line)
		if got := intOrZero(d, "indentResult"); got != wantIndent {
			t.Errorf("indentOf(%q) = %d, want %d (Go's own len(raw)-len(TrimLeft(raw,\" \")))", line, got, wantIndent)
		}

		d = postJSON(t, ts, "runHasLeadingTab", line)
		if got := boolOrFalse(d, "tabResult"); got != wantTab {
			t.Errorf("hasLeadingTab(%q) = %v, want %v (Go's own lead-contains-tab check)", line, got, wantTab)
		}

		d = postJSON(t, ts, "runIsBlankOrComment", line)
		if got := boolOrFalse(d, "blankResult"); got != wantBlank {
			t.Errorf("isBlankOrComment(%q) = %v, want %v", line, got, wantBlank)
		}

		d = postJSON(t, ts, "runLineText", line)
		if got := textOrEmpty(d, "textResult"); got != wantText {
			t.Errorf("lineText(%q) = %q, want %q (Go's own strings.TrimSpace)", line, got, wantText)
		}
	}
}

// goParentIndex runs the REAL Go source.Parse on a synthetic multi-line
// source built from the given indents (one no-op statement per line, each
// indented by its slot's indent count) and reads the resulting forest back
// into the same "parent index per line" shape parentIndexForFlatIndents
// returns, by walking Node.Children. This is this test's use of the actual,
// current Go compiler package for comparison, per this task's instructions
// ("you may READ any .go file for reference" / call existing Go code for
// comparison) — internal/source is never modified.
func goParentIndex(t *testing.T, indents []int) []int {
	t.Helper()
	var b strings.Builder
	for _, ind := range indents {
		b.WriteString(strings.Repeat(" ", ind))
		b.WriteString("x\n")
	}
	roots, err := source.Parse(b.String())
	if err != nil {
		t.Fatalf("source.Parse: %v", err)
	}
	parents := make([]int, len(indents))
	var walk func(nodes []*source.Node, parent int)
	walk = func(nodes []*source.Node, parent int) {
		for _, n := range nodes {
			idx := n.Line.No - 1 // 1-based line numbers -> 0-based slot
			parents[idx] = parent
			walk(n.Children, idx)
		}
	}
	walk(roots, -1)
	return parents
}

// TestParentIndexMatchesGoBuild is the tree-nesting half of this port's
// verification: parentIndexForFlatIndents (the explicit-stack port of
// source.go's recursive `build`) must compute, for the same sequence of
// indents, exactly the same "nearest strictly-shallower-indent ancestor" for
// every line that the real Go `build` computes — including source.go's own
// documented ragged-sibling-indent quirk (siblings are never actually
// required to share the same indent, despite lines 88-92's comment).
func TestParentIndexMatchesGoBuild(t *testing.T) {
	cases := [][]int{
		{0, 4, 8, 4, 0}, // a clean two-level nest, back to root
		{0, 2, 4, 6, 2}, // a deep chain, then back up several levels
		{0, 4, 2, 4, 0}, // ragged siblings under one parent (the quirk)
		{0, 0, 0},       // flat siblings, no nesting at all
		{2, 4, 2, 4},    // starts indented (still must resolve sanely)
	}
	for _, indents := range cases {
		want := goParentIndex(t, indents)

		var slots [8]int
		for i := range slots {
			slots[i] = -1
		}
		for i, v := range indents {
			slots[i] = v
		}
		ts := loadApp(t)
		d := postJSON(t, ts, "runParentIndexForFlatIndents",
			slots[0], slots[1], slots[2], slots[3], slots[4], slots[5], slots[6], slots[7], len(indents))
		raw, ok := d["parentsResult"].([]any)
		if !ok {
			if len(want) == 0 {
				continue
			}
			t.Fatalf("indents=%v: parentsResult = %#v, want a list", indents, d["parentsResult"])
		}
		got := make([]int, len(raw))
		for i, v := range raw {
			got[i] = int(v.(float64))
		}
		if len(got) != len(want) {
			t.Fatalf("indents=%v: got %v, want %v (length mismatch)", indents, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("indents=%v: parent[%d] = %d, want %d (real Go build())", indents, i, got[i], want[i])
			}
		}
	}
}

// goCyclicIndents mirrors source.fct's buildCyclicIndents proc exactly
// (`(i % 3) * 2` for i in [0,n)), so this test's expectations are computed
// the same way source.fct itself computes its own input, then fed through
// the real Go source.Parse for the actual cross-check.
func goCyclicIndents(n int) []int {
	out := make([]int, n)
	for i := 0; i < n; i++ {
		out[i] = (i % 3) * 2
	}
	return out
}

// TestParentIndexForIndentsMatchesGoBuild is the proof that this task's
// language fix (a proc parameter may now be list-typed) genuinely improves on
// source.fct's original parentIndexForFlatIndents workaround, not just adds a
// second, differently-shaped proc: parentIndexForIndents takes a real `[int]`
// parameter, and buildCyclicIndents (also a proc) produces that list and
// hands it over via `do parentIndexForIndents(indents)` — proc-to-proc,
// through a list value, entirely server-side. n intentionally ranges past the
// old fixed-8-slot ceiling (up to 20), which parentIndexForFlatIndents could
// never accept at all (it silently ignores any indent past its 8th slot),
// proving there is no length ceiling left in the fixed version's replacement.
// ── Full pipeline verification (raw multi-line text -> parent-index array) ──
//
// Everything below exercises source.fct's NEW stage-1 procs (parsedIndents/
// parsedTexts/parsedSkipFlags/parsedTabError) and the composed full-pipeline
// proc (parseSourceToParents), cross-checked against the real, unmodified
// internal/source.Parse — not a second reimplementation of its nesting
// algorithm. flattenPreOrder below is pure bookkeeping (a tree walk that
// records order and parent links), not a restatement of the offside-rule
// logic itself.

// flattenPreOrder walks a real Go source.Parse forest in exactly the order
// source.go's own `build` produced it: a node's own line immediately
// followed (recursively) by its children, before its next sibling. Because
// `build` consumes `lines` strictly left to right, that pre-order IS the
// original document order of every kept (non-blank, non-comment) line — the
// same order source.fct's parsedIndents/parsedTexts/parseSourceToParents
// procs build their own parallel arrays in, since they too walk raw lines
// left to right and simply skip (never reorder) the ones source.go drops.
// parentSeq is the caller's own 0-based sequential position among kept
// lines (or -1 for a root); each visited node's indent/text/parent are
// appended in that same document order.
func flattenPreOrder(nodes []*source.Node, parentSeq int, indents *[]int, texts *[]string, parents *[]int) {
	for _, n := range nodes {
		*indents = append(*indents, n.Line.Indent)
		*texts = append(*texts, n.Line.Text)
		*parents = append(*parents, parentSeq)
		mySeq := len(*parents) - 1
		flattenPreOrder(n.Children, mySeq, indents, texts, parents)
	}
}

// goFullPipeline runs ONLY the real, unmodified internal/source.Parse (never
// a hand-rolled reimplementation of its nesting) and reduces its result to
// the same three parallel-array shape source.fct's own stage-1 procs
// produce: indents, trimmed texts, and nearest-strictly-shallower-ancestor
// parent indices, all indexed by sequential position among kept lines. A
// non-nil error (the leading-tab rejection) is reported as isErr=true with
// no other return value meaningful.
func goFullPipeline(t *testing.T, src string) (indents []int, texts []string, parents []int, isErr bool) {
	t.Helper()
	roots, err := source.Parse(src)
	if err != nil {
		return nil, nil, nil, true
	}
	flattenPreOrder(roots, -1, &indents, &texts, &parents)
	return indents, texts, parents, false
}

func intSliceFromAny(t *testing.T, v any) []int {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a []any for an int list, got %T (%v)", v, v)
	}
	out := make([]int, len(arr))
	for i, e := range arr {
		f, ok := e.(float64)
		if !ok {
			t.Fatalf("element %d is %T (%v), want a number", i, e, e)
		}
		out[i] = int(f)
	}
	return out
}

func boolSliceFromAny(t *testing.T, v any) []bool {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a []any for a bool list, got %T (%v)", v, v)
	}
	out := make([]bool, len(arr))
	for i, e := range arr {
		b, ok := e.(bool)
		if !ok {
			t.Fatalf("element %d is %T (%v), want a bool", i, e, e)
		}
		out[i] = b
	}
	return out
}

func strSliceFromAny(t *testing.T, v any) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a []any for a text list, got %T (%v)", v, v)
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("element %d is %T (%v), want a string", i, e, e)
		}
		out[i] = s
	}
	return out
}

// realSources are genuine, varied multi-line `.fct`-shaped inputs: a small
// nested app with a blank line (adapted from LANGUAGE.md's own worked
// examples), an app with full-line comments interspersed among real
// indentation levels, and a deliberately ragged/deep indentation structure
// with no semantic validity requirement at all (source.go's Parse — and
// therefore this port — only ever looks at whitespace and text, never at
// what the text means; the original source_test.go's own goParentIndex
// helper already relies on this by feeding it bare "x" lines).
var realSources = map[string]string{
	"small nested app with a blank line":                 "app A:\n    state count: int = 0\n\n    view Home at \"/\":\n        text \"{count}\"\n",
	"app with comments interspersed at multiple indents": "app A:\n    # a top comment\n    proc classify(x: int, limit: int) -> text:\n        if x > limit:\n            return \"high\"\n        else:\n            return \"low\"\n\n    # another comment\n    state result: text = \"\"\n",
	"ragged/deep indentation, no fixed sibling level":    "root1\n    childA\n        grandchild\n    childB\nsiblingRoot\n        deepButInconsistent\n    shallowerButStillNested\n",
}

// TestParsedStageMatchesGoParse verifies stage 1 alone (source.fct's new
// parsedIndents/parsedTexts/parsedSkipFlags procs) against the real Go
// source.Parse, for genuine multi-line, blank-line-and-comment-bearing
// `.fct`-shaped input — not just the tree-nesting half the original port
// already covered.
func TestParsedStageMatchesGoParse(t *testing.T) {
	for name, src := range realSources {
		t.Run(name, func(t *testing.T) {
			wantIndents, wantTexts, _, isErr := goFullPipeline(t, src)
			if isErr {
				t.Fatalf("goFullPipeline(%q) unexpectedly errored", src)
			}

			ts := loadApp(t)
			d := postJSON(t, ts, "runParsedIndents", src)
			gotIndents := intSliceFromAny(t, d["indentsResult"])
			if len(gotIndents) != len(wantIndents) {
				t.Fatalf("parsedIndents length = %d, want %d (%v vs %v)", len(gotIndents), len(wantIndents), gotIndents, wantIndents)
			}
			for i := range wantIndents {
				if gotIndents[i] != wantIndents[i] {
					t.Errorf("parsedIndents[%d] = %d, want %d (real Go source.Parse)", i, gotIndents[i], wantIndents[i])
				}
			}

			d = postJSON(t, ts, "runParsedTexts", src)
			gotTexts := strSliceFromAny(t, d["textsResult"])
			if len(gotTexts) != len(wantTexts) {
				t.Fatalf("parsedTexts length = %d, want %d (%v vs %v)", len(gotTexts), len(wantTexts), gotTexts, wantTexts)
			}
			for i := range wantTexts {
				if gotTexts[i] != wantTexts[i] {
					t.Errorf("parsedTexts[%d] = %q, want %q (real Go source.Parse)", i, gotTexts[i], wantTexts[i])
				}
			}

			// parsedSkipFlags covers EVERY raw line (not just kept ones), so
			// cross-check it against source.go's own inline blank/comment
			// test applied per raw line, independent of Parse's tree output.
			d = postJSON(t, ts, "runParsedSkipFlags", src)
			gotSkip := boolSliceFromAny(t, d["skipFlagsResult"])
			rawLines := strings.Split(src, "\n")
			if len(gotSkip) != len(rawLines) {
				t.Fatalf("parsedSkipFlags length = %d, want %d (one per raw line)", len(gotSkip), len(rawLines))
			}
			for i, raw := range rawLines {
				want := goIsBlankOrComment(raw)
				if gotSkip[i] != want {
					t.Errorf("parsedSkipFlags[%d] (raw %q) = %v, want %v", i, raw, gotSkip[i], want)
				}
			}
		})
	}
}

// TestParseSourceToParentsFullPipelineMatchesGoParse is the task's headline
// verification: raw multi-line source text in, a parent-index array out,
// checked end to end against the real Go source.Parse (not just its
// tree-nesting half in isolation) — for a clean nested app, an app with
// comments at multiple indent levels, and a genuinely ragged/deep
// indentation structure.
func TestParseSourceToParentsFullPipelineMatchesGoParse(t *testing.T) {
	for name, src := range realSources {
		t.Run(name, func(t *testing.T) {
			_, _, wantParents, isErr := goFullPipeline(t, src)
			if isErr {
				t.Fatalf("goFullPipeline(%q) unexpectedly errored", src)
			}

			ts := loadApp(t)
			d := postJSON(t, ts, "runParseSourceToParents", src)
			if got := boolOrFalse(d, "tabErrorResult"); got {
				t.Fatalf("tabErrorResult = true for a tab-free source, want false")
			}
			raw, ok := d["parentsResult3"].([]any)
			if !ok {
				if len(wantParents) == 0 {
					return
				}
				t.Fatalf("parentsResult3 = %#v, want a list of length %d", d["parentsResult3"], len(wantParents))
			}
			got := intSliceFromAny(t, raw)
			if len(got) != len(wantParents) {
				t.Fatalf("parentsResult3 length = %d, want %d (%v vs %v, real Go source.Parse)", len(got), len(wantParents), got, wantParents)
			}
			for i := range wantParents {
				if got[i] != wantParents[i] {
					t.Errorf("parent[%d] = %d, want %d (real Go source.Parse, full pipeline)", i, got[i], wantParents[i])
				}
			}
		})
	}
}

// TestParseSourceToParentsRejectsLeadingTabs proves the full pipeline's error
// path: a genuine leading tab (source.go's own rejection case) must make
// parsedTabError/parseSourceToParents agree with the real Go source.Parse
// that this input is invalid — and, separately, that the `[-2]` sentinel is
// exactly what a caller relying on the single-call form sees.
func TestParseSourceToParentsRejectsLeadingTabs(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"a genuinely tab-indented line",
			"app A:\n    state x: int = 0\n\tbadline\n    view Home at \"/\":\n        text \"{x}\"\n",
		},
		{
			"a full-line comment whose OWN leading whitespace is a tab (source.go still rejects this — the tab check runs before the comment-skip check)",
			"app A:\n    state x: int = 0\n\t# a comment with a leading tab\n    view Home at \"/\":\n        text \"{x}\"\n",
		},
		{
			"mixed spaces-then-tab leading whitespace",
			"app A:\n  \tstate x: int = 0\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, isErr := goFullPipeline(t, c.src)
			if !isErr {
				t.Fatalf("goFullPipeline(%q) did not error, want the real Go source.Parse to reject this leading tab", c.src)
			}

			ts := loadApp(t)
			d := postJSON(t, ts, "runParseSourceToParents", c.src)
			if got := boolOrFalse(d, "tabErrorResult"); !got {
				t.Errorf("tabErrorResult = false, want true (real Go source.Parse rejects this input)")
			}
			raw, ok := d["parentsResult3"].([]any)
			if !ok {
				t.Fatalf("parentsResult3 = %#v, want the [-2] error sentinel", d["parentsResult3"])
			}
			got := intSliceFromAny(t, raw)
			if len(got) != 1 || got[0] != -2 {
				t.Errorf("parentsResult3 = %v, want the single-element error sentinel [-2]", got)
			}
		})
	}
}

// TestParseSourceToParentsAllTabBlankLineIsNotAnError is the precise
// semantic detail this task calls out by name: a line made ENTIRELY of
// whitespace — even if that whitespace is tabs — is not a leading-tab
// rejection in the real Go source.go, because its outer gate is
// `strings.TrimSpace(raw) != ""`, which is false for such a line. A version
// of parsedTabError that forgot this guard (treating "the leading run
// contains a tab" as sufficient on its own) would reject strictly MORE
// inputs than the real compiler does. This is checked against the real Go
// source.Parse, which must NOT error on this input.
func TestParseSourceToParentsAllTabBlankLineIsNotAnError(t *testing.T) {
	src := "app A:\n    state x: int = 0\n\t\t\n    view Home at \"/\":\n        text \"{x}\"\n"

	wantIndents, wantTexts, wantParents, isErr := goFullPipeline(t, src)
	if isErr {
		t.Fatalf("goFullPipeline(%q) errored, want the real Go source.Parse to accept an all-tab BLANK line (TrimSpace(raw) == \"\" bypasses the tab check entirely)", src)
	}

	ts := loadApp(t)
	d := postJSON(t, ts, "runParseSourceToParents", src)
	if got := boolOrFalse(d, "tabErrorResult"); got {
		t.Fatalf("tabErrorResult = true for an all-tab BLANK line, want false (matching real Go source.Parse's TrimSpace(raw) != \"\" gate)")
	}
	raw, ok := d["parentsResult3"].([]any)
	if !ok {
		t.Fatalf("parentsResult3 = %#v, want a real parent list (not the error sentinel)", d["parentsResult3"])
	}
	got := intSliceFromAny(t, raw)
	if len(got) != len(wantParents) {
		t.Fatalf("parentsResult3 length = %d, want %d (%v vs %v)", len(got), len(wantParents), got, wantParents)
	}
	for i := range wantParents {
		if got[i] != wantParents[i] {
			t.Errorf("parent[%d] = %d, want %d", i, got[i], wantParents[i])
		}
	}

	d = postJSON(t, ts, "runParsedIndents", src)
	gotIndents := intSliceFromAny(t, d["indentsResult"])
	if len(gotIndents) != len(wantIndents) {
		t.Fatalf("parsedIndents length = %d, want %d", len(gotIndents), len(wantIndents))
	}
	for i := range wantIndents {
		if gotIndents[i] != wantIndents[i] {
			t.Errorf("parsedIndents[%d] = %d, want %d", i, gotIndents[i], wantIndents[i])
		}
	}

	d = postJSON(t, ts, "runParsedTexts", src)
	gotTexts := strSliceFromAny(t, d["textsResult"])
	if len(gotTexts) != len(wantTexts) {
		t.Fatalf("parsedTexts length = %d, want %d", len(gotTexts), len(wantTexts))
	}
	for i := range wantTexts {
		if gotTexts[i] != wantTexts[i] {
			t.Errorf("parsedTexts[%d] = %q, want %q", i, gotTexts[i], wantTexts[i])
		}
	}
}

func TestParentIndexForIndentsMatchesGoBuild(t *testing.T) {
	for _, n := range []int{0, 1, 5, 8, 9, 12, 20} {
		indents := goCyclicIndents(n)
		want := goParentIndex(t, indents)

		ts := loadApp(t)
		d := postJSON(t, ts, "runParentIndexForIndents", n)
		raw, ok := d["parentsResult2"].([]any)
		if !ok {
			if len(want) == 0 {
				continue
			}
			t.Fatalf("n=%d: parentsResult2 = %#v, want a list of length %d", n, d["parentsResult2"], len(want))
		}
		got := make([]int, len(raw))
		for i, v := range raw {
			got[i] = int(v.(float64))
		}
		if len(got) != len(want) {
			t.Fatalf("n=%d: got %v, want %v (length mismatch — a real [int] parameter must carry every element, not just the first 8)", n, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("n=%d indents=%v: parent[%d] = %d, want %d (real Go build())", n, indents, i, got[i], want[i])
			}
		}
	}
}

// ── STAGE 3 verification: a genuinely tree-shaped, recursive walk ──
//
// These tests prove source.fct's childrenOf/subtreeSum/subtreeDepth/
// subtreeCount/forestSum/forestMaxDepth/forestNodeCount procs (Option B's
// answer to "a proc cannot construct or return a genuine struct/tree-shaped
// value" — see source.fct's own STAGE 3 doc for the full design rationale)
// really do walk the actual nesting structure a parents array encodes,
// recursively, rather than just re-deriving something a flat pass over the
// same array could already answer.

// goForestAggregates walks the REAL, unmodified internal/source.Parse forest
// (never a reimplementation) for src, computing — by ordinary recursive Go
// functions over the genuine Node/Children pointer tree — the same three
// aggregates source.fct's own STAGE 3 procs compute over their parents-array
// encoding of the identical tree: the sum of every node's own indent (its
// "value"), the longest root-to-leaf path length in nodes ("depth"), and the
// total node count. This is this file's established pattern (cross-check
// against the real Go compiler, not a second hand-rolled reimplementation of
// its own logic) applied to genuinely tree-shaped (not just per-line or
// nesting-flattened) properties.
func goForestAggregates(t *testing.T, src string) (sum, maxDepth, nodeCount int) {
	t.Helper()
	roots, err := source.Parse(src)
	if err != nil {
		t.Fatalf("source.Parse(%q): %v", src, err)
	}
	var walk func(n *source.Node) (s, depth, count int)
	walk = func(n *source.Node) (s, depth, count int) {
		s = n.Line.Indent
		count = 1
		for _, ch := range n.Children {
			cs, cd, cc := walk(ch)
			s += cs
			if cd > depth {
				depth = cd
			}
			count += cc
		}
		return s, depth + 1, count
	}
	for _, r := range roots {
		s, depth, count := walk(r)
		sum += s
		if depth > maxDepth {
			maxDepth = depth
		}
		nodeCount += count
	}
	return sum, maxDepth, nodeCount
}

// TestForestWalkMatchesHandCalculatedTree is the task's headline proof: a
// specific, genuinely tree-shaped (depth 3, two roots, a branching node)
// forest, with every aggregate worked out BY HAND below and cross-checked
// against the real Go source.Parse independently, then checked live over
// HTTP against source.fct's own recursive procs.
//
// The forest (indent shown per line):
//
//	root1 (0)                  root2 (0)
//	 +-- childA (4)             +-- childC (4)
//	 |    +-- gc1 (8)
//	 |    +-- gc2 (8)
//	 +-- childB (4)
//
// parents (nearest strictly-shallower-preceding line, 0-based, -1 = root):
//
//	index:   0      1       2     3     4       5      6
//	line:    root1  childA  gc1   gc2   childB  root2  childC
//	parent: [-1,    0,      1,    1,    0,      -1,    5]
//
// values (each kept line's own indent — the per-node number forestSum adds):
//
//	[0, 4, 8, 8, 4, 0, 4]
//
// Hand-computed postorder aggregates:
//
//	subtreeSum(gc1)=8, subtreeSum(gc2)=8
//	subtreeSum(childA) = 4 + 8 + 8   = 20
//	subtreeSum(childB) = 4
//	subtreeSum(root1)  = 0 + 20 + 4  = 24
//	subtreeSum(childC) = 4
//	subtreeSum(root2)  = 0 + 4       = 4
//	forestSum          = 24 + 4      = 28
//
//	depth(gc1)=depth(gc2)=1, depth(childA)=1+max(1,1)=2, depth(childB)=1
//	depth(root1) = 1 + max(2, 1) = 3
//	depth(childC)=1, depth(root2) = 1 + 1 = 2
//	forestMaxDepth = max(3, 2) = 3   <- depth > 2: a genuinely nested case,
//	                                    not just a flat list of siblings
//
//	count(gc1)=count(gc2)=1, count(childA)=1+1+1=3, count(childB)=1
//	count(root1) = 1 + 3 + 1 = 5
//	count(childC)=1, count(root2) = 1 + 1 = 2
//	forestNodeCount = 5 + 2 = 7      (== the 7 kept lines above, exactly once each)
func TestForestWalkMatchesHandCalculatedTree(t *testing.T) {
	src := "root1\n    childA\n        gc1\n        gc2\n    childB\nroot2\n    childC\n"

	// Sanity-check the hand arithmetic above against the real Go source.Parse
	// tree first, independent of source.fct entirely — if these don't agree,
	// the comment's arithmetic (not source.fct) is wrong.
	wantSum, wantDepth, wantCount := goForestAggregates(t, src)
	if wantSum != 28 || wantDepth != 3 || wantCount != 7 {
		t.Fatalf("this test's own hand-calculation is wrong: real Go source.Parse gives sum=%d depth=%d count=%d, want 28/3/7", wantSum, wantDepth, wantCount)
	}

	ts := loadApp(t)

	d := postJSON(t, ts, "runForestSum", src)
	if got := intOrZero(d, "forestSumResult"); got != 28 {
		t.Errorf("forestSum(handTree) = %d, want 28 (hand-calculated: values 0,4,8,8,4,0,4 summed over the whole forest)", got)
	}

	d = postJSON(t, ts, "runForestMaxDepth", src)
	if got := intOrZero(d, "forestMaxDepthResult"); got != 3 {
		t.Errorf("forestMaxDepth(handTree) = %d, want 3 (root1 -> childA -> gc1/gc2 is the deepest chain, depth > 2)", got)
	}

	d = postJSON(t, ts, "runForestNodeCount", src)
	if got := intOrZero(d, "forestNodeCountResult"); got != 7 {
		t.Errorf("forestNodeCount(handTree) = %d, want 7 (every one of the 7 kept lines counted exactly once)", got)
	}
}

// TestForestWalkHandlesDeepChain proves the recursion actually walks all the
// way down a genuinely deep structure (5 levels, no branching at all) rather
// than happening to be correct only for the shallow hand-worked example
// above — depth and count must track the chain's true length, not plateau
// after the first couple of `do` self-calls.
func TestForestWalkHandlesDeepChain(t *testing.T) {
	src := "a\n    b\n        c\n            d\n                e\n"
	ts := loadApp(t)

	d := postJSON(t, ts, "runForestMaxDepth", src)
	if got := intOrZero(d, "forestMaxDepthResult"); got != 5 {
		t.Errorf("forestMaxDepth(chain of 5) = %d, want 5", got)
	}
	d = postJSON(t, ts, "runForestNodeCount", src)
	if got := intOrZero(d, "forestNodeCountResult"); got != 5 {
		t.Errorf("forestNodeCount(chain of 5) = %d, want 5", got)
	}
	d = postJSON(t, ts, "runForestSum", src)
	if got := intOrZero(d, "forestSumResult"); got != 40 {
		t.Errorf("forestSum(chain of 5) = %d, want 40 (indents 0+4+8+12+16)", got)
	}
}

// TestForestWalkMatchesGoParseRecursion generalizes the proof across this
// file's existing realSources fixtures (a small nested app, an app with
// interspersed comments, and a ragged/deep structure) — every one already
// used to verify the flat parents/indents/texts arrays above, now also
// checked for the genuinely recursive aggregates, cross-checked against the
// real Go source.Parse tree (goForestAggregates), not a second
// reimplementation of source.fct's own logic. It also checks the invariant
// that forestNodeCount always equals the number of kept lines, exactly once
// each — true only if every node in the forest was visited by the recursion,
// no more and no fewer.
func TestForestWalkMatchesGoParseRecursion(t *testing.T) {
	for name, src := range realSources {
		t.Run(name, func(t *testing.T) {
			wantSum, wantDepth, wantCount := goForestAggregates(t, src)
			wantIndents, _, _, isErr := goFullPipeline(t, src)
			if isErr {
				t.Fatalf("goFullPipeline(%q) unexpectedly errored", src)
			}

			ts := loadApp(t)

			d := postJSON(t, ts, "runForestSum", src)
			if got := intOrZero(d, "forestSumResult"); got != wantSum {
				t.Errorf("forestSum = %d, want %d (real Go source.Parse tree, indent-value sum)", got, wantSum)
			}

			d = postJSON(t, ts, "runForestMaxDepth", src)
			if got := intOrZero(d, "forestMaxDepthResult"); got != wantDepth {
				t.Errorf("forestMaxDepth = %d, want %d (real Go source.Parse tree)", got, wantDepth)
			}

			d = postJSON(t, ts, "runForestNodeCount", src)
			got := intOrZero(d, "forestNodeCountResult")
			if got != wantCount {
				t.Errorf("forestNodeCount = %d, want %d (real Go source.Parse tree)", got, wantCount)
			}
			if got != len(wantIndents) {
				t.Errorf("forestNodeCount = %d, want %d (one per kept line — every node visited exactly once)", got, len(wantIndents))
			}
		})
	}
}
