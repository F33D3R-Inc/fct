package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

// defaultGoldenContract is the legacy product's published v2 contract — the
// spec facets/api/main.fct implements. It lives in the read-only legacy tree;
// FACET_GOLDEN_CONTRACT points the test elsewhere.
const defaultGoldenContract = "/home/hiiro/f33d3r/feed-engine/internal/handler/testdata/api_v2_contract.golden.json"

// contractAllowlist is every difference between the golden contract and the
// served one that is intended, each with the reason it was reviewed. A path is
// the JSON-pointer-like location the comparison reports (see diffJSON).
var contractAllowlist = map[string]string{
	// The document's own identity.
	"/info/version":     "the sha256 of the document itself: equal only when every byte is, which the entries below rule out",
	"/x-schema-version": "legacy publishes its database migration head (90); fct publishes this deployment's data-schema ordinal from its own contract history",
	"/x-facet-catalog":  "legacy's web Facet (HTML fragment) catalogue, which Android binds to; this app serves no web Facets",

	// The contract routes are the runtime's own, documented for what fct
	// serves: the legacy texts describe legacy's /api/v1 surface and its
	// migrations, and its history lives in a database that can be down (503);
	// fct's history is the process's own entity table, which cannot be.
	"/paths/~1api~1v2~1contract/get/summary":                "legacy's text names its /api/v1 surface; fct's describes this document",
	"/paths/~1api~1v2~1contract/get/description":            "legacy's text describes its migration head and caching; fct states its conventions in info.description",
	"/paths/~1api~1v2~1contract~1version/get/summary":       "legacy's text names its migration head; fct's names its data-schema version",
	"/paths/~1api~1v2~1contract~1history/get/description":   "legacy's text cites its migration 0065",
	"/paths/~1api~1v2~1contract~1diff/get/description":      "legacy's diff description; fct's diff route documents itself through its schema",
	"/paths/~1api~1v2~1contract~1history/get/responses/503": "legacy's history table can be unreachable; fct's is in-process",
	"/paths/~1api~1v2~1contract~1diff/get/responses/503":    "legacy's history table can be unreachable; fct's is in-process",

	// Room streams resume like the account stream: fct replays a room's
	// missed frames after Last-Event-ID too, and says so.
	"/paths/~1api~1v2~1live~1{id}~1events/get/parameters/header Last-Event-ID":        "fct's room streams resume; legacy's did not",
	"/paths/~1api~1v2~1live~1{id}~1events/get/parameters/query last_event_id":         "fct's room streams resume; legacy's did not",
	"/paths/~1api~1v2~1frequencies~1{id}~1events/get/parameters/header Last-Event-ID": "fct's room streams resume; legacy's did not",
	"/paths/~1api~1v2~1frequencies~1{id}~1events/get/parameters/query last_event_id":  "fct's room streams resume; legacy's did not",

	// A file part of a multipart upload is a binary string; legacy's route
	// registration typed it as a plain string.
	"/paths/~1api~1v2~1works~1{id}~1captions/post/requestBody/content/multipart~1form-data/schema/properties/captions/format": "a file part is format: binary",
	"/paths/~1api~1v2~1works~1{id}~1poster/post/requestBody/content/multipart~1form-data/schema/properties/image/format":      "a file part is format: binary",

	// Known app gaps, documented as the app serves them until they are
	// ported: legacy's token route is the OAuth authorization-code exchange
	// (HTTP Basic client credentials, form grant_type=authorization_code&code=),
	// and its track upload is a multipart audio upload; fct's app takes JSON.
	"/paths/~1api~1v2~1dev~1oauth~1token/post/requestBody": "app gap: the authorization-code exchange is not ported (fct issues tokens for client credentials in a JSON body)",
	"/paths/~1api~1v2~1music~1tracks/post/requestBody":     "app gap: the multipart audio upload is not ported (fct takes the track's URLs in a JSON body)",
}

// The f33d3r API app must publish exactly the golden contract: every path and
// operation with its parameters, request body and responses, every schema, and
// every documented mutation (x-mutation-events) and stream event
// (x-stream-events). Any difference not on contractAllowlist fails.
func TestAPIContractMatchesGolden(t *testing.T) {
	goldenPath := os.Getenv("FACET_GOLDEN_CONTRACT")
	if goldenPath == "" {
		goldenPath = defaultGoldenContract
	}
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden contract not available at %s (set FACET_GOLDEN_CONTRACT): %v", goldenPath, err)
	}
	var golden any
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("golden contract %s: %v", goldenPath, err)
	}

	app, _ := filepath.Abs("../../facets/api/main.fct")
	graph, err := compile.File(app)
	if err != nil {
		t.Fatalf("compiling %s: %v", app, err)
	}
	t.Setenv("FACET_SECRET", "conformance-secret-conformance-secret")
	if os.Getenv("FACET_LOG_LEVEL") == "" {
		t.Setenv("FACET_LOG_LEVEL", "error")
	}
	srv, err := runtime.NewInMemory(graph)
	if err != nil {
		t.Fatalf("starting the app: %v", err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/_contract: %d %s", resp.StatusCode, body)
	}
	if out := os.Getenv("FACET_CONTRACT_OUT"); out != "" {
		os.WriteFile(out, body, 0o644)
	}
	var served any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("served contract: %v", err)
	}

	var diffs []string
	diffJSON("", normalizeContract(golden), normalizeContract(served), &diffs)
	used := map[string]bool{}
	var unexpected []string
	for _, d := range diffs {
		path := d[:strings.Index(d, ": ")]
		if _, ok := contractAllowlist[path]; ok {
			used[path] = true
			continue
		}
		unexpected = append(unexpected, d)
	}
	for path := range contractAllowlist {
		if !used[path] {
			t.Errorf("allowlisted difference %s no longer occurs — remove it from contractAllowlist", path)
		}
	}
	if len(unexpected) > 0 {
		shown := unexpected
		if len(shown) > 200 {
			shown = shown[:200]
		}
		t.Errorf("the served contract differs from the golden one in %d place(s):\n  %s", len(unexpected), strings.Join(shown, "\n  "))
	}
}

// normalizeContract turns the contract's order-free lists into maps keyed by
// what identifies an entry, so a reordering is not a difference and a change
// is reported where it is: parameters by in+name, x-mutation-events by
// event_type, x-stream-events by stream+name.
func normalizeContract(v any) any {
	doc, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, x := range doc {
		out[k] = x
	}
	out["x-mutation-events"] = keyed(doc["x-mutation-events"], func(e map[string]any) string { return fmt.Sprint(e["event_type"]) })
	out["x-stream-events"] = keyed(doc["x-stream-events"], func(e map[string]any) string { return fmt.Sprint(e["stream"], " ", e["name"]) })
	if paths, ok := doc["paths"].(map[string]any); ok {
		np := map[string]any{}
		for p, ops := range paths {
			nops := map[string]any{}
			for m, op := range ops.(map[string]any) {
				o, ok := op.(map[string]any)
				if !ok {
					nops[m] = op
					continue
				}
				no := map[string]any{}
				for k, x := range o {
					no[k] = x
				}
				if ps, ok := o["parameters"]; ok {
					no["parameters"] = keyed(ps, func(e map[string]any) string { return fmt.Sprint(e["in"], " ", e["name"]) })
				}
				nops[m] = no
			}
			np[p] = nops
		}
		out["paths"] = np
	}
	return out
}

func keyed(v any, key func(map[string]any) string) any {
	list, ok := v.([]any)
	if !ok {
		return v
	}
	m := map[string]any{}
	for _, e := range list {
		if em, ok := e.(map[string]any); ok {
			m[key(em)] = em
		}
	}
	return m
}

// diffJSON appends one line per difference between want (golden) and got
// (served): "path: …".
func diffJSON(path string, want, got any, out *[]string) {
	wm, wok := want.(map[string]any)
	gm, gok := got.(map[string]any)
	if wok && gok {
		keys := map[string]bool{}
		for k := range wm {
			keys[k] = true
		}
		for k := range gm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := path + "/" + strings.ReplaceAll(k, "/", "~1")
			w, inW := wm[k]
			g, inG := gm[k]
			switch {
			case !inG:
				*out = append(*out, p+": missing (golden "+brief(w)+")")
			case !inW:
				*out = append(*out, p+": extra (served "+brief(g)+")")
			default:
				diffJSON(p, w, g, out)
			}
		}
		return
	}
	if !reflect.DeepEqual(want, got) {
		*out = append(*out, path+": golden "+brief(want)+", served "+brief(got))
	}
}

func brief(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 160 {
		return string(b[:160]) + "…"
	}
	return string(b)
}
