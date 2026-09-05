package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"facet/internal/ir"
)

// The destinations the render-time scheme check has to separate. Every entry is
// a shape someone reaches for when they want a link to run instead of navigate,
// or a shape the two implementations could plausibly disagree on: the case
// variations (a fold that is not ASCII-only diverges between Go and JS), the
// missing host (`https:///x` addresses this origin, not another), and the
// schemes that are not on the list.
var externalHrefCases = []string{
	"https://github.com/F33D3R-Inc/fct",
	"http://example.com",
	"HTTPS://EXAMPLE.COM",
	"HtTps://example.com/a?b#c",
	"mailto:hi@f33d3r.com",
	"MAILTO:hi@f33d3r.com",
	"mailto:",
	"https://",
	"https:///x",
	"http://?q=1",
	"http://#f",
	"javascript:alert(1)",
	"JavaScript:alert(1)",
	"  javascript:alert(1)",
	"data:text/html,<script>alert(1)</script>",
	"vbscript:msgbox(1)",
	"/docs",
	"//evil.com/x",
	"#install",
	"",
	"httpsİ://example.com",
}

// safeExternalHref is asked on both sides, of the same string, at the same
// moment. If they disagree, an href is an anchor on first paint and inert text
// after hydration (or the reverse) — a link that changes under the cursor, which
// is the failure link_test.go, attrtext_test.go and classmerge_test.go each exist
// to catch one instance of.
func TestClientSafeExternalHrefMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
  function safeExternalHref(href) {
    for (const scheme of ["https://", "http://"]) {
      if (!hasPrefixFold(href, scheme)) continue;
      const rest = href.slice(scheme.length);
      for (let i = 0; i < rest.length; i++) {
        if (rest[i] === "/" || rest[i] === "?" || rest[i] === "#") return i > 0;
      }
      return rest !== "";
    }
    if (hasPrefixFold(href, "mailto:")) return href.length > "mailto:".length;
    return false;
  }
  function hasPrefixFold(s, prefix) {
    if (s.length < prefix.length) return false;
    for (let i = 0; i < prefix.length; i++) {
      let c = s.charCodeAt(i);
      if (c >= 65 && c <= 90) c += 32;
      if (c !== prefix.charCodeAt(i)) return false;
    }
    return true;
  }
const cases = [`)
	for i, c := range externalHrefCases {
		if i > 0 {
			script.WriteString(",")
		}
		script.WriteString(strconv.Quote(c))
	}
	script.WriteString("];\nconsole.log(JSON.stringify(cases.map(safeExternalHref)));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got []bool
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}
	if len(got) != len(externalHrefCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(externalHrefCases))
	}

	for i, in := range externalHrefCases {
		if want := safeExternalHref(in); got[i] != want {
			t.Errorf("safeExternalHref(%q): client %v, server %v", in, got[i], want)
		}
	}

	// The cases the whole check exists for, stated rather than inferred.
	for in, want := range map[string]bool{
		"https://github.com/F33D3R-Inc/fct":        true,
		"http://example.com":                       true,
		"mailto:hi@f33d3r.com":                     true,
		"javascript:alert(1)":                      false,
		"JavaScript:alert(1)":                      false,
		"data:text/html,<script>alert(1)</script>": false,
		"https:///x":                               false,
		"/docs":                                    false,
	} {
		if got := safeExternalHref(in); got != want {
			t.Errorf("safeExternalHref(%q) = %v, want %v", in, got, want)
		}
	}
}

// The mirror above, the anchor branch, and the `rel` that goes with it must be
// the code that ships, or the parity proved here is a coincidence.
func TestClientExternalLinkMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}

	for _, want := range []string{
		`if (!hasPrefixFold(href, scheme)) continue;`,
		`if (hasPrefixFold(href, "mailto:")) return href.length > "mailto:".length;`,
		`if (c >= 65 && c <= 90) c += 32;`,
		// A runtime value still may not become an off-site anchor, and an external
		// one is still re-checked on the string that reaches the browser.
		`if ((node.route && !isAppRoute(href)) ||`,
		`(node.external && !safeExternalHref(href))) {`,
		`if (node.external) a.setAttribute("rel", "noopener noreferrer");`,
		// A fragment is not a route.
		`const i = href.indexOf("#");`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the client half of the\n"+
				"external-link rule has drifted from what this file checks", want)
		}
	}
}

// renderLink renders one link node the way a page render does.
func renderLink(t *testing.T, routes []ir.Route, n ir.Node) string {
	t.Helper()
	s := &Server{ir: &ir.IR{Routes: routes}}
	rd := &renderer{s: s, regions: map[string][]any{}, mat: &materializer{s: s, out: map[string]any{}, counts: map[string]int{}}}
	var b strings.Builder
	rd.node(&b, n, map[string]any{}, "")
	return b.String()
}

// The server's half of the same rule, on the node forms the compiler produces.
//
// The last case is the one that matters most: it is the property that existed
// before external links and had to survive them. A destination that ARRIVES as a
// value — a row from the database, a route parameter — is still confined to this
// app's routes, so an off-site URL sitting in a record renders as inert text no
// matter what its scheme is. Only a destination the author wrote may leave.
func TestServerRendersExternalLinks(t *testing.T) {
	routes := []ir.Route{{Path: "/"}, {Path: "/docs"}}
	label := []ir.Seg{{Lit: "go"}}

	valueSeg := func(v string) []ir.Seg {
		return []ir.Seg{{Expr: &ir.Expr{Kind: "lit", Val: v, VType: "text"}}}
	}

	cases := []struct {
		name string
		node ir.Node
		want string
	}{{
		name: "a literal external URL is an anchor, with rel and without target",
		node: ir.Node{Kind: "link", Label: label, Path: "https://github.com/F33D3R-Inc/fct", External: true},
		want: `<a class="fa-link" href="https://github.com/F33D3R-Inc/fct" rel="noopener noreferrer">go</a>`,
	}, {
		name: "a mailto destination is an anchor",
		node: ir.Node{Kind: "link", Label: label, Path: "mailto:hi@f33d3r.com", External: true},
		want: `<a class="fa-link" href="mailto:hi@f33d3r.com" rel="noopener noreferrer">go</a>`,
	}, {
		name: "an external template escapes the value into its path segment",
		node: ir.Node{Kind: "link", Label: label, External: true, PathSegs: append(
			[]ir.Seg{{Lit: "https://github.com/F33D3R-Inc/"}}, valueSeg("a/b")...)},
		want: `<a class="fa-link" href="https://github.com/F33D3R-Inc/a%2Fb" rel="noopener noreferrer">go</a>`,
	}, {
		name: "an internal path carries no rel",
		node: ir.Node{Kind: "link", Label: label, Path: "/docs"},
		want: `<a class="fa-link" href="/docs">go</a>`,
	}, {
		name: "an anchor destination is an anchor, and the target node carries the id",
		node: ir.Node{Kind: "link", Label: label, Path: "/docs#install", Anchor: "here"},
		want: `<a class="fa-link" id="here" href="/docs#install">go</a>`,
	}, {
		name: "an External node whose href is not on the allowlist renders as inert text",
		node: ir.Node{Kind: "link", Label: label, Path: "javascript:alert(1)", External: true},
		want: `<span class="fa-link">go</span>`,
	}, {
		name: "a runtime value that names a route is an anchor",
		node: ir.Node{Kind: "link", Label: label, Route: true, PathSegs: valueSeg("/docs")},
		want: `<a class="fa-link" href="/docs">go</a>`,
	}, {
		name: "a runtime value that is an off-site URL is still inert text",
		node: ir.Node{Kind: "link", Label: label, Route: true, PathSegs: valueSeg("https://evil.example/phish")},
		want: `<span class="fa-link">go</span>`,
	}, {
		name: "a runtime value carrying a javascript: payload is still inert text",
		node: ir.Node{Kind: "link", Label: label, Route: true, PathSegs: valueSeg("javascript:alert(1)")},
		want: `<span class="fa-link">go</span>`,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderLink(t, routes, c.node); got != c.want {
				t.Errorf("rendered\n  %s\nwant\n  %s", got, c.want)
			}
		})
	}
}

// A fragment is a position inside a page, not a route. Without stripping it the
// route table is asked about a one-segment path spelled `docs#install`, which
// matches nothing — so a computed `/docs#install` would render as inert text and a
// guarded route's policy would never be consulted.
func TestAFragmentIsNotPartOfTheRoute(t *testing.T) {
	routes := []ir.Route{{Path: "/"}, {Path: "/docs"}}

	for href, want := range map[string]bool{
		"/docs":         true,
		"/docs#install": true,
		"#install":      false,
		"/nope":         false,
	} {
		if got := isAppRoute(routes, href); got != want {
			t.Errorf("isAppRoute(%q) = %v, want %v", href, got, want)
		}
	}
}
