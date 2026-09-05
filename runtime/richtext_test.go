package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// markdownHTML renders a safe Markdown subset; the JS mirror in assets/facet.js
// must produce identical output. These cases pin the contract (and the escaping).
var markdownCases = []struct{ name, in, want string }{
	{"h1", "# Title", "<h1>Title</h1>"},
	{"h2/h3", "## Sub\n### Subsub", "<h2>Sub</h2><h3>Subsub</h3>"},
	{"inline", "**bold** and *it* and `c`", "<p><strong>bold</strong> and <em>it</em> and <code>c</code></p>"},
	{"list", "- a\n- b", "<ul><li>a</li><li>b</li></ul>"},
	{"paragraph multiline", "line1\nline2", "<p>line1<br>line2</p>"},
	{"code fence escaped", "```\nx < 1 & y\n```", "<pre><code>x &lt; 1 &amp; y</code></pre>"},
	{"xss escaped", "<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>"},
	{"heading then para", "# T\nbody", "<h1>T</h1><p>body</p>"},
	// A quotation is the one block an article needs that this subset had no
	// spelling for. It is the same lowering as the list directly above it — a
	// run of prefixed lines collected into one wrapper — and its body is a
	// paragraph, joined the way the paragraph arm joins lines, so a quote and
	// the prose around it are made of the same thing.
	{"blockquote", "> a quote", "<blockquote><p>a quote</p></blockquote>"},
	{"blockquote multiline", "> one\n> two", "<blockquote><p>one<br>two</p></blockquote>"},
	{"blockquote inline + escaping", "> **x** & <y>", "<blockquote><p><strong>x</strong> &amp; &lt;y&gt;</p></blockquote>"},
	{"blockquote ends a paragraph", "body\n> q", "<p>body</p><blockquote><p>q</p></blockquote>"},
	{"a bare angle bracket is prose", ">notaquote", "<p>&gt;notaquote</p>"},
}

func TestMarkdownHTML(t *testing.T) {
	for _, c := range markdownCases {
		t.Run(c.name, func(t *testing.T) {
			if got := markdownHTML(c.in); got != c.want {
				t.Errorf("markdownHTML(%q):\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// The mirror in assets/facet.js must produce identical output — the comment
// above has said so since the subset was written, and nothing checked it. It is
// checked here now, the way link_test.go, classmerge_test.go and attrtext_test.go
// check theirs: by running the four functions that SHIP, pulled out of the
// shipped file, over the same cases the Go side is pinned to.
//
// A divergence here is not cosmetic. Richtext is written into the DOM with
// innerHTML on the client and into the first paint by the server, so the two
// sides disagreeing means an article changes shape at hydration — or, in the
// escaping cases, that one side emits a tag the other neutralised.
func TestClientMarkdownMatchesTheServer(t *testing.T) {
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
	for _, fn := range []string{"mdEscape", "mdInline", "mdBlockStart", "markdownHtml"} {
		script.WriteString(extractFunction(t, src, fn) + "\n")
	}
	var ins []string
	for _, c := range markdownCases {
		ins = append(ins, c.in)
	}
	b, err := json.Marshal(ins)
	if err != nil {
		t.Fatal(err)
	}
	script.WriteString("const cases = " + string(b) + ";\n")
	script.WriteString("console.log(JSON.stringify(cases.map(markdownHtml)));\n")

	out, err := exec.Command(node, "-e", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("running the client mirror: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	for i, c := range markdownCases {
		if got[i] != c.want {
			t.Errorf("%s: the client renders %q, the server renders %q", c.name, got[i], c.want)
		}
	}
}
