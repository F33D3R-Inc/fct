package runtime

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"facet/internal/ir"
)

// The FacetQL query wire types are the seam between fct's Query and the engine's
// POST /nodes/query (AGENT_LOG §4b). These tests pin the request encoding and the
// {nodes,next} response decoding without a live engine, and confirm a decoded node
// round-trips back into a row through the same Load-path helper (nodeRecord).

func TestFQQueryRequestEncoding(t *testing.T) {
	// where p.likes > 0 — the ir.Expr serializes with its own JSON tags, unchanged.
	where := &ir.Expr{Kind: "bin", Op: ">",
		L: &ir.Expr{Kind: "get", Obj: &ir.Expr{Kind: "ref", Name: "p"}, Field: "likes"},
		R: &ir.Expr{Kind: "lit", Val: 0, VType: "int"}}
	b, err := json.Marshal(fqQueryRequest{
		Kind: "Post", Where: where, ItemVar: "p", Order: "likes", Desc: true, Limit: 25, After: "CUR",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// canonical field names (§4b), byte-for-byte.
	for _, sub := range []string{
		`"kind":"Post"`, `"item_var":"p"`, `"order":"likes"`, `"desc":true`,
		`"limit":25`, `"after":"CUR"`, `"where":`, `"field":"likes"`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("query request missing %q in:\n%s", sub, got)
		}
	}

	// first page omits the cursor (empty = first page).
	b, _ = json.Marshal(fqQueryRequest{Kind: "Post", ItemVar: "p", Limit: 10})
	if strings.Contains(string(b), `"after"`) {
		t.Errorf("first-page request must omit after:\n%s", b)
	}
}

func TestFQQueryResponseDecoding(t *testing.T) {
	body := []byte(`{"nodes":[` +
		`{"address":"Post:1","kind":"Post","data":"{\"id\":1,\"author\":\"ada\",\"likes\":3}"}],` +
		`"next":"NEXTCUR"}`)
	nodes, next, err := decodeQueryPage(body)
	if err != nil {
		t.Fatal(err)
	}
	if next != "NEXTCUR" {
		t.Errorf("next = %q, want NEXTCUR", next)
	}
	if len(nodes) != 1 || nodes[0].Address != "Post:1" {
		t.Fatalf("nodes = %#v, want one Post:1 node", nodes)
	}

	// the node decodes into a row via the same Load-path helper.
	r, err := nodeRecord(post, nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	if r["id"] != 1 || r["author"] != "ada" || r["likes"] != 3 {
		t.Errorf("nodeRecord = %#v, want {id:1 author:ada likes:3}", r)
	}

	// the last page carries an empty cursor.
	_, next, err = decodeQueryPage([]byte(`{"nodes":[],"next":""}`))
	if err != nil || next != "" {
		t.Errorf("last page: next=%q err=%v, want \"\" nil", next, err)
	}
}

// PurgeExpiredSessions emits one native delete_where op. This pins that op's wire
// shape to §4b: tagged `type`, `kind`, and the predicate under `where`.
func TestFQDeleteWhereOpEncoding(t *testing.T) {
	pred := fqBin("<", fqGet("_expires_unix"), fqLitInt(1700))
	b, err := json.Marshal(fqTxRequest{Operations: []fqTxOp{
		{Type: "delete_where", Kind: "__session", Where: pred},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	// (Go json HTML-escapes "<" to < on the wire; serde decodes it back — so
	// assert on the unescaped parts, matching how the query-request test avoids it.)
	for _, sub := range []string{
		`"operations":[`, `"type":"delete_where"`, `"kind":"__session"`,
		`"where":`, `"field":"_expires_unix"`, `"name":"item"`, `"val":1700`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("delete_where op missing %q in:\n%s", sub, got)
		}
	}
	// no predicate => where omitted (behaves like clear_kind).
	b, _ = json.Marshal(fqTxOp{Type: "delete_where", Kind: "__session"})
	if strings.Contains(string(b), `"where"`) {
		t.Errorf("nil-predicate delete_where must omit where:\n%s", b)
	}
}

// A session stores the whole persistedSession plus a numeric _expires_unix so the
// purge predicate can compare expiry numerically. This confirms both survive a
// round-trip through the reserved-kind data JSON.
func TestFQSessionDataRoundTrip(t *testing.T) {
	exp := time.Unix(1735689600, 0).UTC()
	in := fqSessionData{
		persistedSession: persistedSession{
			Actor: "ada", Role: "member", Verified: true,
			State: map[string]any{"cart": "x"}, Expires: exp,
		},
		ExpiresUnix: exp.Unix(),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"_expires_unix":1735689600`) {
		t.Errorf("session data missing numeric _expires_unix:\n%s", b)
	}
	if !strings.Contains(string(b), `"actor":"ada"`) {
		t.Errorf("session data missing promoted actor field:\n%s", b)
	}
	var out fqSessionData
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Actor != "ada" || !out.Verified || out.ExpiresUnix != exp.Unix() {
		t.Errorf("round-trip = %#v, want actor=ada verified=true expUnix=%d", out, exp.Unix())
	}
}

// A FacetQL index name becomes a filename on the engine, which admits at most 64
// bytes of [A-Za-z0-9_-]. It must also be a pure function of (entity, field), or
// a second boot would declare a second index instead of re-declaring the same one.
func TestFQIndexName(t *testing.T) {
	if got := fqIndexName("Tweet", "created"); got != "idx_Tweet_created" {
		t.Errorf("fqIndexName(Tweet, created) = %q, want the pgStore base name idx_Tweet_created", got)
	}
	long := strings.Repeat("Entity", 12) // 72 bytes before the field is even added
	for _, name := range []string{
		fqIndexName("Tweet", "created"), fqIndexName(long, "created"), fqIndexName("Odd.Name", "a b"),
	} {
		if len(name) == 0 || len(name) > fqMaxIndexName {
			t.Errorf("index name %q is %d bytes; the engine admits 1..%d", name, len(name), fqMaxIndexName)
		}
		for _, b := range []byte(name) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-') {
				t.Errorf("index name %q contains %q, outside the engine's alphabet", name, b)
			}
		}
	}
	if fqIndexName(long, "created") == fqIndexName(long, "updated") {
		t.Error("two fields of one entity must not collide onto one index name")
	}
	if fqIndexName("Tweet", "created") != fqIndexName("Tweet", "created") {
		t.Error("the name must be a pure function of (entity, field)")
	}
}

// Clear must be one native `clear_kind` op, not N deletes.
//
// The engine has had this primitive all along and `fqTx.Clear` always used it;
// `fqStore.Clear` paged the kind and issued a DELETE per node, so "clear a kind"
// meant two different things — atomic inside a transaction, N non-atomic round
// trips outside one — depending on nothing the caller could see.
func TestFQClearEmitsOneNativeOp(t *testing.T) {
	got, err := json.Marshal(fqTxOp{Type: "clear_kind", Kind: "Tweet"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The contract (AGENT_LOG §4b) is exactly these two keys: a delete or clear
	// op that carried stray zero-valued node fields would not match it.
	if want := `{"type":"clear_kind","kind":"Tweet"}`; string(got) != want {
		t.Errorf("clear_kind op = %s, want %s", got, want)
	}
}

// The offset-paged kind listing is gone, and must stay gone: it was the last way
// this client could ask for a deep offset, which FacetQL refuses past
// FACETQL_MAX_QUERY_OFFSET — a refusal that used to stop an app booting at 10k
// rows. Both former callers now use primitives that cost the same at any depth.
func TestFQClientHasNoOffsetPagedListing(t *testing.T) {
	src, err := os.ReadFile("fqclient.go")
	if err != nil {
		t.Fatalf("reading the client: %v", err)
	}
	for _, gone := range []string{"listKind", `q.Set("offset"`} {
		if strings.Contains(string(src), gone) {
			t.Errorf("fqclient.go still contains %q — the deep-offset path is back", gone)
		}
	}
}
