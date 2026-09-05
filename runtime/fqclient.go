package runtime

// Minimal FacetQL HTTP client used by fqStore. It is deliberately self-contained
// (no cross-module import of the facetql/fa client, which lives in another Go
// module) and speaks only the endpoints the Store adapter needs:
//
//	POST   /node               upsert a node (client-supplied address)
//	DELETE /node/:address      remove a node
//	GET    /nodes?kind&limit&offset   list a kind, paged
//	POST   /transaction        all-or-nothing batch of ops
//	POST   /publish            cross-instance event fan-out
//	GET    /admin/indexes      the declared secondary indexes
//	POST   /admin/indexes      declare one
//	DELETE /admin/indexes/:name  drop one
//	GET    /admin/references   the declared referential (cascade) rules
//	POST   /admin/references   declare one
//	GET    /                   liveness probe
//
// Auth is a per-identity token sent as the `x-api-key` header on every request
// (AGENT_LOG §4b). See parseFacetQLURL for how the base URL and token are
// extracted from FACET_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"facet/internal/ir"
)

// fqClient is a thin HTTP wrapper around a FacetQL instance.
type fqClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// fqNode is the wire representation of a FacetQL node. `data` is opaque JSON text
// owned by the adapter: every fct row field (including relation ids) is encoded
// there. The four coordinate axes default to 0 for v1 (see fqStore risk notes).
type fqNode struct {
	Address   string `json:"address"`
	Kind      string `json:"kind"`
	X         uint8  `json:"x"`
	Y         uint8  `json:"y"`
	Z         uint8  `json:"z"`
	Q         uint8  `json:"q"`
	Data      string `json:"data"`
	Public    bool   `json:"public"`
	Owner     string `json:"owner,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// fqTxOp is one operation inside a POST /transaction batch. It serializes to the
// canonical wire contract (AGENT_LOG §4b): a serde-tagged object with key "type"
// and snake_case values, where each op type carries exactly its own fields:
//
//	{ "type":"insert_node", "address":…, "kind":…, "x":0,"y":0,"z":0,"q":0, "data":…, "public":false }
//	{ "type":"delete_node", "address":… }
//	{ "type":"clear_kind",  "kind":… }
//	{ "type":"delete_where", "kind":…, "where":<Expr|omitted> }
//	{ "type":"set_if", "address":…, "field":…, <one expectation>, "set":{…} }
//
// MarshalJSON emits the exact per-type shape (no stray zero fields on delete/clear),
// so the body matches the contract byte-for-byte.
type fqTxOp struct {
	Type    string
	Address string
	Kind    string
	X       uint8
	Y       uint8
	Z       uint8
	Q       uint8
	Data    string
	Public  bool
	Where   *ir.Expr // delete_where predicate; nil = unconditional (like clear_kind)

	// set_if only: the field the condition is tested against, the condition, and
	// the fields merged into the node's data when it holds.
	Field  string
	Expect fqExpect
	Set    map[string]any
}

// fqExpect is the single condition a `set_if` op tests against one field.
//
// FacetQL demands exactly one of expect_le / expect_eq / expect_absent and
// answers 400 to a body carrying two or none (facetql api/routes.rs). That is a
// bug this type makes unrepresentable rather than one a caller has to remember:
// the discriminant is unexported, so an expectation can only be built by a
// constructor, and a constructor produces exactly one.
//
// Only the AtMost form has a caller today (ReserveCron's "advance this only if it
// is already due"). The other two the engine offers — equality, for a revision
// counter, and absence, for create-once — are one constructor and one arm each
// when something needs them; adding them unused would be two shapes on the wire
// nothing has ever sent.
type fqExpect struct {
	kind  fqExpectKind
	bound float64
}

type fqExpectKind uint8

const (
	fqExpectNone fqExpectKind = iota // the zero value: not a set_if
	fqExpectAtMost
)

// fqExpectLE is the lease/deadline condition: the field is a number and is at
// most bound. A field that is absent, null or non-numeric does not satisfy it —
// the engine refuses to coerce, because a deadline it read as "due" by accident
// would hand the same slot to every caller.
func fqExpectLE(bound float64) fqExpect { return fqExpect{kind: fqExpectAtMost, bound: bound} }

func (o fqTxOp) MarshalJSON() ([]byte, error) {
	switch o.Type {
	case "insert_node":
		return json.Marshal(struct {
			Type    string `json:"type"`
			Address string `json:"address"`
			Kind    string `json:"kind"`
			X       uint8  `json:"x"`
			Y       uint8  `json:"y"`
			Z       uint8  `json:"z"`
			Q       uint8  `json:"q"`
			Data    string `json:"data"`
			Public  bool   `json:"public"`
		}{o.Type, o.Address, o.Kind, o.X, o.Y, o.Z, o.Q, o.Data, o.Public})
	case "delete_node":
		return json.Marshal(struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		}{o.Type, o.Address})
	case "clear_kind":
		return json.Marshal(struct {
			Type string `json:"type"`
			Kind string `json:"kind"`
		}{o.Type, o.Kind})
	case "delete_where":
		// Predicate under JSON key "where"; omitted when nil (== clear_kind).
		return json.Marshal(struct {
			Type  string   `json:"type"`
			Kind  string   `json:"kind"`
			Where *ir.Expr `json:"where,omitempty"`
		}{o.Type, o.Kind, o.Where})
	case "set_if":
		// Exactly one expectation key, chosen by the constructor that built it.
		// `set` is merged into the node's data by the engine, never a
		// replacement, so an unrelated field on the node is not clobbered.
		set := o.Set
		if set == nil {
			// An assert-only compare-and-set is `{}`, never `null`: the engine's
			// field defaults to an empty map and would refuse a null.
			set = map[string]any{}
		}
		switch o.Expect.kind {
		case fqExpectAtMost:
			return json.Marshal(struct {
				Type     string         `json:"type"`
				Address  string         `json:"address"`
				Field    string         `json:"field"`
				ExpectLE float64        `json:"expect_le"`
				Set      map[string]any `json:"set"`
			}{o.Type, o.Address, o.Field, o.Expect.bound, set})
		default:
			return nil, fmt.Errorf("facetql: set_if on %s.%s carries no expectation", o.Address, o.Field)
		}
	default:
		return nil, fmt.Errorf("facetql: unknown transaction op type %q", o.Type)
	}
}

// fqTxRequest is the POST /transaction body: { "operations": [ <op>, … ] }.
type fqTxRequest struct {
	Operations []fqTxOp `json:"operations"`
}

// fqIndexDef is one declared secondary index over a `data` field — the wire shape
// of both /admin/indexes bodies. Name is the index's identity and also its
// filename on the engine, which is why it has a restricted alphabet (see
// fqIndexName); Kind is the node kind it covers and Field the top-level `data`
// field it orders by.
type fqIndexDef struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Field string `json:"field"`
	// Unique refuses a write that would give two nodes of the kind the same
	// value for the field. It is declared on the index because it *is* the
	// index — the check is a prefix scan of the entries already there — which is
	// also why a reference by a data field requires the referenced field to
	// carry one. Omitted when false, so an ordinary index's body is unchanged.
	Unique bool `json:"unique,omitempty"`
}

// fqReferenceDef is one declared referential rule — the wire shape of both
// /admin/references bodies. Kind.Field is the referencing (child) side, which
// holds the parent's key and whose index serves the lookup; ParentKind is the
// referenced side and ParentField the value on it that the child's field
// matches. OnDelete is what deleting the parent does: FacetQL takes `cascade`,
// `restrict` or `set_null`, and fct declares only `cascade`, because that is the
// one rule an fct relation has ever meant (ir.Field: "a foreign key with ON
// DELETE CASCADE") and the one pgStore's DDL emits.
type fqReferenceDef struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Field       string `json:"field"`
	ParentKind  string `json:"parent_kind"`
	ParentField string `json:"parent_field,omitempty"`
	OnDelete    string `json:"on_delete"`
}

// fqPublish is the POST /publish body (replaces Postgres LISTEN/NOTIFY).
type fqPublish struct {
	Channel string `json:"channel"`
	Payload string `json:"payload"`
}

// parseFacetQLURL turns a FACET_DATABASE_URL of the form
//
//	facetql://[token@]host:port[?tls=1]
//
// into an HTTP base URL and API token. The token may also be supplied via the
// FACETQL_TOKEN env var (checked by the caller) if absent from the URL.
func parseFacetQLURL(raw string) (baseURL, token string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse FACET_DATABASE_URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("FACET_DATABASE_URL %q has no host (expected facetql://[token@]host:port)", raw)
	}
	if u.User != nil {
		// facetql://TOKEN@host  or  facetql://user:TOKEN@host
		if pw, ok := u.User.Password(); ok && pw != "" {
			token = pw
		} else {
			token = u.User.Username()
		}
	}
	scheme := "http"
	if u.Query().Get("tls") == "1" {
		scheme = "https"
	}
	return scheme + "://" + u.Host, token, nil
}

func newFQClient(baseURL, token string) *fqClient {
	return &fqClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// fqHTTPError is a non-2xx response from FacetQL, carrying the status code.
//
// The status is part of the error because *which* refusal it was is what a
// caller has to act on: "this identity may not do that" (403) is a different
// fact from "the engine rejected the request" (400) or "the node is gone" (404),
// and only one of them is ever a caller's to tolerate. Reading that off the
// rendered message with a string match would make the message a contract, which
// it is not — the text below is this client's, but the body inside it is the
// engine's and changes with it. Error() renders exactly what this client has
// always produced, so anything that only prints an error is unaffected.
type fqHTTPError struct {
	Method     string
	Path       string
	Status     int    // e.g. 403
	StatusText string // e.g. "403 Forbidden"
	Body       string // the engine's own message, verbatim
}

func (e *fqHTTPError) Error() string {
	return fmt.Sprintf("facetql %s %s: HTTP %s: %s", e.Method, e.Path, e.StatusText, e.Body)
}

// fqStatus reports the HTTP status an error carries, unwrapping whatever it has
// been wrapped in on the way up. It is 0 when the failure never reached a
// response at all (a dial failure, a marshal error) — deliberately distinct from
// every real status, so "not a 403" and "never got an answer" cannot be confused.
func fqStatus(err error) int {
	var he *fqHTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// do issues a request, marshaling body (if any) as JSON and returning the raw
// response body plus the HTTP status. A non-2xx status yields an *fqHTTPError;
// the status is still returned alongside so callers (e.g. deleteNode) can treat
// a 404 as a benign, idempotent outcome without inspecting the error.
func (c *fqClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("facetql marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("facetql build request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("x-api-key", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("facetql %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, &fqHTTPError{
			Method: method, Path: path,
			Status: resp.StatusCode, StatusText: resp.Status,
			Body: strings.TrimSpace(string(data)),
		}
	}
	return data, resp.StatusCode, nil
}

// upsert stores or replaces a node (POST /node).
func (c *fqClient) upsert(ctx context.Context, n fqNode) error {
	_, _, err := c.do(ctx, http.MethodPost, "/node", n)
	return err
}

// createIfAbsent creates a node only if its address is free (POST /node with
// `if_absent`), reporting whether this caller was the one that created it.
//
// Plain POST /node is an upsert: two callers racing to create the same address
// both "succeed" and the second silently overwrites the first. `if_absent` makes
// the engine check for the node and refuse with 409 under the same write lock it
// inserts through (facetql api/routes.rs, create_node), so exactly one racer
// creates it and every other one is told so. That is what makes a first-ever cron
// tick decidable — see fqStore.ReserveCron, the only caller.
func (c *fqClient) createIfAbsent(ctx context.Context, n fqNode) (created bool, err error) {
	body := struct {
		fqNode
		IfAbsent bool `json:"if_absent"`
	}{n, true}
	_, status, err := c.do(ctx, http.MethodPost, "/node", body)
	switch {
	case status == http.StatusConflict:
		return false, nil // it already exists: somebody else created it
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// deleteNode removes a node by address (DELETE /node/:address). A missing node is
// treated as success (idempotent delete).
func (c *fqClient) deleteNode(ctx context.Context, address string) error {
	_, status, err := c.do(ctx, http.MethodDelete, "/node/"+url.PathEscape(address), nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// getNode fetches a single node by address (GET /node/:address). A missing node
// is a benign (found=false) result, not an error — the caller decides.
func (c *fqClient) getNode(ctx context.Context, address string) (fqNode, bool, error) {
	data, status, err := c.do(ctx, http.MethodGet, "/node/"+url.PathEscape(address), nil)
	if status == http.StatusNotFound {
		return fqNode{}, false, nil
	}
	if err != nil {
		return fqNode{}, false, err
	}
	var n fqNode
	if err := json.Unmarshal(data, &n); err != nil {
		return fqNode{}, false, fmt.Errorf("facetql decode node %q: %w", address, err)
	}
	return n, true, nil
}

// claim atomically leases a node to the caller (POST /node/:address/claim). The
// claimer identity is the request's token owner (server-side), so this is the
// FOR-UPDATE-SKIP-LOCKED equivalent: exactly one caller wins an unclaimed node.
// Returns won=true on success, won=false if it is already claimed (409) or gone
// (404) — both are normal "someone else got it / nothing to lease" outcomes.
func (c *fqClient) claim(ctx context.Context, address string) (won bool, err error) {
	_, status, err := c.do(ctx, http.MethodPost, "/node/"+url.PathEscape(address)+"/claim", nil)
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusConflict, http.StatusNotFound:
		return false, nil
	default:
		return false, err
	}
}

// The offset-paged kind listing that used to live here is deliberately gone.
//
// It was the last path in this client that could ask FacetQL for a deep offset,
// and deep offsets are a trap the engine bounds rather than serves: reaching row
// `offset` means walking the access path and discarding every row before it, so
// FacetQL refuses past FACETQL_MAX_QUERY_OFFSET. That refusal used to stop an app
// from booting at 10 000 rows. Both former callers now use the primitive that
// costs the same at any depth — `loadAll` pages by keyset cursor, and `Clear`
// emits one native `clear_kind` op — so there is no longer a way to spell the
// slow query, which is the only reliable way to keep it from coming back.

// decodeNodes accepts either a bare JSON array of nodes or a {"nodes":[...]}
// envelope.
func decodeNodes(data []byte) ([]fqNode, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []fqNode
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("facetql decode nodes array: %w", err)
		}
		return arr, nil
	}
	var env struct {
		Nodes []fqNode `json:"nodes"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("facetql decode nodes envelope: %w", err)
	}
	return env.Nodes, nil
}

// fqQueryRequest is the POST /nodes/query body: the pushed-down read filter
// (AGENT_LOG §4b). `where` is the ir.Expr predicate — its JSON tags already mirror
// FacetQL's Rust predicate.rs field-for-field, so it serializes without any
// translation. `after` is the opaque keyset cursor from the previous page; it is
// omitted on the first page (empty = first page).
type fqQueryRequest struct {
	Kind    string   `json:"kind"`
	Where   *ir.Expr `json:"where,omitempty"`
	ItemVar string   `json:"item_var"`
	Order   string   `json:"order"`
	Desc    bool     `json:"desc"`
	Limit   int      `json:"limit"`
	After   string   `json:"after,omitempty"`
}

// query runs a predicate-pushdown, keyset-paginated read (POST /nodes/query) and
// returns one page of nodes plus the opaque cursor for the next page ("" on the
// last page). An unpushable predicate is a 400 from FacetQL, which do surfaces as
// an error — never a silently wrong or empty page.
func (c *fqClient) query(ctx context.Context, req fqQueryRequest) ([]fqNode, string, error) {
	data, _, err := c.do(ctx, http.MethodPost, "/nodes/query", req)
	if err != nil {
		return nil, "", err
	}
	return decodeQueryPage(data)
}

// fqCountRequest is the POST /nodes/count body (AGENT_LOG §4b): the same
// selection a query takes, asked for its cardinality. It deliberately has no
// order/limit/after — the engine refuses those rather than accept and drop them,
// because a caller handed a `limit` that did nothing would believe it had
// counted a page.
type fqCountRequest struct {
	Kind    string   `json:"kind"`
	Where   *ir.Expr `json:"where,omitempty"`
	ItemVar string   `json:"item_var"`
}

// fqCountByRequest is the POST /nodes/count_by body: one predicate answered for
// many pinned values of one field. `values` is the point of it — with a declared
// index over the grouped field each answer is the length of one key range and no
// record is read at all, whereas grouping the whole kind computes every answer to
// use a handful. Omitting `values` asks for the whole kind on purpose.
type fqCountByRequest struct {
	Kind    string   `json:"kind"`
	Where   *ir.Expr `json:"where,omitempty"`
	ItemVar string   `json:"item_var"`
	GroupBy string   `json:"group_by"`
	Values  []any    `json:"values,omitempty"`
}

// count runs a predicate-pushdown cardinality read (POST /nodes/count). An
// unpushable predicate is a 400, which surfaces as an error — never a zero, which
// would be indistinguishable from "nothing matched".
func (c *fqClient) count(ctx context.Context, req fqCountRequest) (int, error) {
	data, _, err := c.do(ctx, http.MethodPost, "/nodes/count", req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("facetql decode count response: %w", err)
	}
	return resp.Count, nil
}

// countBy runs a grouped cardinality read (POST /nodes/count_by) and returns one
// count per requested value, keyed by the value's text form. The engine answers
// every value that was asked about, zero included; the map is pre-filled anyway
// so a caller can never read an absent key as "the store forgot".
func (c *fqClient) countBy(ctx context.Context, req fqCountByRequest) (map[string]int, error) {
	data, _, err := c.do(ctx, http.MethodPost, "/nodes/count_by", req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Counts []struct {
			Value any `json:"value"`
			Count int `json:"count"`
		} `json:"counts"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("facetql decode count_by response: %w", err)
	}
	out := make(map[string]int, len(req.Values))
	for _, v := range req.Values {
		out[toStr(v)] = 0
	}
	for _, c := range resp.Counts {
		out[toStr(c.Value)] = c.Count
	}
	return out, nil
}

// decodeQueryPage parses a POST /nodes/query response — {"nodes":[...],"next":""}
// (AGENT_LOG §4b) — into a page of nodes and the opaque next cursor, returned
// unchanged (`next":""` = last page).
func decodeQueryPage(data []byte) ([]fqNode, string, error) {
	var resp struct {
		Nodes []fqNode `json:"nodes"`
		Next  string   `json:"next"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, "", fmt.Errorf("facetql decode query response: %w", err)
	}
	return resp.Nodes, resp.Next, nil
}

// errFQPrecondition is the one refusal a batch can earn without anything being
// wrong with it: a `set_if` expectation did not hold, so the whole batch was
// applied nowhere and another caller won the race.
//
// It is a sentinel rather than a status a caller reads because losing a race is
// an outcome, not a failure — the caller's next move is to do nothing, and the
// only way to distinguish it from "the request was bad" (400) or "the engine
// broke" (500) reliably is a value, never the message. FacetQL answers exactly
// 412 for it and nothing else does (facetql api/routes.rs, execute_transaction).
var errFQPrecondition = errors.New("facetql: a set_if precondition did not hold; nothing in the batch was applied")

// transaction submits a batch of ops atomically (POST /transaction).
//
// A 412 becomes errFQPrecondition, wrapping the typed fqHTTPError so the engine's
// own message and status stay reachable. Classification happens here, once,
// because "which refusal was that" is a property of the response, not of the
// caller: a caller that re-derived it would be deriving it from the status code
// this client already has, or worse from the prose.
func (c *fqClient) transaction(ctx context.Context, ops []fqTxOp) error {
	_, status, err := c.do(ctx, http.MethodPost, "/transaction", fqTxRequest{Operations: ops})
	if status == http.StatusPreconditionFailed {
		return fmt.Errorf("%w: %w", errFQPrecondition, err)
	}
	return err
}

// publish fans a payload out to every instance (POST /publish).
func (c *fqClient) publish(ctx context.Context, channel, payload string) error {
	_, _, err := c.do(ctx, http.MethodPost, "/publish", fqPublish{Channel: channel, Payload: payload})
	return err
}

// listIndexes returns every declared secondary index (GET /admin/indexes). The
// endpoint is admin-only, so a token without that role surfaces the 403 rather
// than an empty set that would read as "nothing is indexed". It arrives as a
// typed fqHTTPError, which is what lets Migrate tell "this identity may not
// reconcile indexes" from a reconcile that actually broke.
func (c *fqClient) listIndexes(ctx context.Context) ([]fqIndexDef, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/admin/indexes", nil)
	if err != nil {
		return nil, err
	}
	var defs []fqIndexDef
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("facetql decode index list: %w", err)
	}
	return defs, nil
}

// createIndex declares one index (POST /admin/indexes -> 201). Re-declaring an
// identical index is a successful no-op on the engine, which is what makes the
// reconcile safe to run on every boot. A 409 is the opposite case — a different
// index already holds that name, or another already covers that field — and is
// a contradiction, not a repeat: it is returned as an error carrying the engine's
// own message (do embeds the response body), never swallowed.
func (c *fqClient) createIndex(ctx context.Context, def fqIndexDef) error {
	_, _, err := c.do(ctx, http.MethodPost, "/admin/indexes", def)
	return err
}

// dropIndex removes a declared index by name (DELETE /admin/indexes/:name).
// Migrate never calls it: an index it did not declare may be an operator's, and
// dropping one is an operator decision, not a consequence of running an app.
func (c *fqClient) dropIndex(ctx context.Context, name string) error {
	_, _, err := c.do(ctx, http.MethodDelete, "/admin/indexes/"+url.PathEscape(name), nil)
	return err
}

// listReferences returns every declared referential rule (GET /admin/references).
// Admin-only, like the index endpoints and for the same reason — a referential
// action runs with the authority of the declaration, not of the caller, so an
// application able to declare its own could arrange for another owner's rows to
// be deleted (facetql storage/reference.rs).
func (c *fqClient) listReferences(ctx context.Context) ([]fqReferenceDef, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/admin/references", nil)
	if err != nil {
		return nil, err
	}
	var defs []fqReferenceDef
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("facetql decode reference list: %w", err)
	}
	return defs, nil
}

// createReference declares one referential rule (POST /admin/references -> 201).
// Re-declaring an identical one succeeds, which is what makes the reconcile safe
// on every boot. A different rule under the same name is a 409; a rule the
// declared access paths or the data already there cannot support is a 400 naming
// what is missing — both are contradictions, and both surface as errors carrying
// the engine's own message.
func (c *fqClient) createReference(ctx context.Context, def fqReferenceDef) error {
	_, _, err := c.do(ctx, http.MethodPost, "/admin/references", def)
	return err
}

// ping is the liveness probe (GET /).
func (c *fqClient) ping(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/", nil)
	return err
}
