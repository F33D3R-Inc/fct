package runtime

import "testing"

// markdownHTML renders a safe Markdown subset; the JS mirror in assets/facet.js
// must produce identical output. These cases pin the contract (and the escaping).
func TestMarkdownHTML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"h1", "# Title", "<h1>Title</h1>"},
		{"h2/h3", "## Sub\n### Subsub", "<h2>Sub</h2><h3>Subsub</h3>"},
		{"inline", "**bold** and *it* and `c`", "<p><strong>bold</strong> and <em>it</em> and <code>c</code></p>"},
		{"list", "- a\n- b", "<ul><li>a</li><li>b</li></ul>"},
		{"paragraph multiline", "line1\nline2", "<p>line1<br>line2</p>"},
		{"code fence escaped", "```\nx < 1 & y\n```", "<pre><code>x &lt; 1 &amp; y</code></pre>"},
		{"xss escaped", "<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>"},
		{"heading then para", "# T\nbody", "<h1>T</h1><p>body</p>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := markdownHTML(c.in); got != c.want {
				t.Errorf("markdownHTML(%q):\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
