package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A route's contract documentation lives in its declaration's block: its
// summary and description, the wire type its body is, the error statuses it
// publishes, its operationId, and each path/query parameter's description,
// closed values and client-facing type. Wire types say exactly what crosses:
// maps `{T}`, nested lists `[[T]]`, any JSON, and `T? or null` (left out when
// empty, maybe-null where present); every schema is closed. A route over its
// rate budget reports it in X-RateLimit-* headers on every answer.
const contractDocsApp = `app Docs:
    type MintRequest:
        policy: text
        note: text?
        extra: {text}?
    type NumberDTO:
        id: text
        links: {text}
        grid: [[int]]
        blob: json
        when: datetime? or null
        owner: text or null
    type Ping:
        n: int
    policy member:
        actor != "guest"
    contract "/api/v2/contract" rate read since "2026-09-13":
        title "Docs API"
        description "What the docs app serves."
        bearer "The token from POST /api/v2/sessions."
    action mint(policy: text, note: text?) -> NumberDTO:
        requires member
        check policy != "" "That is not a contact policy." status 400 code "unknown_policy"
        return NumberDTO{id: "1", links: fromJson("{\"a\":\"b\"}"), grid: fromJson("[[1,2],[3]]"), blob: fromJson("{}"), owner: ""}
    action getOne(id: int, view: text?) -> NumberDTO:
        return NumberDTO{id: "" + id, links: fromJson("{}"), grid: fromJson("[]"), blob: fromJson("7"), owner: "ada"}
    action join(id: int) -> Ping:
        requires member
        return Ping{n: id}
    api POST "/api/v2/numbers" -> mint status 201 rate write since "2026-09-18":
        summary "Mint a Number."
        description "The caller's own Number."
        body MintRequest
        errors 400, 409
        operation "mintNumber"
    api GET "/api/v2/numbers/{id}" -> getOne rate read since "2026-09-18":
        summary "One Number."
        id: text "The Number's id."
        view: text "Which projection." one of "full", "short"
    stream "/api/v2/rooms/{id}/events" requires member rate read since "2026-09-18":
        connect -> join as ping
        summary "One room as an event stream."
        id: text "Room id."
        errors 403, 404
        ping: Ping since "2026-09-06" "A ping."
    view Home at "/":
        text "x"
`

func TestContractDocumentation(t *testing.T) {
	g, err := compile.String(contractDocsApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	doc := buildContract(g, 0)
	raw, _ := json.Marshal(doc)
	var d map[string]any
	json.Unmarshal(raw, &d)
	paths := d["paths"].(map[string]any)
	jsonOf := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	if d["openapi"] != "3.1.0" || jsonOf(d["info"].(map[string]any)["title"]) != `"Docs API"` || d["info"].(map[string]any)["description"] != "What the docs app serves." {
		t.Fatalf("info = %v %v", d["openapi"], d["info"])
	}
	if got := d["components"].(map[string]any)["securitySchemes"].(map[string]any)["bearer"].(map[string]any)["description"]; got != "The token from POST /api/v2/sessions." {
		t.Fatalf("bearer description = %v", got)
	}

	mint := paths["/api/v2/numbers"].(map[string]any)["post"].(map[string]any)
	for k, want := range map[string]string{
		"operationId":  `"mintNumber"`,
		"summary":      `"Mint a Number."`,
		"description":  `"The caller's own Number."`,
		"parameters":   `[]`,
		"requestBody":  `{"content":{"application/json":{"schema":{"oneOf":[{"$ref":"#/components/schemas/MintRequest"},{"type":"null"}]}}},"required":true}`,
		"x-rate-limit": `{"burst":15,"headers":["X-RateLimit-Limit","X-RateLimit-Remaining","X-RateLimit-Reset","Retry-After"],"keyed_by":"ip","limiter":"write","per_minute":60}`,
	} {
		if got := jsonOf(mint[k]); got != want {
			t.Errorf("mint %s = %s, want %s", k, got, want)
		}
	}
	var codes []string
	for c := range mint["responses"].(map[string]any) {
		codes = append(codes, c)
	}
	if got := jsonOf(sortedStrings(codes)); got != `["201","400","401","409"]` {
		t.Errorf("mint responses = %s: the published errors, and 401 for a session route", got)
	}
	if got := jsonOf(mint["responses"].(map[string]any)["201"]); !strings.Contains(got, `{"oneOf":[{"$ref":"#/components/schemas/NumberDTO"},{"type":"null"}]}`) {
		t.Errorf("mint reply = %s", got)
	}

	one := paths["/api/v2/numbers/{id}"].(map[string]any)["get"].(map[string]any)
	if got := jsonOf(one["operationId"]); got != `"getNumbersId"` {
		t.Errorf("default operationId = %s (method + path)", got)
	}
	params := jsonOf(one["parameters"])
	for _, want := range []string{
		`{"description":"The Number's id.","in":"path","name":"id","required":true,"schema":{"type":"string"}}`,
		`{"description":"Which projection.","in":"query","name":"view","required":false,"schema":{"enum":["full","short"],"type":"string"}}`,
	} {
		if !strings.Contains(params, want) {
			t.Errorf("parameters = %s, want %s", params, want)
		}
	}
	if _, has := one["responses"].(map[string]any)["304"]; has {
		t.Error("a conditional GET's 304 is a documented convention, not a per-route response")
	}

	room := paths["/api/v2/rooms/{id}/events"].(map[string]any)["get"].(map[string]any)
	if room["summary"] != "One room as an event stream." || room["x-stream"] != nil {
		t.Errorf("stream op = %v", room)
	}
	if got := jsonOf(room["parameters"]); !strings.Contains(got, `{"description":"Room id.","in":"path","name":"id","required":true,"schema":{"type":"string"}}`) {
		t.Errorf("stream parameters = %s", got)
	}
	codes = nil
	for c := range room["responses"].(map[string]any) {
		codes = append(codes, c)
	}
	if got := jsonOf(sortedStrings(codes)); got != `["200","401","403","404"]` {
		t.Errorf("stream responses = %s", got)
	}
	for _, ev := range d["x-stream-events"].([]any) {
		if ev.(map[string]any)["name"] == "hello" {
			t.Error("a stream that declares no hello publishes none")
		}
	}

	schemas := d["components"].(map[string]any)["schemas"].(map[string]any)
	for name, want := range map[string]string{
		"MintRequest": `{"additionalProperties":false,"properties":{"extra":{"additionalProperties":{"type":"string"},"type":"object"},"note":{"type":"string"},"policy":{"type":"string"}},"required":["policy"],"type":"object"}`,
		"NumberDTO":   `{"additionalProperties":false,"properties":{"blob":{"description":"Any JSON value."},"grid":{"items":{"items":{"type":"integer"},"type":"array"},"type":"array"},"id":{"type":"string"},"links":{"additionalProperties":{"type":"string"},"type":"object"},"owner":{"type":["string","null"]},"when":{"format":"date-time","type":["string","null"]}},"required":["blob","grid","id","links","owner"],"type":"object"}`,
	} {
		if got := jsonOf(schemas[name]); got != want {
			t.Errorf("%s = %s\nwant %s", name, got, want)
		}
	}

	// Served: the reply's shapes, and the budget headers on every answer.
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v2/numbers/5")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if jsonOf(got) != `{"blob":7,"grid":[],"id":"5","links":{},"owner":"ada"}` {
		t.Errorf("reply = %s: an empty `T? or null` is left out, a map and nested list cross as JSON", jsonOf(got))
	}
	if resp.Header.Get("X-RateLimit-Limit") != "300" || resp.Header.Get("X-RateLimit-Remaining") == "" || resp.Header.Get("X-RateLimit-Reset") == "" {
		t.Errorf("budget headers = %v", resp.Header)
	}
}

func sortedStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}

// A route's documentation must agree with its action.
func TestContractDocumentationRefusals(t *testing.T) {
	base := `app R:
    type Req:
        a: int
    type Out:
        x: int
    action act(a: text, id: int) -> Out:
        return Out{x: 1}
    view Home at "/":
        text "x"
`
	for _, tc := range []struct{ decl, want string }{
		{"    api POST \"/r/{id}\" -> act:\n        body Nope\n", "is not a declared wire type"},
		{"    api POST \"/r/{id}\" -> act:\n        body Req\n", "disagree on its type"},
		{"    api GET \"/r/{id}\" -> act:\n        body Req\n", "a GET carries no request body"},
		{"    api GET \"/r/{id}\" -> act:\n        zz: text \"No such.\"\n", "not a parameter of action"},
		{"    api GET \"/r/{id}\" -> act:\n        id: bool \"Wrong.\"\n", "takes it as int"},
		{"    api POST \"/r/{id}\" -> act:\n        a: text \"In the body.\"\n", "travels in the body"},
		{"    api GET \"/r/{id}\" -> act:\n        errors 200\n", "400-599"},
		{"    api GET \"/r/{id}\" -> act:\n        operation \"not an id\"\n", "must be an identifier"},
		{"    api GET \"/r/{id}\" -> act:\n", "no indented block"},
		{"    type M:\n        m: {json}\n", "a map or nested list of json is just json"},
	} {
		_, err := compile.String(strings.Replace(base, "    view Home", tc.decl+"    view Home", 1))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.decl, err, tc.want)
		}
	}
}
