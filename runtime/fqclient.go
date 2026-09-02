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
//	GET    /                   liveness probe
//
// Auth is a per-identity token sent as the `x-api-key` header on every request
// (AGENT_LOG §4b). See parseFacetQLURL for how the base URL and token are
// extracted from FACET_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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
}

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
	default:
		return nil, fmt.Errorf("facetql: unknown transaction op type %q", o.Type)
	}
}

// fqTxRequest is the POST /transaction body: { "operations": [ <op>, … ] }.
type fqTxRequest struct {
	Operations []fqTxOp `json:"operations"`
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

// do issues a request, marshaling body (if any) as JSON and returning the raw
// response body plus the HTTP status. A non-2xx status yields an error; the
// status is still returned so callers (e.g. deleteNode) can treat a 404 as a
// benign, idempotent outcome.
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
		return data, resp.StatusCode, fmt.Errorf("facetql %s %s: HTTP %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	return data, resp.StatusCode, nil
}

// upsert stores or replaces a node (POST /node).
func (c *fqClient) upsert(ctx context.Context, n fqNode) error {
	_, _, err := c.do(ctx, http.MethodPost, "/node", n)
	return err
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

// listKind fetches one page of nodes of a kind (GET /nodes?kind&limit&offset).
// FacetQL may answer with a bare array or a {"nodes":[...]} envelope; both are
// accepted.
func (c *fqClient) listKind(ctx context.Context, kind string, limit, offset int) ([]fqNode, error) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	data, _, err := c.do(ctx, http.MethodGet, "/nodes?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return decodeNodes(data)
}

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

// transaction submits a batch of ops atomically (POST /transaction).
func (c *fqClient) transaction(ctx context.Context, ops []fqTxOp) error {
	_, _, err := c.do(ctx, http.MethodPost, "/transaction", fqTxRequest{Operations: ops})
	return err
}

// publish fans a payload out to every instance (POST /publish).
func (c *fqClient) publish(ctx context.Context, channel, payload string) error {
	_, _, err := c.do(ctx, http.MethodPost, "/publish", fqPublish{Channel: channel, Payload: payload})
	return err
}

// ping is the liveness probe (GET /).
func (c *fqClient) ping(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodGet, "/", nil)
	return err
}
