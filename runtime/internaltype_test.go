package runtime

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// `type Name @internal:` — a value object a library's actions pass among
// themselves: usable as an action parameter, absent from the contract, and
// refused as an api route's parameter or reply.
func TestInternalTypeStaysOffTheContract(t *testing.T) {
	src := `app I:
    type Shown:
        n: int
    type Hidden @internal:
        n: int
    entity Row:
        id: int
        n: int
    action keep(h: Hidden) @internal:
        add Row { n: h.n }
    action shown(n: int) -> Shown:
        run keep(Hidden{n: n})
        return Shown{n: count(Row)}
    api POST "/api/shown" -> shown
    view V at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if schemas["Shown"] == nil || schemas["Hidden"] != nil {
		t.Errorf("schemas: Shown %v, Hidden %v — want only Shown", schemas["Shown"] != nil, schemas["Hidden"] != nil)
	}
	if got, err := srv.RunValue("ada", "member", true, "shown", []any{4}); err != nil || got.(map[string]any)["n"] != 1 {
		t.Errorf("shown = %v, %v", got, err)
	}
	bad := strings.Replace(src, `api POST "/api/shown" -> shown`, `api POST "/api/keep" -> keep`, 1)
	bad = strings.Replace(bad, "action keep(h: Hidden) @internal:", "action keep(h: Hidden):", 1)
	if _, err := compile.String(bad); err == nil || !strings.Contains(err.Error(), "@internal") {
		t.Errorf("an api route over an internal type: err = %v", err)
	}
}
