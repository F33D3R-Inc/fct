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
//	GET    /events             cross-instance event fan-in (SSE; ?after= resumes)
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
	wire "facet/schema/generated"
)

// fqClient is a thin HTTP wrapper around a FacetQL instance.
type fqClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// fqNode used to be a single hand-written struct wearing two different real
// shapes: a write body (POST /node's flat x/y/z/q/public, always zero for v1)
// and a read/decode target (GET /node's real nested `coordinate`/`visibility`
// response). Only .Data/.Address were ever actually read back from a decoded
// node (traced every caller: byIdentity, LoadSession, FinishJob, RecentAudit,
// nodeRecord) — so the mismatch was never a live bug, but it meant fqNode was
// never really "the wire type for Node," just two informally different
// shapes sharing a name. Split for real, per the generated schema
// (schema/facetql_wire.fct):
//
//   - fqNodeWrite = the generated CreateNodeRequest (POST /node's real body
//     shape: flat x/y/z/q, public, edges, if_absent).
//   - fqNode = the generated wire.Node (GET /node's real nested response
//     shape) — used only as a decode target now.
type fqNodeWrite = wire.CreateNodeRequest
type fqNode = wire.Node

// newFQNodeWrite builds a write-shape request with every optional field at
// its safe, always-omitted-on-the-wire default — the same defaults rowNode's
// callers have always gotten (coordinates 0, not public, no edges), just
// spelled as real zero values of the generated type instead of an implicit
// struct literal.
func newFQNodeWrite(address, kind, data string) fqNodeWrite {
	return fqNodeWrite{Address: address, Kind: kind, Data: data, Edges: []wire.EdgeSpec{}}
}

// fqTxOp is one operation inside a POST /transaction batch: the fields fct's
// own callers actually construct (5 of the schema's 7 real TxOp variants —
// insert_edge/delete_edge have no caller here yet). Its shape used to be
// mirrored by hand into a per-variant anonymous struct inside MarshalJSON
// below; that marshaling is now delegated to the generated wire.TxOp (see
// toWire), which is itself generated from schema/facetql_wire.fct — so the
// wire contract this actually produces can no longer drift from the schema
// silently the way AGENT_LOG's §4b prose once did.
type fqTxOp struct {
	Type    string
	Address string
	Kind    string
	X       int64
	Y       int64
	Z       int64
	Q       int64
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

// toWire converts to the generated wire.TxOp, routing Where through the
// ir.Expr<->wire.Expr seam (wireseam.go) instead of relying on ir.Expr's own
// tags to happen to match, and Set through a plain json.Marshal (the
// generated type carries it as json.RawMessage, since a set_if's merged
// fields are arbitrary node data, not a typed shape the schema can name).
func (o fqTxOp) toWire() (wire.TxOp, error) {
	w := wire.TxOp{
		Type:    o.Type,
		Address: o.Address,
		Kind:    o.Kind,
		X:       o.X,
		Y:       o.Y,
		Z:       o.Z,
		Q:       o.Q,
		Data:    o.Data,
		Public:  o.Public,
		Field:   o.Field,
	}
	if o.Where != nil {
		we, err := wireExprFromIR(o.Where)
		if err != nil {
			return wire.TxOp{}, fmt.Errorf("facetql: %s on kind %q: %w", o.Type, o.Kind, err)
		}
		w.Where = we
	}
	if o.Type == "set_if" {
		if o.Expect.kind != fqExpectAtMost {
			return wire.TxOp{}, fmt.Errorf("facetql: set_if on %s.%s carries no expectation", o.Address, o.Field)
		}
		w.ExpectLe = o.Expect.bound
		// An assert-only compare-and-set is `{}`, never `null`: the engine's
		// field defaults to an empty map and would refuse a null.
		set := o.Set
		if set == nil {
			set = map[string]any{}
		}
		b, err := json.Marshal(set)
		if err != nil {
			return wire.TxOp{}, fmt.Errorf("facetql: encode set_if.set for %s.%s: %w", o.Address, o.Field, err)
		}
		w.Set = b
	}
	return w, nil
}

// MarshalJSON delegates the actual wire encoding to the generated
// wire.TxOp's own MarshalJSON (which emits the exact per-type shape — no
// stray zero fields on delete/clear — matching schema/facetql_wire.fct's
// `message TxOp`), after routing this op's fields through toWire. The only
// thing fqTxOp still owns is which of the schema's 7 variants fct's own
// callers actually use (5 today) and the client-side "set_if must carry
// exactly one expectation" guarantee, which the generated marshal has no way
// to know is a requirement (a body with none would otherwise decode as a
// silently-wrong 400 from the server instead of failing at construction).
func (o fqTxOp) MarshalJSON() ([]byte, error) {
	w, err := o.toWire()
	if err != nil {
		return nil, err
	}
	return json.Marshal(w)
}

// fqTxRequest is the POST /transaction body: { "operations": [ <op>, … ] }.
type fqTxRequest struct {
	Operations []fqTxOp `json:"operations"`
}

// fqIndexDef is one declared secondary index over a `data` field — the wire
// shape of both /admin/indexes bodies, now the generated CreateIndexRequest
// (schema/facetql_wire.fct) directly: Name/Kind/Field/Unique match field-for-
// field (Name is the index's identity and also its filename on the engine,
// which is why it has a restricted alphabet — see fqIndexName; Unique refuses
// a write that would give two nodes of the kind the same value for the
// field). The generated type also carries an optional `Mode`, unused by
// every real caller here, which simply stays unset. GET /admin/indexes's
// real response is a separate, richer shape (IndexInfo) the schema doesn't
// cover yet (SCHEMA_IDL_SCOPE.md, "known remaining gap"); decoding it into
// this request-shaped type still works because every field fct has ever
// actually read (Name/Kind/Field/Unique) is a subset of it, exactly as the
// old hand-written fqIndexDef only ever captured that same subset.
type fqIndexDef = wire.CreateIndexRequest

// fqReferenceDef is one declared referential rule — the wire shape of both
// /admin/references bodies, now the generated CreateReferenceRequest
// directly. Kind.Field is the referencing (child) side, which holds the
// parent's key and whose index serves the lookup; ParentKind is the
// referenced side and ParentField the value on it that the child's field
// matches. OnDelete is a wire.ReferentialAction now (was a bare string);
// fct declares only "cascade" (an untyped string constant, so every existing
// call site assigns to it unchanged), because that is the one rule an fct
// relation has ever meant (ir.Field: "a foreign key with ON DELETE CASCADE")
// and the one pgStore's DDL emits. Same GET-response caveat as fqIndexDef.
type fqReferenceDef = wire.CreateReferenceRequest

// fqPublish is the POST /publish body (replaces Postgres LISTEN/NOTIFY).
//
// Deliberately NOT cut over to the generated wire.PublishRequest: that type
// is {payload} only, because FacetQL's own /publish audience is identity-
// scoped (Audience::Owner/Everyone in database.rs), not channel-scoped — see
// this session's investigation of the "channel field drift", which found the
// field is real but consumed one layer up, by fabric's frontdoor
// (fabric-facetql/src/frontdoor/plan.rs), which reads `channel` straight out
// of the raw request body to route a publish to the correct backend shard by
// keyspace. A fabric-fronted deployment depends on this client still sending
// it — cutting fqPublish over to the generated (channel-less) type would
// silently break shard routing for exactly the deployments that need it
// most. Facetql itself accepts and ignores the field either way, so keeping
// it costs facetql nothing and fabric needs it kept.
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
func (c *fqClient) upsert(ctx context.Context, n fqNodeWrite) error {
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
//
// IfAbsent is a real field on the generated request type now (it used to be a
// hand-written wrapper struct embedding fqNode); setting it directly is the
// whole change.
func (c *fqClient) createIfAbsent(ctx context.Context, n fqNodeWrite) (created bool, err error) {
	n.IfAbsent = true
	_, status, err := c.do(ctx, http.MethodPost, "/node", n)
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

// fqQueryRequest is the POST /nodes/query body: the pushed-down read filter.
// `Where` is fct's own compiler/runtime ir.Expr; MarshalJSON routes it
// through the ir.Expr<->wire.Expr seam (wireseam.go) rather than relying on
// ir.Expr's own tags to happen to still match FacetQL's real predicate.rs
// shape. `after` is the opaque keyset cursor from the previous page; it is
// omitted on the first page (empty = first page).
type fqQueryRequest struct {
	Kind    string
	Where   *ir.Expr
	ItemVar string
	Order   string
	Desc    bool
	Limit   int
	After   string
}

func (q fqQueryRequest) MarshalJSON() ([]byte, error) {
	where, err := wireExprFromIR(q.Where)
	if err != nil {
		return nil, fmt.Errorf("facetql: query on kind %q: %w", q.Kind, err)
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Where   *wire.Expr `json:"where,omitempty"`
		ItemVar string     `json:"item_var"`
		Order   string     `json:"order"`
		Desc    bool       `json:"desc"`
		Limit   int        `json:"limit"`
		After   string     `json:"after,omitempty"`
	}{q.Kind, where, q.ItemVar, q.Order, q.Desc, q.Limit, q.After})
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
	Kind    string
	Where   *ir.Expr
	ItemVar string
}

func (q fqCountRequest) MarshalJSON() ([]byte, error) {
	where, err := wireExprFromIR(q.Where)
	if err != nil {
		return nil, fmt.Errorf("facetql: count on kind %q: %w", q.Kind, err)
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Where   *wire.Expr `json:"where,omitempty"`
		ItemVar string     `json:"item_var"`
	}{q.Kind, where, q.ItemVar})
}

// fqCountByRequest is the POST /nodes/count_by body: one predicate answered for
// many pinned values of one field. `values` is the point of it — with a declared
// index over the grouped field each answer is the length of one key range and no
// record is read at all, whereas grouping the whole kind computes every answer to
// use a handful. Omitting `values` asks for the whole kind on purpose.
type fqCountByRequest struct {
	Kind    string
	Where   *ir.Expr
	ItemVar string
	GroupBy string
	Values  []any
}

func (q fqCountByRequest) MarshalJSON() ([]byte, error) {
	where, err := wireExprFromIR(q.Where)
	if err != nil {
		return nil, fmt.Errorf("facetql: count_by on kind %q: %w", q.Kind, err)
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Where   *wire.Expr `json:"where,omitempty"`
		ItemVar string     `json:"item_var"`
		GroupBy string     `json:"group_by"`
		Values  []any      `json:"values,omitempty"`
	}{q.Kind, where, q.ItemVar, q.GroupBy, q.Values})
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

// fqAggregateRequest is the POST /nodes/aggregate body: the same selection a
// count takes, plus which reduction to perform over which `data` field. The
// engine refuses a function/field pair it cannot answer — a `sum` with no field,
// or a `count` with one — before it reads a row, so a malformed ask is a 400
// rather than a number that looks plausible.
type fqAggregateRequest struct {
	Kind    string
	Where   *ir.Expr
	ItemVar string
	Func    string
	Field   string
}

func (q fqAggregateRequest) MarshalJSON() ([]byte, error) {
	where, err := wireExprFromIR(q.Where)
	if err != nil {
		return nil, fmt.Errorf("facetql: aggregate on kind %q: %w", q.Kind, err)
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Where   *wire.Expr `json:"where,omitempty"`
		ItemVar string     `json:"item_var"`
		Func    string     `json:"func"`
		Field   string     `json:"field,omitempty"`
	}{q.Kind, where, q.ItemVar, q.Func, q.Field})
}

// fqAggregateByRequest is the POST /nodes/aggregate_by body — the grouped form,
// with the same `values` shortcut fqCountByRequest documents.
type fqAggregateByRequest struct {
	Kind    string
	Where   *ir.Expr
	ItemVar string
	GroupBy string
	Values  []any
	Func    string
	Field   string
}

func (q fqAggregateByRequest) MarshalJSON() ([]byte, error) {
	where, err := wireExprFromIR(q.Where)
	if err != nil {
		return nil, fmt.Errorf("facetql: aggregate_by on kind %q: %w", q.Kind, err)
	}
	return json.Marshal(struct {
		Kind    string     `json:"kind"`
		Where   *wire.Expr `json:"where,omitempty"`
		ItemVar string     `json:"item_var"`
		GroupBy string     `json:"group_by"`
		Values  []any      `json:"values,omitempty"`
		Func    string     `json:"func"`
		Field   string     `json:"field,omitempty"`
	}{q.Kind, where, q.ItemVar, q.GroupBy, q.Values, q.Func, q.Field})
}

// aggregate runs a pushed-down reduction (POST /nodes/aggregate).
//
// FacetQL answers `null` for a `min`/`max` over no rows, because at that layer
// there genuinely is no smallest value. This language has no such hole — its
// reducer returns 0 for every empty reduction — so the null is converted here,
// at the seam that owns the difference. `toInt(nil)` is 0, which is why the
// conversion is the same call the rest of the runtime already makes rather than
// a special case.
func (c *fqClient) aggregate(ctx context.Context, req fqAggregateRequest) (int, error) {
	data, _, err := c.do(ctx, http.MethodPost, "/nodes/aggregate", req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("facetql decode aggregate response: %w", err)
	}
	return toInt(resp.Result), nil
}

// aggregateBy runs a grouped reduction (POST /nodes/aggregate_by) and returns one
// value per requested key. The engine answers every value that was asked about;
// the map is pre-filled with zeroes anyway so a caller can never read an absent
// key as "the store forgot".
func (c *fqClient) aggregateBy(ctx context.Context, req fqAggregateByRequest) (map[string]int, error) {
	data, _, err := c.do(ctx, http.MethodPost, "/nodes/aggregate_by", req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Groups []struct {
			Value  any `json:"value"`
			Result any `json:"result"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("facetql decode aggregate_by response: %w", err)
	}
	out := make(map[string]int, len(req.Values))
	for _, v := range req.Values {
		out[toStr(v)] = 0
	}
	for _, g := range resp.Groups {
		out[toStr(g.Value)] = toInt(g.Result)
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

// eventsHTTPClient is dedicated to GET /events and carries no timeout: it is a
// long-lived SSE stream by design (FacetQL's own routing deliberately keeps it
// outside the request-timeout layer — a stream severed every N seconds is not
// a guarded stream, it is a broken one), and fqClient.http's 15s timeout would
// sever it on a schedule instead of on an actual disconnect.
var eventsHTTPClient = &http.Client{}

// errEventsResumeGone is returned by streamEvents when FacetQL answers 410 to
// a resume — the requested position aged out of the engine's retention ring.
// There is nothing to resume from at that point; the caller's own choice is
// whether to reconnect from the live edge (accepting the gap) or give up.
var errEventsResumeGone = errors.New("facetql: resume position for GET /events is no longer available")

// streamEvents opens GET /events, resuming after the given sequence when it is
// nonzero, and returns the raw response body for the caller to read as SSE
// frames (id:/data: lines, blank-line-terminated) — decoding those frames is
// the caller's concern (cluster.go's clusterEvent), not this client's. The
// caller must close the returned body when done with it.
func (c *fqClient) streamEvents(ctx context.Context, after uint64) (io.ReadCloser, error) {
	path := "/events"
	if after > 0 {
		path = fmt.Sprintf("/events?after=%d", after)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("facetql build request GET %s: %w", path, err)
	}
	if c.token != "" {
		req.Header.Set("x-api-key", c.token)
	}
	resp, err := eventsHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("facetql GET %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusGone {
		resp.Body.Close()
		return nil, errEventsResumeGone
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &fqHTTPError{
			Method: http.MethodGet, Path: path,
			Status: resp.StatusCode, StatusText: resp.Status,
			Body: strings.TrimSpace(string(data)),
		}
	}
	return resp.Body, nil
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
