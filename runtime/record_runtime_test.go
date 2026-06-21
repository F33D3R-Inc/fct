package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/internal/ir"
)

// A brain returns a structured object; the action binds the whole record and reads
// its fields — scalar (score, ok) and list (reasons) — into authoritative state,
// which surfaces to the client as deltas. This is the record feature end to end.
const modApp = `app Mod:
    service Brain at "http://placeholder.invalid":
        moderate(body: text) -> Verdict
    record Verdict:
        score: int
        reasons: [text]
        ok: bool
    state lastScore: int = 0
    state verdictOk: bool = true
    state notes: [text] = []
    action moderate(body: text):
        let v = call Brain.moderate(body)
        lastScore = v.score
        verdictOk = v.ok
        notes = v.reasons
    view Home at "/":
        box:
            text "{lastScore}"
`

func TestRecordReturnBindsFields(t *testing.T) {
	brain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A loosely-typed reply: score as a JSON number, ok as a real bool, reasons a
		// list of strings. coerceOne must land them on the record's declared types.
		json.NewEncoder(w).Encode(map[string]any{
			"score":   7,
			"ok":      false,
			"reasons": []string{"spam", "all caps"},
			"extra":   "ignored", // a field the record doesn't declare is dropped
		})
	}))
	defer brain.Close()

	g, err := compile.String(modApp)
	if err != nil {
		t.Fatal(err)
	}
	g.Services[0].URL = brain.URL

	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/moderate", "application/json", strings.NewReader(`{"args":["hello"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("moderate returned %d", resp.StatusCode)
	}
	var out struct {
		Deltas map[string]any `json:"deltas"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if got := toInt(out.Deltas["lastScore"]); got != 7 {
		t.Errorf("lastScore = %v, want 7 (v.score)", out.Deltas["lastScore"])
	}
	if got := truthy(out.Deltas["verdictOk"]); got != false {
		t.Errorf("verdictOk = %v, want false (v.ok)", out.Deltas["verdictOk"])
	}
	notes, _ := out.Deltas["notes"].([]any)
	if len(notes) != 2 || toStr(notes[0]) != "spam" || toStr(notes[1]) != "all caps" {
		t.Errorf("notes = %v, want [spam, all caps] (v.reasons)", out.Deltas["notes"])
	}
}

// coerceOne decodes a record reply field-by-field against its schema, coercing
// each member to its declared type and dropping undeclared keys.
func TestCoerceOneRecord(t *testing.T) {
	srv := &Server{byRecord: map[string]*ir.Record{
		"Verdict": {Name: "Verdict", Fields: []ir.RecordField{
			{Name: "score", Type: "int"},
			{Name: "reasons", Type: "text", List: true},
			{Name: "ok", Type: "bool"},
		}},
	}}
	got := srv.coerceOne(map[string]any{
		"score":   float64(9),
		"reasons": []any{"a", "b"},
		"ok":      true,
		"junk":    1,
	}, "Verdict")
	want := record{"score": 9, "reasons": []any{"a", "b"}, "ok": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coerceOne record = %#v, want %#v", got, want)
	}
	// A missing field falls to its type's zero, not a nil hole.
	got = srv.coerceOne(map[string]any{"score": float64(1)}, "Verdict")
	m := got.(record)
	if m["ok"] != false || !reflect.DeepEqual(m["reasons"], []any{}) {
		t.Errorf("missing fields should zero-fill: %#v", m)
	}
}
