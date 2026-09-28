package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// Search on the site and over the API is the reference's: words stemmed
// and accent-folded, ranked, a misspelling tolerated with the correction
// offered — server render and client render agreeing on the order.
func TestSearchRanksStemsAndToleratesTypos(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "f33d3r_com.fct")
	act := func(name string, args ...any) string {
		t.Helper()
		code, body := a.action(name, args...)
		if code != 200 {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		return body
	}
	act("webSignup", "ada", "pw12345678")
	act("post", "Just adopted a baby elephant at the sanctuary today #zoo")
	act("post", "The elephant in the room is the zoo budget")
	act("post", "Best café crème in Zoë's neighbourhood")
	act("post", "widget widget widget best widget shop for all your widget needs")
	act("post", "not really a widget kind of day")

	page := func() (string, string) {
		t.Helper()
		code, body := a.get("/search")
		if code != 200 {
			t.Fatalf("GET /search: %d", code)
		}
		return body, serverText(body)
	}
	// Stemmed: "elephants" finds both elephant posts, not the others.
	act("webSearch", "elephants")
	body, text := page()
	if !strings.Contains(text, "Posts·2") || !strings.Contains(text, "babyelephant") || !strings.Contains(text, "elephantintheroom") || strings.Contains(text, "café") {
		t.Errorf("elephants: %q", text)
	}
	// Ranked: the post that is all widgets leads the one that mentions one.
	act("webSearch", "widget")
	body, text = page()
	if i, j := strings.Index(text, "widgetwidgetwidget"), strings.Index(text, "notreallyawidget"); i < 0 || j < 0 || i > j {
		t.Errorf("widget: the denser post should lead: %q", text)
	}
	run, _ := runClientAgainst(t, a, body, nil)
	if i, j := strings.Index(run.Text, "widgetwidgetwidget"), strings.Index(run.Text, "notreallyawidget"); i < 0 || j < 0 || i > j {
		t.Errorf("widget: the client render lost the rank order: %q", run.Text)
	}
	// Accents fold; a misspelling is tolerated and says so.
	act("webSearch", "cafe creme")
	if _, text = page(); !strings.Contains(text, "Posts·1") || !strings.Contains(text, "Bestcafé") {
		t.Errorf("cafe creme: %q", text)
	}
	act("webSearch", "neighbourhod")
	if _, text = page(); !strings.Contains(text, "Bestcafé") || !strings.Contains(text, "closematches") {
		t.Errorf("neighbourhod: %q", text)
	}
	// Operators: #tag by tag, -word, "phrase", Latest.
	act("webSearch", "#zoo")
	if _, text = page(); !strings.Contains(text, "Posts·1") || !strings.Contains(text, "babyelephant") {
		t.Errorf("#zoo: %q", text)
	}
	act("webSearch", `elephant -baby`)
	if _, text = page(); !strings.Contains(text, "Posts·1") || !strings.Contains(text, "elephantintheroom") {
		t.Errorf("elephant -baby: %q", text)
	}
	act("webSearch", "elephant")
	act("webSearchLane", true)
	if _, text = page(); strings.Index(text, "elephantintheroom") > strings.Index(text, "babyelephant") {
		t.Errorf("Latest: newest first: %q", text)
	}
	// The API answers the same search.
	var got struct {
		Works []struct {
			Body string `json:"body"`
		} `json:"works"`
		Total      int    `json:"total"`
		Typo       bool   `json:"typo"`
		DidYouMean string `json:"did_you_mean"`
		People     []struct {
			Handle string `json:"handle"`
		} `json:"people"`
	}
	raw := act("search", "widget")
	var env struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil || json.Unmarshal(env.Value, &got) != nil {
		t.Fatalf("search reply: %s", raw)
	}
	if got.Total != 2 || len(got.Works) != 2 || !strings.HasPrefix(got.Works[0].Body, "widget widget") {
		t.Errorf("API search widget: %s", raw)
	}
	raw = act("search", "ada")
	if err := json.Unmarshal([]byte(raw), &env); err != nil || json.Unmarshal(env.Value, &got) != nil || len(got.People) == 0 || got.People[0].Handle != "ada" {
		t.Errorf("API search people: %s", raw)
	}
}
