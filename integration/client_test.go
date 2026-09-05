package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// renderClient executes the shipped client against a page the server actually
// sent, under a minimal DOM, and returns how many nodes it produced.
//
// Nothing else in this repo runs facet.js. Its Go counterpart is unit-tested and
// its own logic is not, so a fault in it is invisible until someone opens a
// browser — which is exactly how a blank page shipped: `mount()` cleared the root
// and then threw on a component whose `params` arrived as JSON null, so the
// server-rendered page was destroyed and replaced with nothing. Every Go test
// passed. The failure needs a JavaScript engine to see, so this uses one.
func renderClient(t *testing.T, html string) int {
	nodes, _ := renderClientText(t, html)
	return nodes
}

// renderClientText also returns the whole rendered text, whitespace removed.
//
// The node count alone is too weak an assertion, and the gap is specific: the
// server and the client each walk the page IR to address a region's rows and a
// render's aggregate values, and if their walks disagree by one position the
// client renders a count as `0` — a node, not a blank page. "Not blank" passes.
// Comparing the text catches it, because the server put the real number in the
// HTML it sent and the client would have replaced it with a wrong one.
func renderClientText(t *testing.T, html string) (int, string) {
	nodes, text, _ := renderClientPage(t, html)
	return nodes, text
}

// renderClientPage also returns every anchor destination the client produced.
// Text cannot tell a link from inert text with the same label, and that is the
// difference the runtime contract for a computed destination turns on.
// clientAttr is one value the client put somewhere that is not body text: an
// element attribute, an input's placeholder property, or the text of a control
// whose label is an IR attribute (an option, a submit button, an upload, a link).
type clientAttr struct {
	Tag   string
	Name  string
	Value string
}

// renderClientAttrs also returns those, so a test can ask whether the client put
// the same characters in the same attributes the server's escaped HTML did.
func renderClientAttrs(t *testing.T, html string) []clientAttr {
	t.Helper()

	_, _, _, attrs := renderClientFull(t, html)

	return attrs
}

func renderClientPage(t *testing.T, html string) (int, string, []string) {
	nodes, text, hrefs, _ := renderClientFull(t, html)
	return nodes, text, hrefs
}

func renderClientFull(t *testing.T, html string) (int, string, []string, []clientAttr) {
	t.Helper()

	run, _ := runClient(t, html, nil)

	return run.Nodes, run.Text, run.Hrefs, run.Attrs
}

// clientControl is one two-way control the client rendered, in the state a
// browser would act on: which cell it writes, and — for a checkbox or a radio —
// whether it is ticked. `checked` and a control's current `value` are DOM
// properties rather than attributes, so clientAttr cannot carry them and a test
// written over attributes alone cannot tell a ticked box from an empty one.
type clientControl struct {
	Tag     string `json:"tag"`
	Bind    string `json:"bind"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	Checked bool   `json:"checked"`
	Role    string `json:"role"`
	Cls     string `json:"cls"`
	// Autocomplete is what a password manager reads. It is the whole difference
	// between `password` and `newpassword`, so a struct that omitted it would
	// compare two password boxes as identical while one of them told the manager
	// to fill the account's existing secret into a field for a new one.
	Autocomplete string `json:"autocomplete"`
	// Options is a select's whole choice list as one comparable string —
	// `value=label` per choice, a `*` on the selected one, joined by `|`. A
	// select's options are children whose `value`/`selected` are DOM properties,
	// so a control whose choices come from data is otherwise invisible to this
	// struct: the two sides could offer completely different lists and every
	// field above would still match.
	Options string `json:"options"`
}

// clientRun is everything the shim observed about the page at one moment.
type clientRun struct {
	Nodes    int
	Text     string
	Hrefs    []string
	Attrs    []clientAttr
	Controls []clientControl
}

// driveStep is one interaction to perform after boot: click the element the
// selector names, or type a value into it.
type driveStep struct {
	Sel   string `json:"sel"`
	Do    string `json:"do,omitempty"`
	Value string `json:"value,omitempty"`
}

// runClient boots the shipped client over a page the server actually sent and,
// when steps are given, drives them — returning what the page looked like after
// first paint and again after the interaction.
//
// The second half is what a control needs proving. Rendering a checkbox is the
// easy half; the half that was impossible before controls existed is the actor
// writing a `@client` cell and everything bound to that cell re-rendering, and
// no amount of looking at first paint can see it happen.
func runClient(t *testing.T, html string, steps []driveStep) (clientRun, clientRun) {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot exercise the client")
	}

	dir := t.TempDir()
	page := filepath.Join(dir, "page.html")
	if err := os.WriteFile(page, []byte(html), 0o600); err != nil {
		t.Fatalf("writing the page: %v", err)
	}

	client, err := filepath.Abs("../runtime/assets/facet.js")
	if err != nil {
		t.Fatalf("locating the client: %v", err)
	}

	args := []string{"testdata/domshim.js", page, client}
	if len(steps) > 0 {
		script, err := json.Marshal(steps)
		if err != nil {
			t.Fatalf("encoding the interaction script: %v", err)
		}
		args = append(args, string(script))
	}

	out, err := exec.Command(node, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("the client failed to render the page the server sent:\n%s", out)
	}

	first := parseClientRun(t, string(out), "root nodes: ", "all text: ", "hrefs: ", "attrs: ", "controls: ")
	if first.Nodes < 0 {
		t.Fatalf("the client produced no node count:\n%s", out)
	}
	if len(steps) == 0 {
		return first, clientRun{Nodes: -1}
	}

	after := parseClientRun(t, string(out), "after nodes: ", "after all text: ", "after hrefs: ", "after attrs: ", "after controls: ")
	if after.Nodes < 0 {
		t.Fatalf("the interaction script did not run:\n%s", out)
	}

	return first, after
}

func parseClientRun(t *testing.T, out, nodesK, textK, hrefsK, attrsK, ctlK string) clientRun {
	t.Helper()

	run := clientRun{Nodes: -1}

	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, nodesK); ok {
			n, convErr := strconv.Atoi(strings.TrimSpace(rest))
			if convErr != nil {
				t.Fatalf("unreadable node count %q", rest)
			}
			run.Nodes = n
		}
		if rest, ok := strings.CutPrefix(line, textK); ok {
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &run.Text); err != nil {
				t.Fatalf("unreadable rendered text %q: %v", rest, err)
			}
		}
		if rest, ok := strings.CutPrefix(line, hrefsK); ok {
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &run.Hrefs); err != nil {
				t.Fatalf("unreadable hrefs %q: %v", rest, err)
			}
		}
		if rest, ok := strings.CutPrefix(line, attrsK); ok {
			var triples [][3]string
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &triples); err != nil {
				t.Fatalf("unreadable attrs %q: %v", rest, err)
			}
			for _, tr := range triples {
				run.Attrs = append(run.Attrs, clientAttr{Tag: tr[0], Name: tr[1], Value: tr[2]})
			}
		}
		if rest, ok := strings.CutPrefix(line, ctlK); ok {
			if err := json.Unmarshal([]byte(strings.TrimSpace(rest)), &run.Controls); err != nil {
				t.Fatalf("unreadable controls %q: %v", rest, err)
			}
		}
	}

	return run
}

var (
	scriptOrStyle = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>`)
	anyTag        = regexp.MustCompile(`<[^>]+>`)
	whitespace    = regexp.MustCompile(`\s+`)
)

// serverText is the visible text of the page the server sent, normalized the
// same way the shim normalizes the client's — so the two are comparable without
// depending on how either splits its nodes.
//
// Scoped to the mount point, because that is all the client replaces. The head's
// <title> and the IR/state payloads are part of the document and not part of the
// render, and including them would make every page differ for a reason that is
// not a bug.
func serverText(html string) string {
	const open = `<div id="fa-root" data-fa-mount>`

	start := strings.Index(html, open)
	if start < 0 {
		return ""
	}
	start += len(open)

	end := strings.Index(html[start:], `<script type="application/json" id="fa-ir">`)
	if end < 0 {
		end = len(html) - start
	}

	body := html[start : start+end]

	stripped := anyTag.ReplaceAllString(scriptOrStyle.ReplaceAllString(body, ""), " ")

	return whitespace.ReplaceAllString(unescapeEntities(stripped), "")
}

// unescapeEntities undoes the escaping the server applies when it writes text
// into HTML. Only the handful that matter here; a full entity table would be a
// second HTML parser, and the shim's side never escapes at all.
func unescapeEntities(s string) string {
	return strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&#34;", `"`,
	).Replace(s)
}

// An app whose shape is the one that broke: a component taking no parameters.
// Go marshals its empty parameter list as JSON `null`, and the client iterated
// it without asking whether it was there.
const componentApp = `app Widgets:
    entity Item:
        id: int
        label: text
    component Banner:
        box:
            text "a component with no parameters at all"
    component Labelled(label: text):
        box:
            text "{label}"
    view Home at "/":
        box:
            use Banner()
            use Labelled("with one")
            for i in Item by id desc limit 5:
                text "{i.label}"
`

// The client must render the page the server sent, not throw on it.
//
// A throw here is not a degraded render — mount() replaces the document body, so
// an exception anywhere in the tree leaves a blank page. The assertion is
// therefore deliberately weak about *what* rendered and absolute about whether
// anything did.
func TestTheClientRendersEveryPageTheServerSends(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, componentApp)

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	if n := renderClient(t, html); n <= 1 {
		t.Errorf("the client produced %d nodes — the page is blank", n)
	}
}

// Every route of the real F33D3R app, through the real client.
//
// The app under test is the one being built, not a fixture, because the bugs
// this catches came from combinations a fixture would not have: a param-less
// component nested inside a list inside a layout shared by five routes.
func TestTheRealAppRendersOnEveryRoute(t *testing.T) {
	app, err := filepath.Abs("../../facets/home.fct")
	if err != nil || !fileExists(app) {
		t.Skip("facets/home.fct not present")
	}

	e := startEngine(t)
	a := startAppFile(t, e, app)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("post", "a post to render"); code != 200 {
		t.Fatalf("post: %d %s", code, body)
	}

	for _, route := range []string{
		"/", "/profile/alice", "/post/1", "/notifications", "/search",
	} {
		t.Run(route, func(t *testing.T) {
			code, html := a.get(route)
			if code != 200 {
				t.Fatalf("GET %s: %d", route, code)
			}
			nodes, clientText := renderClientText(t, html)
			if nodes <= 1 {
				t.Errorf("%s renders blank on the client", route)
				return
			}

			// The real assertion: the client must render the same page the
			// server already rendered, character for character.
			want := serverText(html)
			if clientText != want {
				serverWindow, clientWindow := firstDifference(want, clientText)
				t.Errorf("%s: the client rendered different text than the server sent.\n"+
					"This is what a mismatch between the two page walks looks like —\n"+
					"a region addressed by the wrong path, or an aggregate read from\n"+
					"the wrong position, shows up as a changed number rather than as\n"+
					"a blank page.\n\nserver: %s\nclient: %s",
					route, serverWindow, clientWindow)
			}
		})
	}
}

// firstDifference renders the two strings around the first place they diverge,
// because a 4 KB diff of concatenated page text is unreadable and the answer is
// almost always in the forty characters either side.
func firstDifference(want, got string) (string, string) {
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}

	window := func(s string) string {
		start := max(i-40, 0)
		end := min(i+40, len(s))

		return s[start:end]
	}

	return window(want), window(got)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// An aggregate in a plain interpolation, under a list row.
//
// This is the one shape that separates the two page walks, and no other test in
// the tree has it. `TestTheRealAppRendersOnEveryRoute` runs the real app, but
// every aggregate in `facets/home.fct` sits inside a `use` argument or an `if`
// condition — regions whose fill function sets the render path itself — so the
// client happened to be reading from the same address the server wrote to, and
// the char-for-char comparison agreed by accident.
//
// `countingApp` (regression_test.go) puts `count(...)` in a `text` under a `for`
// instead. The server records that count at the text node's own path; a client
// that only sets its path at region boundaries looks it up at the enclosing
// `for`'s path, misses, falls back to scanning a collection the page deliberately
// does not ship, and renders `0` — the right shape, a wrong number, on a page
// that otherwise looks perfectly fine.
func TestTheClientCountsFromTheNodeThatEvaluatedTheCount(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, countingApp)

	if code, body := a.action("signup", "alice", "hunter2hunter2"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	for _, body := range []string{"the first note", "the second note"} {
		if code, out := a.action("write", body); code != 200 {
			t.Fatalf("write %q: %d %s", body, code, out)
		}
	}
	// Note 1 gets three stars, note 2 gets one — so a count read from the wrong
	// address cannot coincide with the right one, and neither can zero.
	for _, note := range []string{"1", "1", "1", "2"} {
		if code, body := a.action("star", note); code != 200 {
			t.Fatalf("star note %s: %d %s", note, code, body)
		}
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	// The server's own paint is the control: if this is wrong the client is not
	// the thing under test.
	if !strings.Contains(html, "has 3 stars") || !strings.Contains(html, "has 1 stars") {
		t.Fatalf("the server did not paint the counts it was supposed to: %s", serverText(html))
	}

	nodes, clientText := renderClientText(t, html)
	if nodes <= 1 {
		t.Fatalf("the client rendered nothing")
	}

	want := serverText(html)
	if clientText != want {
		serverWindow, clientWindow := firstDifference(want, clientText)
		t.Errorf("the client renumbered an aggregate the server had already computed.\n"+
			"An aggregate is addressed by the path of the node that evaluated it, so a\n"+
			"client that only tracks that path at region boundaries reads the enclosing\n"+
			"region's address, misses, and renders 0.\n\nserver: %s\nclient: %s",
			serverWindow, clientWindow)
	}
}
