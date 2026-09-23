package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// `type Name as "schema":` publishes the type's schema under a contract name
// that is not an fct identifier (a lowercase name, a generic instantiation's
// reflected name), and every $ref to the type follows it; fct code keeps
// using Name.
func TestTypeSchemaNameAlias(t *testing.T) {
	src := `app W:
    type KeyWire as "sealedKeyWire":
        key_id: text
    type Envelope as "sealedEnvelope":
        sealed: [KeyWire]
    type WorkPage as "V2Page[pkg.V2WorkDTO]":
        items: [Envelope]
        next_cursor: text?
    action page() -> WorkPage:
        return WorkPage{items: [Envelope{sealed: [KeyWire{key_id: "k1"}]}], next_cursor: "c"}
    api GET "/api/v2/page" -> page
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := &apiClient{t: t, base: ts.URL}
	code, body, _ := c.do("GET", "/api/v2/page", "")
	if code != 200 || body["next_cursor"] != "c" {
		t.Fatalf("page = %d %v", code, body)
	}
	resp, err := http.Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, want := range []string{"sealedKeyWire", "sealedEnvelope", "V2Page[pkg.V2WorkDTO]"} {
		if schemas[want] == nil {
			t.Fatalf("schema %q missing: %v", want, keysOf(schemas))
		}
	}
	for _, gone := range []string{"KeyWire", "Envelope", "WorkPage"} {
		if schemas[gone] != nil {
			t.Fatalf("schema %q published under its fct name too", gone)
		}
	}
	raw, _ := json.Marshal(doc)
	if !strings.Contains(string(raw), `"$ref":"#/components/schemas/sealedKeyWire"`) ||
		!strings.Contains(string(raw), `"$ref":"#/components/schemas/V2Page[pkg.V2WorkDTO]"`) {
		t.Fatalf("refs do not follow the schema name: %s", raw)
	}

	// Two types may not publish the same schema name.
	dup := strings.Replace(src, `type Envelope as "sealedEnvelope":`, `type Envelope as "sealedKeyWire":`, 1)
	if _, err := compile.String(dup); err == nil || !strings.Contains(err.Error(), "already publishes") {
		t.Fatalf("duplicate schema name: %v", err)
	}
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// errMessage is a declared route's error message, from its APIErrorDTO envelope.
func errMessage(obj map[string]any) string {
	e, _ := obj["error"].(map[string]any)
	return toStr(e["message"])
}
