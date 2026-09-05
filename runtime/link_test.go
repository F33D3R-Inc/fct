package runtime

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Inputs chosen for the characters the two languages' built-ins disagree about,
// plus the ones that actually break routing.
var pathEscapeCases = []string{
	"alice",
	"a/b",         // would silently become two path segments
	"a?x=1",       // would turn the rest of the path into a query string
	"a#frag",      // would become a fragment
	"100%",        // a bare % starts an escape sequence
	"a=b",         // Go's PathEscape leaves this; encodeURIComponent does not
	"a&b$c+d:e@f", // ...and the rest of that set
	"a!b*c'd(e)",  // encodeURIComponent leaves these; Go's PathEscape does not
	"a b",
	"ünïcøde",
	"",
	"..",
}

func TestEscapePathSegmentEscapesEverythingReserved(t *testing.T) {
	for _, in := range pathEscapeCases {
		got := escapePathSegment(in)

		for i := 0; i < len(got); i++ {
			c := got[i]

			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
				c >= '0' && c <= '9' ||
				c == '-' || c == '.' || c == '_' || c == '~' ||
				c == '%' ||
				c >= 'A' && c <= 'F'

			if !ok {
				t.Errorf("escapePathSegment(%q) = %q, which still contains %q",
					in, got, string(c))
				break
			}
		}
	}

	// The cases that matter, spelled out.
	for in, want := range map[string]string{
		"a/b":   "a%2Fb",
		"a?x=1": "a%3Fx%3D1",
		"100%":  "100%25",
	} {
		if got := escapePathSegment(in); got != want {
			t.Errorf("escapePathSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// The server and the client each render a link's href. If they disagree by one
// character the link changes the instant the client takes over — under the
// cursor, invisibly, until someone clicks it. Neither language's built-in escaper
// matches the other, which is why both sides spell the rule out and this runs
// them over the same inputs.
func TestClientPathEscapeMatchesServer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot check the client mirror")
	}

	var script strings.Builder
	script.WriteString(`
function escapePathSegment(s) {
  let out = "";
  for (const byte of new TextEncoder().encode(s)) {
    const c = String.fromCharCode(byte);
    if (/[A-Za-z0-9\-._~]/.test(c)) out += c;
    else out += "%" + byte.toString(16).toUpperCase().padStart(2, "0");
  }
  return out;
}
const cases = `)
	script.WriteString("[")
	for i, c := range pathEscapeCases {
		if i > 0 {
			script.WriteString(",")
		}
		script.WriteString(strconv.Quote(c))
	}
	script.WriteString("];\nconsole.log(JSON.stringify(cases.map(escapePathSegment)));\n")

	out, err := exec.Command(node, "-e", script.String()).Output()
	if err != nil {
		t.Fatalf("running the client mirror: %v", err)
	}

	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("unreadable client output %q: %v", out, err)
	}

	if len(got) != len(pathEscapeCases) {
		t.Fatalf("client returned %d results, want %d", len(got), len(pathEscapeCases))
	}

	for i, in := range pathEscapeCases {
		if want := escapePathSegment(in); got[i] != want {
			t.Errorf("escapePathSegment(%q): client %q, server %q", in, got[i], want)
		}
	}
}

// The mirror this test runs must be the one that ships.
func TestClientPathEscapeMirrorIsTheShippedSource(t *testing.T) {
	raw, err := os.ReadFile("assets/facet.js")
	if err != nil {
		t.Fatalf("reading the shipped client: %v", err)
	}

	for _, want := range []string{
		`if (/[A-Za-z0-9\-._~]/.test(c)) out += c;`,
		`else out += "%" + byte.toString(16).toUpperCase().padStart(2, "0");`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("assets/facet.js no longer contains %q — the mirror checked by\n"+
				"TestClientPathEscapeMatchesServer has drifted from the shipped client", want)
		}
	}
}
