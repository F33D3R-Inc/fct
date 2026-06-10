package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

func generate(t *testing.T, src string) *Output {
	t.Helper()
	facets, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := Generate(facets)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func manifestEntry(t *testing.T, out *Output, name string) map[string]any {
	t.Helper()
	var m struct {
		Facets []map[string]any `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatalf("manifest unmarshal: %v", err)
	}
	for _, f := range m.Facets {
		if f["name"] == name {
			return f
		}
	}
	t.Fatalf("no manifest entry for %q", name)
	return nil
}

// THE FLAGSHIP GUARANTEE: client-rendered primitives emit ZERO server template.
// A compromised server has nothing to render vault plaintext with.
func TestClientPrimitivesEmitNoServerTemplate(t *testing.T) {
	for _, src := range []struct{ name, code string }{
		{"DM", "vault DM:\n    what:\n        envelope: str\n    decrypt:\n        <p>{plaintext}</p>\n"},
		{"Clip", "media Clip:\n    what:\n        url: str\n    source:\n        <hls src=\"{url}\"/>\n"},
		{"Typing", "signal Typing:\n    what:\n        who: str\n    ttl: 5s\n"},
	} {
		out := generate(t, src.code)
		if _, ok := out.Templates[src.name]; ok {
			t.Errorf("%s: a server template was emitted for a client-rendered primitive", src.name)
		}
		e := manifestEntry(t, out, src.name)
		if _, hasTmpl := e["template"]; hasTmpl {
			t.Errorf("%s: manifest must not carry a template for a client-rendered primitive", src.name)
		}
	}
}

// vault/media bodies are captured in the manifest's client field for the runtime.
func TestClientBodyRecordedInManifest(t *testing.T) {
	out := generate(t, "vault DM:\n    what:\n        envelope: str\n    decrypt:\n        <p>{plaintext}</p>\n")
	e := manifestEntry(t, out, "DM")
	if e["kind"] != "vault" {
		t.Errorf("kind = %v, want vault", e["kind"])
	}
	client, _ := e["client"].(string)
	if !strings.Contains(client, "{plaintext}") {
		t.Errorf("client body not recorded: %q", client)
	}
}

// Server-rendered primitives still emit a template and record their extras.
func TestServerPrimitiveManifest(t *testing.T) {
	out := generate(t, "stream LiveChat:\n    what:\n        msgs: MsgList\n    throttle: 200ms\n    window: 100\n    looks:\n        <div>{msgs}</div>\n")
	if _, ok := out.Templates["LiveChat"]; !ok {
		t.Fatal("stream must emit a server template")
	}
	e := manifestEntry(t, out, "LiveChat")
	if e["kind"] != "stream" || e["throttle"] != "200ms" || e["window"] != "100" {
		t.Errorf("manifest extras wrong: %+v", e)
	}
	if e["template"] != "LiveChat.tmpl.html" {
		t.Errorf("template = %v", e["template"])
	}
}

// A decrypt: body references client-runtime values (the decrypted plaintext), not
// what: props — so it must NOT be field-checked against what:. This compiles even
// though `plaintext` is undeclared, where the same reference in a facet's looks:
// would be a compile error.
func TestClientBodyNotFieldChecked(t *testing.T) {
	generate(t, "vault DM:\n    what:\n        envelope: str\n    decrypt:\n        <p>{plaintext}</p>\n")
}

// Typed codegen works for every kind (all have what:).
func TestGoStructsForNonFacetKind(t *testing.T) {
	facets, err := parser.Parse("feed Timeline:\n    what:\n        items: PostList\n        cursor: str\n    looks:\n        <ul>{items}</ul>\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := GoStructs("app", facets)
	if !strings.Contains(got, "type TimelineData struct") || !strings.Contains(got, "Items PostList") {
		t.Errorf("GoStructs missing feed struct:\n%s", got)
	}
}
