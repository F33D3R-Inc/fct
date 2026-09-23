package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"facet/internal/ir"
)

// `contract "/api/v2/contract"` (ir.ContractRoute): the app serves its own
// published contract — the same document GET /api/_contract answers — under
// a path of its choosing, together with
//
//	<path>/version  its version, the data schema version, when it was generated
//	<path>/history  every distinct version this deployment has served, newest first
//	<path>/diff     what changed between two of them (?from=&to=), computed here
//
// Each process records the document it generated at boot, under its
// version, in the reserved FacetContractVersion table (the same durable path
// a declared entity takes), so the history and the diff survive deploys. The
// data schema version (x-schema-version) is kept in the same rows: the
// ordinal of the entity schema this process runs among every distinct one
// the deployment has recorded, so it rises exactly when the data schema
// changes — the fct runtime's migration head.

const contractEntity = "FacetContractVersion"

func init() { reservedEntities[contractEntity] = true }

// injectContractEntities adds the contract history table when the app
// declares a `contract` route (see injectEnterpriseEntities for the pattern).
func injectContractEntities(graph *ir.IR) {
	if graph.Contract == nil {
		return
	}
	for _, e := range graph.Entities {
		if e.Name == contractEntity {
			return
		}
	}
	graph.Entities = append(graph.Entities, ir.Entity{Name: contractEntity, Fields: []ir.Field{
		{Name: "id", Type: "int"},
		{Name: "version", Type: "text", Index: true},
		{Name: "schema_version", Type: "int"},
		{Name: "schema_hash", Type: "text"},
		{Name: "first_seen", Type: "int"},
		{Name: "document", Type: "text"},
	}})
}

// entitySchemaHash identifies the data schema this process runs: every
// durable entity's fields and their types.
func entitySchemaHash(g *ir.IR) string {
	type col struct{ Name, Type, Ref string }
	shape := map[string][]col{}
	for _, e := range g.Entities {
		if e.Ephemeral || isReservedEntity(e.Name) {
			continue
		}
		var cols []col
		for _, f := range e.Fields {
			cols = append(cols, col{f.Name, f.Type, f.Ref})
		}
		shape[e.Name] = cols
	}
	raw, _ := json.Marshal(shape)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// contractRows is the recorded history, as records (caller holds s.mu).
func (s *Server) contractRows() []record {
	var out []record
	for _, r := range s.entities[contractEntity] {
		if m, ok := r.(record); ok {
			out = append(out, m)
		}
	}
	return out
}

// recordContractVersion fixes this process's schema version from the
// recorded history and records the document it serves, once per version.
func (s *Server) recordContractVersion() {
	if s.ir.Contract == nil {
		return
	}
	hash := entitySchemaHash(s.ir)
	s.mu.Lock()
	rows := s.contractRows()
	sv, max := 0, 0
	for _, r := range rows {
		n := toInt(r["schema_version"])
		if n > max {
			max = n
		}
		if toStr(r["schema_hash"]) == hash && sv == 0 {
			sv = n
		}
	}
	if sv == 0 {
		sv = max + 1
	}
	s.schemaVersion = sv
	s.mu.Unlock()

	version := s.contractVersion()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.contractRows() {
		if toStr(r["version"]) == version {
			return
		}
	}
	s.insertReserved(contractEntity, record{
		"version": version, "schema_version": sv, "schema_hash": hash,
		"first_seen": int(s.contractGenerated.Unix()), "document": string(s.contractRaw),
	})
}

// contractRoute wraps one contract endpoint: GET only, metered by the
// declaration's rate class.
func (s *Server) contractRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if lim := s.rateClass(s.ir.Contract.Rate); lim != nil && !lim.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			apiError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		h(w, r)
	}
}

// writeConditional answers a JSON body with a strong ETag over its bytes,
// 304 when the client already holds it.
func writeConditional(w http.ResponseWriter, r *http.Request, body []byte, cacheControl string) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", cacheControl)
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

func (s *Server) handleContractDocument(w http.ResponseWriter, r *http.Request) {
	s.contractDocument()
	writeConditional(w, r, s.contractRaw, "public, max-age=60")
}

func (s *Server) handleContractVersion(w http.ResponseWriter, r *http.Request) {
	s.contractDocument()
	body, _ := json.Marshal(map[string]any{
		"version":        s.contractVersion(),
		"schema_version": s.schemaVersion,
		"generated_at":   s.contractGenerated.Format(time.RFC3339),
	})
	writeConditional(w, r, body, "private, no-cache")
}

func (s *Server) handleContractHistory(w http.ResponseWriter, r *http.Request) {
	current := s.contractVersion()
	type entry struct {
		Version       string `json:"version"`
		SchemaVersion int    `json:"schema_version"`
		FirstSeen     string `json:"first_seen"`
		Current       bool   `json:"current"`
		at            int
	}
	var versions []entry
	haveCurrent := false
	s.mu.Lock()
	for _, row := range s.contractRows() {
		v := toStr(row["version"])
		at := toInt(row["first_seen"])
		versions = append(versions, entry{Version: v, SchemaVersion: toInt(row["schema_version"]),
			FirstSeen: time.Unix(int64(at), 0).UTC().Format(time.RFC3339), Current: v == current, at: at})
		haveCurrent = haveCurrent || v == current
	}
	s.mu.Unlock()
	if !haveCurrent { // always listed, recorded or not
		versions = append(versions, entry{Version: current, SchemaVersion: s.schemaVersion,
			FirstSeen: s.contractGenerated.Format(time.RFC3339), Current: true, at: int(s.contractGenerated.Unix())})
	}
	sort.SliceStable(versions, func(i, j int) bool {
		if versions[i].at != versions[j].at {
			return versions[i].at > versions[j].at
		}
		return versions[i].Version < versions[j].Version
	})
	body, _ := json.Marshal(map[string]any{
		"current":        current,
		"schema_version": s.schemaVersion,
		"server_time":    time.Now().UTC().Format(time.RFC3339),
		"versions":       versions,
	})
	writeConditional(w, r, body, "private, no-cache")
}

func (s *Server) handleContractDiff(w http.ResponseWriter, r *http.Request) {
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" {
		apiError(w, http.StatusBadRequest, "name the version to diff from (?from=), or `current`")
		return
	}
	if to == "" {
		to = "current"
	}
	fromDoc, ok := s.contractDocumentByVersion(w, from)
	if !ok {
		return
	}
	toDoc, ok := s.contractDocumentByVersion(w, to)
	if !ok {
		return
	}
	diff, err := contractDiff(fromDoc, toDoc)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "the recorded contract could not be read")
		return
	}
	body, _ := json.Marshal(diff)
	writeConditional(w, r, body, "private, no-cache")
}

// contractDocumentByVersion is the recorded document for a version, or this
// process's own for `current`; it answers the refusal itself.
func (s *Server) contractDocumentByVersion(w http.ResponseWriter, version string) ([]byte, bool) {
	s.contractDocument()
	if version == "current" || version == s.contractVersion() {
		return s.contractRaw, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.contractRows() {
		if toStr(row["version"]) == version {
			return []byte(toStr(row["document"])), true
		}
	}
	apiError(w, http.StatusNotFound, "this deployment never served contract version "+version)
	return nil, false
}

// ── The contract routes' own schemas and operations ──────────────────────────

// contractSchemas are the components the contract routes answer with.
func contractSchemas() map[string]any {
	str, boolean, integer := strSchema(), map[string]any{"type": "boolean"}, map[string]any{"type": "integer"}
	dt := map[string]any{"type": "string", "format": "date-time"}
	anyJSON := map[string]any{"description": "Any JSON value."}
	strs := map[string]any{"type": "array", "items": str}
	list := func(name string) map[string]any { return map[string]any{"type": "array", "items": ref(name)} }
	mapOfAny := map[string]any{"type": "object", "additionalProperties": anyJSON}
	return map[string]any{
		"APIContractDocumentDTO": objSchema(map[string]any{
			"openapi": str, "info": ref("APIContractInfoDTO"), "components": ref("APIContractComponentsDTO"),
			"paths":             map[string]any{"type": "object", "additionalProperties": mapOfAny},
			"security":          map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": strs}},
			"x-schema-version":  integer,
			"x-stream-events":   list("APIContractStreamEventDTO"),
			"x-mutation-events": list("APIContractMutationEventDTO"),
			"x-facet-catalog":   list("FacetCatalogBindingDTO"),
		}, "components", "info", "openapi", "paths", "security", "x-facet-catalog", "x-mutation-events", "x-schema-version", "x-stream-events"),
		"APIContractInfoDTO":       objSchema(map[string]any{"title": str, "version": str, "description": str}, "description", "title", "version"),
		"APIContractComponentsDTO": objSchema(map[string]any{"schemas": mapOfAny, "securitySchemes": mapOfAny}, "schemas", "securitySchemes"),
		"APIContractStreamEventDTO": objSchema(map[string]any{"name": str, "stream": str, "summary": str, "x-since": str, "payload": anyJSON},
			"name", "stream", "summary", "x-since"),
		"APIContractMutationEventDTO": objSchema(map[string]any{"event_type": str, "summary": str, "x-since": str,
			"fields": list("APIContractMutationFieldDTO"), "body": anyJSON, "result": anyJSON}, "event_type", "fields", "summary", "x-since"),
		"APIContractMutationFieldDTO": objSchema(map[string]any{"name": str, "type": str, "required": boolean, "description": str, "enum": strs},
			"name", "required", "type"),
		"FacetCatalogBindingDTO": objSchema(map[string]any{"name": str, "facet_id": str, "layer": str, "routes": strs, "stream_events": strs},
			"facet_id", "layer", "name", "routes", "stream_events"),
		"APIContractVersionDTO": objSchema(map[string]any{"version": str, "schema_version": integer, "generated_at": dt},
			"generated_at", "schema_version", "version"),
		"APIContractHistoryDTO": objSchema(map[string]any{"current": str, "schema_version": integer, "server_time": dt,
			"versions": list("APIContractHistoryEntryDTO")}, "current", "schema_version", "server_time", "versions"),
		"APIContractHistoryEntryDTO": objSchema(map[string]any{"version": str, "schema_version": integer, "first_seen": dt, "current": boolean},
			"current", "first_seen", "schema_version", "version"),
		"ContractDiffDTO": objSchema(map[string]any{
			"from": ref("ContractDiffSideDTO"), "to": ref("ContractDiffSideDTO"), "identical": boolean,
			"routes_added": strs, "routes_removed": strs, "routes_changed": list("ContractRouteDiffDTO"),
			"stream_events_added": strs, "stream_events_removed": strs, "stream_events_changed": list("ContractEventDiffDTO"),
			"mutations_added": strs, "mutations_removed": strs, "mutations_changed": list("ContractMutationDiffDTO"),
			"schemas_added": strs, "schemas_removed": strs, "schemas_changed": list("ContractSchemaDiffDTO"),
		}, "from", "identical", "mutations_added", "mutations_changed", "mutations_removed", "routes_added", "routes_changed",
			"routes_removed", "schemas_added", "schemas_changed", "schemas_removed", "stream_events_added", "stream_events_changed",
			"stream_events_removed", "to"),
		"ContractDiffSideDTO": objSchema(map[string]any{"version": str, "schema_version": integer}, "schema_version", "version"),
		"ContractRouteDiffDTO": objSchema(map[string]any{"route": str, "params": ref("ContractFieldsDiffDTO"), "request": ref("ContractFieldsDiffDTO"),
			"responses": list("ContractResponseDiffDTO"), "deprecated": boolean, "deprecated_changed": boolean},
			"deprecated", "deprecated_changed", "params", "request", "responses", "route"),
		"ContractResponseDiffDTO": objSchema(map[string]any{"status": str, "added": boolean, "removed": boolean, "fields": ref("ContractFieldsDiffDTO")},
			"added", "fields", "removed", "status"),
		"ContractEventDiffDTO": objSchema(map[string]any{"stream": str, "name": str, "payload": ref("ContractFieldsDiffDTO")}, "name", "payload", "stream"),
		"ContractMutationDiffDTO": objSchema(map[string]any{"event_type": str, "fields": ref("ContractFieldsDiffDTO"),
			"body": ref("ContractFieldsDiffDTO"), "result": ref("ContractFieldsDiffDTO")}, "body", "event_type", "fields", "result"),
		"ContractSchemaDiffDTO": objSchema(map[string]any{"name": str, "fields": ref("ContractFieldsDiffDTO")}, "fields", "name"),
		"ContractFieldsDiffDTO": objSchema(map[string]any{"added": list("ContractFieldDTO"), "removed": list("ContractFieldDTO"),
			"type_changed": list("ContractFieldChangeDTO"), "required_changed": list("ContractFieldChangeDTO")},
			"added", "removed", "required_changed", "type_changed"),
		"ContractFieldDTO":       objSchema(map[string]any{"name": str, "type": str, "required": boolean}, "name", "required", "type"),
		"ContractFieldChangeDTO": objSchema(map[string]any{"name": str, "from": str, "to": str}, "from", "name", "to"),
	}
}

// contractOperations are the contract routes' entries in paths.
func contractOperations(c *ir.ContractRoute, errResp func(string) map[string]any) map[string]map[string]any {
	ok := func(schema string) map[string]any {
		return map[string]any{"description": "OK", "content": map[string]any{"application/json": map[string]any{"schema": ref(schema)}}}
	}
	op := func(path, summary, schema string, params []map[string]any, errs map[int]string) map[string]any {
		responses := map[string]any{"200": ok(schema), "304": map[string]any{"description": "Not Modified"}}
		for code, desc := range errs {
			responses[itoa(code)] = errResp(desc)
		}
		o := map[string]any{
			"operationId": operationIDFor(http.MethodGet, path), "summary": summary,
			"x-auth": "none", "x-since": c.Since, "x-conditional-get": true,
			"security": []map[string]any{}, "responses": responses,
		}
		if len(params) > 0 {
			o["parameters"] = params
		}
		if c.Rate != "" {
			o["x-rate-limit"] = rateLimitDoc(c.Rate)
			responses["429"] = errResp("Too Many Requests")
		}
		return o
	}
	q := func(name, desc string, required bool) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": required, "schema": strSchema(), "description": desc}
	}
	return map[string]map[string]any{
		c.Path: op(c.Path, "This contract: an OpenAPI document of every declared route, stream event and mutation.", "APIContractDocumentDTO",
			[]map[string]any{{"name": "If-None-Match", "in": "header", "required": false, "schema": strSchema(), "description": "The ETag last received; a match is answered 304."}}, nil),
		c.Path + "/version": op(c.Path+"/version", "The contract's version, the data schema version, and when this process generated it.", "APIContractVersionDTO", nil, nil),
		c.Path + "/history": op(c.Path+"/history", "Every distinct contract this deployment has served, newest first.", "APIContractHistoryDTO", nil, nil),
		c.Path + "/diff": op(c.Path+"/diff", "What changed between two contract versions, computed on the server.", "ContractDiffDTO",
			[]map[string]any{q("from", "The older version (its info.version), or `current`.", true), q("to", "The newer version, or `current`; default current.", false)},
			map[int]string{400: "Bad Request", 404: "Not Found"}),
	}
}

// ── Diffing two documents ────────────────────────────────────────────────────

type contractField struct {
	Type     string
	Required bool
}

type contractFields map[string]contractField

type contractOp struct {
	Params     contractFields
	Request    contractFields
	Responses  map[string]contractFields
	Deprecated bool
}

type contractMutation struct {
	Fields, Body, Result contractFields
}

// contractShape is a document reduced to what a client can be broken by.
type contractShape struct {
	Version   string
	Schema    int64
	Routes    map[string]contractOp
	Streams   map[string]contractFields
	Mutations map[string]contractMutation
	Schemas   map[string]contractFields
}

// contractDocumentJSON is the subset of the document the diff reads; it
// decodes generically so a document an older runtime wrote still diffs.
type contractDocumentJSON struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Schema     any                                   `json:"x-schema-version"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]map[string]any `json:"schemas"`
	} `json:"components"`
	Streams []struct {
		Name    string         `json:"name"`
		Stream  string         `json:"stream"`
		Payload map[string]any `json:"payload"`
	} `json:"x-stream-events"`
	Mutations []struct {
		EventType string `json:"event_type"`
		Fields    []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"fields"`
		Body   map[string]any `json:"body"`
		Result map[string]any `json:"result"`
	} `json:"x-mutation-events"`
}

type contractOperationJSON struct {
	Deprecated bool `json:"deprecated"`
	Parameters []struct {
		Name     string         `json:"name"`
		In       string         `json:"in"`
		Required bool           `json:"required"`
		Schema   map[string]any `json:"schema"`
	} `json:"parameters"`
	RequestBody struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]struct {
			Schema map[string]any `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

func contractShapeOf(body []byte) (*contractShape, error) {
	var doc contractDocumentJSON
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	shape := &contractShape{
		Version: doc.Info.Version, Routes: map[string]contractOp{}, Streams: map[string]contractFields{},
		Mutations: map[string]contractMutation{}, Schemas: map[string]contractFields{},
	}
	if n, ok := doc.Schema.(float64); ok {
		shape.Schema = int64(n)
	}
	resolve := func(schema map[string]any) contractFields { return schemaFields(schema, doc.Components.Schemas) }
	for name, schema := range doc.Components.Schemas {
		shape.Schemas[name] = resolve(schema)
	}
	for path, methods := range doc.Paths {
		for method, raw := range methods {
			var op contractOperationJSON
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", method, path, err)
			}
			entry := contractOp{Params: contractFields{}, Request: contractFields{}, Responses: map[string]contractFields{}, Deprecated: op.Deprecated}
			for _, p := range op.Parameters {
				entry.Params[p.Name+"@"+p.In] = contractField{Type: schemaTypeName(p.Schema), Required: p.Required}
			}
			for _, c := range op.RequestBody.Content {
				for k, v := range resolve(c.Schema) {
					entry.Request[k] = v
				}
			}
			for status, resp := range op.Responses {
				fields := contractFields{}
				for _, c := range resp.Content {
					for k, v := range resolve(c.Schema) {
						fields[k] = v
					}
				}
				entry.Responses[status] = fields
			}
			shape.Routes[strings.ToUpper(method)+" "+path] = entry
		}
	}
	for _, e := range doc.Streams {
		shape.Streams[e.Stream+" "+e.Name] = resolve(e.Payload)
	}
	for _, m := range doc.Mutations {
		entry := contractMutation{Fields: contractFields{}, Body: resolve(m.Body), Result: resolve(m.Result)}
		for _, f := range m.Fields {
			entry.Fields[f.Name] = contractField{Type: f.Type, Required: f.Required}
		}
		shape.Mutations[m.EventType] = entry
	}
	return shape, nil
}

// schemaFields is the field set a schema describes: a component's
// properties (through $ref and a nullable oneOf), or an inline object's.
func schemaFields(schema map[string]any, components map[string]map[string]any) contractFields {
	out := contractFields{}
	if schema == nil {
		return out
	}
	if name := schemaRefName(schema); name != "" {
		schema = components[name]
		if schema == nil {
			return out
		}
	}
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	for name, raw := range props {
		fs, _ := raw.(map[string]any)
		out[name] = contractField{Type: schemaTypeName(fs), Required: required[name]}
	}
	return out
}

// schemaRefName is the component a schema references, directly or as the
// non-null arm of a nullable oneOf; "" otherwise.
func schemaRefName(schema map[string]any) string {
	if r, ok := schema["$ref"].(string); ok {
		return strings.TrimPrefix(r, "#/components/schemas/")
	}
	if arms, ok := schema["oneOf"].([]any); ok {
		for _, arm := range arms {
			if m, ok := arm.(map[string]any); ok {
				if name := schemaRefName(m); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// schemaTypeName writes a schema's type the way the diff reports it: the
// component name, or the JSON type, with array<T>, map<T>, |null for
// nullable and (format) for a formatted string.
func schemaTypeName(schema map[string]any) string {
	if schema == nil {
		return "any"
	}
	if arms, ok := schema["oneOf"].([]any); ok {
		name := schemaRefName(schema)
		if name == "" {
			name = "any"
		}
		for _, arm := range arms {
			if m, ok := arm.(map[string]any); ok && m["type"] == "null" {
				return name + "|null"
			}
		}
		return name
	}
	if name := schemaRefName(schema); name != "" {
		return name
	}
	base, nullable := "any", false
	switch t := schema["type"].(type) {
	case string:
		base = t
	case []any:
		for _, v := range t {
			if s, _ := v.(string); s == "null" {
				nullable = true
			} else {
				base = s
			}
		}
	}
	switch base {
	case "array":
		items, _ := schema["items"].(map[string]any)
		base = "array<" + schemaTypeName(items) + ">"
	case "object":
		if ap, ok := schema["additionalProperties"].(map[string]any); ok {
			base = "map<" + schemaTypeName(ap) + ">"
		}
	case "string":
		if f, ok := schema["format"].(string); ok && f != "" {
			base = "string(" + f + ")"
		}
	}
	if nullable {
		base += "|null"
	}
	return base
}

type contractFieldDTO struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type contractFieldChangeDTO struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

type contractFieldsDiffDTO struct {
	Added           []contractFieldDTO       `json:"added"`
	Removed         []contractFieldDTO       `json:"removed"`
	TypeChanged     []contractFieldChangeDTO `json:"type_changed"`
	RequiredChanged []contractFieldChangeDTO `json:"required_changed"`
}

func (d contractFieldsDiffDTO) changed() bool {
	return len(d.Added)+len(d.Removed)+len(d.TypeChanged)+len(d.RequiredChanged) > 0
}

type contractDiffSideDTO struct {
	Version       string `json:"version"`
	SchemaVersion int64  `json:"schema_version"`
}

type contractResponseDiffDTO struct {
	Status  string                `json:"status"`
	Added   bool                  `json:"added"`
	Removed bool                  `json:"removed"`
	Fields  contractFieldsDiffDTO `json:"fields"`
}

type contractRouteDiffDTO struct {
	Route             string                    `json:"route"`
	Params            contractFieldsDiffDTO     `json:"params"`
	Request           contractFieldsDiffDTO     `json:"request"`
	Responses         []contractResponseDiffDTO `json:"responses"`
	Deprecated        bool                      `json:"deprecated"`
	DeprecatedChanged bool                      `json:"deprecated_changed"`
}

type contractEventDiffDTO struct {
	Stream  string                `json:"stream"`
	Name    string                `json:"name"`
	Payload contractFieldsDiffDTO `json:"payload"`
}

type contractMutationDiffDTO struct {
	EventType string                `json:"event_type"`
	Fields    contractFieldsDiffDTO `json:"fields"`
	Body      contractFieldsDiffDTO `json:"body"`
	Result    contractFieldsDiffDTO `json:"result"`
}

type contractSchemaDiffDTO struct {
	Name   string                `json:"name"`
	Fields contractFieldsDiffDTO `json:"fields"`
}

type contractDiffDTO struct {
	From                contractDiffSideDTO       `json:"from"`
	To                  contractDiffSideDTO       `json:"to"`
	Identical           bool                      `json:"identical"`
	RoutesAdded         []string                  `json:"routes_added"`
	RoutesRemoved       []string                  `json:"routes_removed"`
	RoutesChanged       []contractRouteDiffDTO    `json:"routes_changed"`
	StreamEventsAdded   []string                  `json:"stream_events_added"`
	StreamEventsRemoved []string                  `json:"stream_events_removed"`
	StreamEventsChanged []contractEventDiffDTO    `json:"stream_events_changed"`
	MutationsAdded      []string                  `json:"mutations_added"`
	MutationsRemoved    []string                  `json:"mutations_removed"`
	MutationsChanged    []contractMutationDiffDTO `json:"mutations_changed"`
	SchemasAdded        []string                  `json:"schemas_added"`
	SchemasRemoved      []string                  `json:"schemas_removed"`
	SchemasChanged      []contractSchemaDiffDTO   `json:"schemas_changed"`
}

// contractDiff computes what changed from one document to another; every
// list is sorted, so two reads of the same pair are byte-equal.
func contractDiff(fromBody, toBody []byte) (*contractDiffDTO, error) {
	from, err := contractShapeOf(fromBody)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	to, err := contractShapeOf(toBody)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	out := &contractDiffDTO{
		From:        contractDiffSideDTO{Version: from.Version, SchemaVersion: from.Schema},
		To:          contractDiffSideDTO{Version: to.Version, SchemaVersion: to.Schema},
		RoutesAdded: []string{}, RoutesRemoved: []string{}, RoutesChanged: []contractRouteDiffDTO{},
		StreamEventsAdded: []string{}, StreamEventsRemoved: []string{}, StreamEventsChanged: []contractEventDiffDTO{},
		MutationsAdded: []string{}, MutationsRemoved: []string{}, MutationsChanged: []contractMutationDiffDTO{},
		SchemasAdded: []string{}, SchemasRemoved: []string{}, SchemasChanged: []contractSchemaDiffDTO{},
	}
	for _, key := range unionKeys(keysOfMap(from.Routes), keysOfMap(to.Routes)) {
		a, inFrom := from.Routes[key]
		b, inTo := to.Routes[key]
		switch {
		case !inFrom:
			out.RoutesAdded = append(out.RoutesAdded, key)
		case !inTo:
			out.RoutesRemoved = append(out.RoutesRemoved, key)
		default:
			d := contractRouteDiffDTO{Route: key, Params: fieldsDiff(a.Params, b.Params), Request: fieldsDiff(a.Request, b.Request), Responses: []contractResponseDiffDTO{}}
			for _, status := range unionKeys(keysOfMap(a.Responses), keysOfMap(b.Responses)) {
				ra, okA := a.Responses[status]
				rb, okB := b.Responses[status]
				entry := contractResponseDiffDTO{Status: status, Added: !okA, Removed: !okB, Fields: fieldsDiff(ra, rb)}
				if entry.Added || entry.Removed || entry.Fields.changed() {
					d.Responses = append(d.Responses, entry)
				}
			}
			d.DeprecatedChanged = a.Deprecated != b.Deprecated
			d.Deprecated = b.Deprecated
			if d.Params.changed() || d.Request.changed() || len(d.Responses) > 0 || d.DeprecatedChanged {
				out.RoutesChanged = append(out.RoutesChanged, d)
			}
		}
	}
	for _, key := range unionKeys(keysOfMap(from.Streams), keysOfMap(to.Streams)) {
		a, inFrom := from.Streams[key]
		b, inTo := to.Streams[key]
		switch {
		case !inFrom:
			out.StreamEventsAdded = append(out.StreamEventsAdded, key)
		case !inTo:
			out.StreamEventsRemoved = append(out.StreamEventsRemoved, key)
		default:
			if d := fieldsDiff(a, b); d.changed() {
				stream, name, _ := strings.Cut(key, " ")
				out.StreamEventsChanged = append(out.StreamEventsChanged, contractEventDiffDTO{Stream: stream, Name: name, Payload: d})
			}
		}
	}
	for _, key := range unionKeys(keysOfMap(from.Mutations), keysOfMap(to.Mutations)) {
		a, inFrom := from.Mutations[key]
		b, inTo := to.Mutations[key]
		switch {
		case !inFrom:
			out.MutationsAdded = append(out.MutationsAdded, key)
		case !inTo:
			out.MutationsRemoved = append(out.MutationsRemoved, key)
		default:
			d := contractMutationDiffDTO{EventType: key, Fields: fieldsDiff(a.Fields, b.Fields), Body: fieldsDiff(a.Body, b.Body), Result: fieldsDiff(a.Result, b.Result)}
			if d.Fields.changed() || d.Body.changed() || d.Result.changed() {
				out.MutationsChanged = append(out.MutationsChanged, d)
			}
		}
	}
	for _, key := range unionKeys(keysOfMap(from.Schemas), keysOfMap(to.Schemas)) {
		a, inFrom := from.Schemas[key]
		b, inTo := to.Schemas[key]
		switch {
		case !inFrom:
			out.SchemasAdded = append(out.SchemasAdded, key)
		case !inTo:
			out.SchemasRemoved = append(out.SchemasRemoved, key)
		default:
			if d := fieldsDiff(a, b); d.changed() {
				out.SchemasChanged = append(out.SchemasChanged, contractSchemaDiffDTO{Name: key, Fields: d})
			}
		}
	}
	out.Identical = len(out.RoutesAdded)+len(out.RoutesRemoved)+len(out.RoutesChanged)+
		len(out.StreamEventsAdded)+len(out.StreamEventsRemoved)+len(out.StreamEventsChanged)+
		len(out.MutationsAdded)+len(out.MutationsRemoved)+len(out.MutationsChanged)+
		len(out.SchemasAdded)+len(out.SchemasRemoved)+len(out.SchemasChanged) == 0
	return out, nil
}

// fieldsDiff compares two field sets by name.
func fieldsDiff(a, b contractFields) contractFieldsDiffDTO {
	out := contractFieldsDiffDTO{Added: []contractFieldDTO{}, Removed: []contractFieldDTO{}, TypeChanged: []contractFieldChangeDTO{}, RequiredChanged: []contractFieldChangeDTO{}}
	word := func(req bool) string {
		if req {
			return "required"
		}
		return "optional"
	}
	for _, name := range unionKeys(keysOfMap(a), keysOfMap(b)) {
		fa, inA := a[name]
		fb, inB := b[name]
		switch {
		case !inA:
			out.Added = append(out.Added, contractFieldDTO{Name: name, Type: fb.Type, Required: fb.Required})
		case !inB:
			out.Removed = append(out.Removed, contractFieldDTO{Name: name, Type: fa.Type, Required: fa.Required})
		default:
			if fa.Type != fb.Type {
				out.TypeChanged = append(out.TypeChanged, contractFieldChangeDTO{Name: name, From: fa.Type, To: fb.Type})
			}
			if fa.Required != fb.Required {
				out.RequiredChanged = append(out.RequiredChanged, contractFieldChangeDTO{Name: name, From: word(fa.Required), To: word(fb.Required)})
			}
		}
	}
	return out
}

func keysOfMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// unionKeys is the sorted union of two key lists.
func unionKeys(a, b []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, k := range list {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
