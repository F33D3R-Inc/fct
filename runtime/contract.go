package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"facet/internal/ir"
)

// GET /api/_contract — the app's declared endpoints (ir.API) as an OpenAPI 3
// document, generated from the compiled graph so it can never drift from what
// the runtime serves: every route, its parameters (path, query or JSON body,
// typed from the action's own parameter list), its success status and reply
// schema (the action's `-> Type`, as a primitive, an entity's row shape, or a
// wire `type`), its error shapes, and the contract's own extensions —
// x-auth, x-rate-limit, x-since — the way f33d3r's mobile clients read them.
// The document's version is a hash of the routes and schemas it describes,
// so a client can tell whether the contract it was built against still holds.

func (s *Server) handleContract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	s.contractDocument()
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.contractRaw)
}

// contractDocument is the app's published contract, built once per process:
// the IR is immutable and the schema version is fixed at boot.
func (s *Server) contractDocument() map[string]any {
	s.contractOnce.Do(func() {
		s.contractDoc = buildContract(s.ir, s.schemaVersion)
		s.contractRaw, _ = json.Marshal(s.contractDoc)
		s.contractGenerated = time.Now().UTC()
	})
	return s.contractDoc
}

// contractVersion is the published contract's info.version.
func (s *Server) contractVersion() string {
	return s.contractDocument()["info"].(map[string]any)["version"].(string)
}

// errorSchemaName is the component every error response references: the
// runtime's own error envelope, `{"error": {"code": …, "message": …}}`
// (see apiError).
const errorSchemaName = "APIErrorDTO"

// nullable marks a schema as maybe-null the way the contract spells it: a
// reference becomes oneOf [the reference, null]; any other schema gains
// "null" in its type (a schema with no type — any JSON value — is typed
// ["null"] alongside its description).
func nullable(schema map[string]any) map[string]any {
	if r, ok := schema["$ref"]; ok {
		return map[string]any{"oneOf": []any{map[string]any{"$ref": r}, map[string]any{"type": "null"}}}
	}
	out := make(map[string]any, len(schema)+1)
	for k, v := range schema {
		out[k] = v
	}
	switch t := out["type"].(type) {
	case string:
		out["type"] = []string{t, "null"}
	case nil:
		out["type"] = []string{"null"}
	}
	return out
}

// bodySchema is a request or reply body's schema: a named DTO is referenced
// as maybe-null (a client decoding it tolerates a null body), anything else
// as itself.
func bodySchema(schema map[string]any) map[string]any {
	if _, ok := schema["$ref"]; ok {
		return nullable(schema)
	}
	return schema
}

// fieldSchema is one wire type field's schema: its element type, as a list
// (nested to its depth) or a map of it, maybe-null when declared `or null`.
func fieldSchema(f ir.WireField, g *ir.IR, usedEntities map[string]bool) map[string]any {
	sch := wireSchema(f.Type, false, g, usedEntities)
	switch {
	case f.Map:
		sch = map[string]any{"type": "object", "additionalProperties": sch}
	case f.List:
		for i := 0; i < max(f.Depth, 1); i++ {
			sch = map[string]any{"type": "array", "items": sch}
		}
	}
	if f.Nullable {
		sch = nullable(sch)
	}
	return sch
}

// errorSchemas are the error envelope's components.
func errorSchemas() map[string]any {
	return map[string]any{
		errorSchemaName: objSchema(map[string]any{"error": ref("apiErrorBody")}, "error"),
		"apiErrorBody":  objSchema(map[string]any{"code": strSchema(), "message": strSchema()}, "code", "message"),
	}
}

func strSchema() map[string]any { return map[string]any{"type": "string"} }

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

// objSchema is a closed object schema with the given properties, the named
// ones required.
func objSchema(props map[string]any, required ...string) map[string]any {
	sch := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		sch["required"] = required
	}
	return sch
}

// rateLimitDoc is a route's x-rate-limit: the limiter class it is metered
// under, keyed by client IP, and the budget this deployment configured for
// it. A limited request is answered 429 with Retry-After.
func rateLimitDoc(class string) map[string]any {
	per := rateLimitFromEnvClass(class)
	return map[string]any{"limiter": class, "per_minute": per, "burst": rateBurst(per), "keyed_by": "ip",
		"headers": []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"}}
}

// operationIDFor names a runtime-served route the way declared routes'
// clients expect: the method, then each literal path segment after the
// /api/vN prefix, capitalized ({param} segments by their name).
func operationIDFor(method, path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) >= 2 && segs[0] == "api" && len(segs[1]) > 1 && segs[1][0] == 'v' {
		segs = segs[2:]
	}
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range segs {
		seg = strings.Trim(seg, "{}")
		for _, part := range strings.FieldsFunc(seg, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// contractDescription is info.description: the conventions every route this
// runtime publishes keeps.
const contractDescription = "The app's declared HTTP surface, generated by the Facet runtime from the compiled graph: every `api` route, every `stream` and its events (x-stream-events), and the tagged-union mutations a whole-body route accepts (x-mutation-events). info.version is the sha256 of this document without it, so equal contracts have equal versions; x-schema-version is the data schema's ordinal in this deployment's history. Every GET answering JSON is a conditional GET (x-conditional-get): a 200 carries a strong ETag over its bytes and Cache-Control: private, no-cache; send If-None-Match to be answered 304. Every error body is an APIErrorDTO. A route over its x-rate-limit budget is answered 429 with Retry-After. A stream opens with an unnumbered hello frame; every other frame carries an id, and a client reconnecting with Last-Event-ID is replayed what it missed."

// buildContract is the document for a graph at a data schema version (0 when
// the app declares no `contract`, so no history numbers its schemas; see
// runtime/contracthistory.go).
func buildContract(g *ir.IR, schemaVersion int) map[string]any {
	schemas := errorSchemas()
	usedEntities := map[string]bool{}
	paths := map[string]map[string]any{}
	errResp := func(desc string) map[string]any {
		return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": nullable(ref(errorSchemaName))}}}
	}
	for _, a := range g.APIs {
		if a.Dispatch != "" {
			if paths[a.Path] == nil {
				paths[a.Path] = map[string]any{}
			}
			paths[a.Path][strings.ToLower(a.Method)] = dispatchOperation(a, g)
			continue
		}
		var act *ir.Action
		for i := range g.Actions {
			if g.Actions[i].Name == a.Action {
				act = &g.Actions[i]
			}
		}
		if act == nil {
			continue
		}
		inPath := map[string]bool{}
		for _, p := range a.Params {
			inPath[p] = true
		}
		params := []map[string]any{}
		docs := map[string]ir.APIParamDoc{}
		for _, d := range a.ParamDocs {
			docs[d.Name] = d
		}
		// paramSchema is a path or query parameter's schema: its documented
		// wire type and closed values when the route documents it.
		paramSchema := func(p ir.Param) map[string]any {
			d, ok := docs[p.Name]
			if !ok {
				return wireSchema(p.Type, p.List, g, usedEntities)
			}
			list := p.List && d.Type != "text" // a list documented as its comma-separated text
			sch := wireSchema(d.Type, list, g, usedEntities)
			if len(d.Enum) > 0 {
				if list {
					sch["items"].(map[string]any)["enum"] = d.Enum
				} else {
					sch["enum"] = d.Enum
				}
			}
			return sch
		}
		withDoc := func(entry map[string]any, name string) map[string]any {
			if d, ok := docs[name]; ok && d.Description != "" {
				entry["description"] = d.Description
			}
			return entry
		}
		body := map[string]any{}
		var required []string
		var bodyWireParams []ir.Param // POST/PUT/PATCH params that bind from the JSON body, in order
		hasBytesBody := false         // a `bytes` param uploads a file: the body is multipart, not JSON
		for _, p := range act.Params {
			if p.Name == a.Bearer {
				continue // carried as `Authorization: Bearer`, described by security
			}
			schema := wireSchema(p.Type, p.List, g, usedEntities)
			switch {
			case inPath[p.Name]:
				params = append(params, withDoc(map[string]any{"name": p.Name, "in": "path", "required": true, "schema": paramSchema(p)}, p.Name))
			case a.Method == http.MethodGet:
				// A DELETE carries its non-path parameters in a JSON body like any
				// other write (`DELETE /me/two-factor {"code": …}`); only a GET,
				// which has no body, reads them from the query.
				qp := map[string]any{"name": p.Name, "in": "query", "required": !p.Optional, "schema": paramSchema(p)}
				if d, ok := docs[p.Name]; p.List && !(ok && d.Type == "text") {
					// Comma-separated (`?ids=1,2`), the form runtime/server.go's paramArg splits.
					qp["style"], qp["explode"] = "form", false
				}
				params = append(params, withDoc(qp, p.Name))
			default:
				if p.Type == "bytes" {
					hasBytesBody = true
				}
				bodyWireParams = append(bodyWireParams, p)
				body[p.Name] = schema
				if !p.Optional {
					required = append(required, p.Name)
				}
			}
		}
		// A body of exactly one parameter typed as a declared wire `type` or
		// `message` is the whole-body convention (`api POST "/events" ->
		// postEvent`, `postEvent(msg: ClientMessage)`): the JSON body IS the
		// DTO/tagged union, not `{"msg": {...}}` — see runtime/apidecl.go's
		// matching bind-the-whole-body case. Every other body shape keeps the
		// per-field object it already had.
		wholeBodyWireType := ""
		if len(bodyWireParams) == 1 && isWireTypeOrMessage(bodyWireParams[0].Type, g) {
			wholeBodyWireType = bodyWireParams[0].Type
		}
		binary := a.Ret == "bytes" // the reply is a file, answered as the body itself
		opID := a.Operation
		if opID == "" {
			opID = operationIDFor(a.Method, a.Path)
		}
		op := map[string]any{
			"operationId": opID,
			"x-auth":      a.Auth,
			"x-since":     a.Since,
			"parameters":  params,
		}
		if a.Summary != "" {
			op["summary"] = a.Summary
		}
		if a.Description != "" {
			op["description"] = a.Description
		}
		if a.Rate != "" {
			op["x-rate-limit"] = rateLimitDoc(a.Rate)
		}
		conditional := a.Method == http.MethodGet && a.Ret != "" && !binary
		if conditional {
			op["x-conditional-get"] = true
		}
		switch {
		case a.Body != "":
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": nullable(ref(schemaNameOf(a.Body, g)))}}}
		case wholeBodyWireType != "":
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": bodySchema(wireSchema(wholeBodyWireType, false, g, usedEntities))}}}
		case len(body) > 0:
			bodySchema := map[string]any{"type": "object", "properties": body}
			if len(required) > 0 {
				sort.Strings(required)
				bodySchema["required"] = required
			}
			contentType := "application/json"
			if hasBytesBody {
				contentType = "multipart/form-data"
			}
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{contentType: map[string]any{"schema": bodySchema}}}
		}
		responses := map[string]any{}
		okResp := map[string]any{"description": http.StatusText(a.Status)}
		if a.Ret != "" && a.Status != http.StatusNoContent && !binary {
			okResp["content"] = map[string]any{"application/json": map[string]any{"schema": bodySchema(wireSchema(a.Ret, a.RetList, g, usedEntities))}}
		}
		responses[itoa(a.Status)] = okResp
		// The error statuses: the route's declared `errors` (which the
		// compiler proved cover every check), else every status its checks
		// can fail with; a credentialed route also answers 401. The
		// runtime's own refusals — a malformed request (400), a spent rate
		// budget (429), a conditional GET's 304 — are conventions the
		// document states once (info.description), not per route.
		with := func(code int, desc string) { responses[itoa(code)] = errResp(desc) }
		for _, code := range a.Errors {
			with(code, http.StatusText(code))
		}
		switch {
		case a.Auth == "session", a.Bearer != "":
			if _, ok := responses["401"]; !ok {
				with(401, "Unauthorized")
			}
			op["security"] = []map[string]any{{"bearer": []string{}}}
		default:
			op["security"] = []map[string]any{} // open: no credential required
		}
		for _, st := range act.Body {
			if len(a.Errors) == 0 {
				collectCheckStatuses(st, func(code int) { with(code, http.StatusText(code)) })
			}
			// `return … status N`: a second success outcome, same reply shape.
			collectReturnStatuses(st, func(code int) {
				alt := map[string]any{"description": http.StatusText(code)}
				if c, ok := okResp["content"]; ok {
					alt["content"] = c
				}
				responses[itoa(code)] = alt
			})
		}
		if binary && len(a.Errors) == 0 {
			with(http.StatusBadGateway, "Bad Gateway") // the file came from a service that did not answer
		}
		op["responses"] = responses
		if paths[a.Path] == nil {
			paths[a.Path] = map[string]any{}
		}
		paths[a.Path][strings.ToLower(a.Method)] = op
	}
	for _, t := range g.Types {
		props := map[string]any{}
		var req []string
		for _, f := range t.Fields {
			props[f.Name] = fieldSchema(f, g, usedEntities)
			if !f.Optional {
				req = append(req, f.Name)
			}
		}
		sort.Strings(req)
		schemas[t.SchemaName()] = objSchema(props, req...)
	}
	// A `message` is a tagged union: internally tagged on "type" with the
	// variant's own (snake_case) name as the discriminant value — see
	// ast.Message's doc. Each variant gets its own named schema (its fields
	// plus a `type` const), and the message's own name is the oneOf across
	// them, with an OpenAPI discriminator so a client can dispatch on the
	// same field the wire actually carries.
	dispatchOnly := map[string]bool{}
	for _, a := range g.APIs {
		if a.Dispatch != "" {
			dispatchOnly[a.Dispatch] = true
		}
	}
	for _, m := range g.Messages {
		if dispatchOnly[m.Name] && !messageReferenced(m.Name, g) {
			// A dispatching route's union is documented variant by variant in
			// x-mutation-events; nothing references it as a schema.
			continue
		}
		var variantRefs []map[string]any
		mapping := map[string]any{}
		for _, v := range m.Variants {
			vSchemaName := m.Name + "_" + v.Name
			props := map[string]any{m.TagName(): map[string]any{"type": "string", "enum": []string{v.WireName()}}}
			req := []string{m.TagName()}
			for _, f := range v.Fields {
				props[f.Name] = wireSchema(f.Type, f.List, g, usedEntities)
				if !f.Optional {
					req = append(req, f.Name)
				}
			}
			schemas[vSchemaName] = map[string]any{"type": "object", "properties": props, "required": req}
			ref := "#/components/schemas/" + vSchemaName
			variantRefs = append(variantRefs, map[string]any{"$ref": ref})
			mapping[v.WireName()] = ref
		}
		schemas[m.Name] = map[string]any{
			"oneOf":         variantRefs,
			"discriminator": map[string]any{"propertyName": m.TagName(), "mapping": mapping},
		}
	}
	for name := range usedEntities {
		for _, e := range g.Entities {
			if e.Name != name {
				continue
			}
			props := map[string]any{}
			for _, f := range e.Fields {
				if f.Secret || f.E2E || f.Password {
					continue
				}
				props[f.Name] = wireSchema(f.Type, false, g, nil)
			}
			schemas[name] = map[string]any{"type": "object", "properties": props}
		}
	}
	// Streams: one GET route each, and every event it carries — the runtime's
	// own `hello` connect frame too, on a stream that declares it — in
	// x-stream-events.
	streamEvents := []map[string]any{}
	for _, st := range g.Streams {
		var evs []ir.StreamEvent
		if st.Hello != "" {
			schemas["HelloEventDTO"] = helloEventSchema()
			evs = append(evs, ir.StreamEvent{Name: "hello", Type: "HelloEventDTO", Since: st.Hello,
				Summary: "Connect frame: this connection's id, the contract and schema versions the server serves, its clock. Unnumbered."})
		}
		evs = append(evs, st.Events...)
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].Name < evs[j].Name })
		var names []string
		for _, ev := range evs {
			names = append(names, ev.Name)
			payloadRef := ref(schemaNameOf(ev.Type, g))
			streamEvents = append(streamEvents, map[string]any{
				"name": ev.Name, "stream": st.Path, "summary": ev.Summary, "x-since": ev.Since,
				"payload": map[string]any{"oneOf": []any{payloadRef, map[string]any{"type": "null"}}},
			})
		}
		responses := map[string]any{"200": map[string]any{
			"description":     "text/event-stream; events: " + strings.Join(names, ", ") + " (schemas in x-stream-events)",
			"content":         map[string]any{"text/event-stream": map[string]any{"schema": strSchema()}},
			"x-stream-events": names,
		}}
		op := map[string]any{
			"operationId": operationIDFor(http.MethodGet, st.Path),
			"x-auth":      st.Auth,
			"x-since":     st.Since,
			"parameters":  streamParams(st),
			"responses":   responses,
		}
		if st.Summary != "" {
			op["summary"] = st.Summary
		}
		if st.Description != "" {
			op["description"] = st.Description
		}
		// The stream's published errors; else its connect hooks' own
		// refusals (`check … status 404`).
		for _, code := range st.Errors {
			responses[itoa(code)] = errResp(http.StatusText(code))
		}
		for _, h := range st.Connects {
			if len(st.Errors) > 0 {
				break
			}
			for i := range g.Actions {
				if g.Actions[i].Name == h.Action {
					for _, stmt := range g.Actions[i].Body {
						collectCheckStatuses(stmt, func(code int) { responses[itoa(code)] = errResp(http.StatusText(code)) })
					}
				}
			}
		}
		if st.Rate != "" {
			op["x-rate-limit"] = rateLimitDoc(st.Rate)
		}
		if st.Auth == "session" {
			op["security"] = []map[string]any{{"bearer": []string{}}}
			responses["401"] = errResp("Unauthorized")
		} else {
			op["security"] = []map[string]any{}
		}
		paths[st.Path] = map[string]any{"get": op}
	}
	sort.SliceStable(streamEvents, func(i, j int) bool {
		if streamEvents[i]["stream"] != streamEvents[j]["stream"] {
			return streamEvents[i]["stream"].(string) < streamEvents[j]["stream"].(string)
		}
		return streamEvents[i]["name"].(string) < streamEvents[j]["name"].(string)
	})
	// `contract "/path"`: the document, its version, history and diff, each
	// with the runtime's own schema for its answer.
	if c := g.Contract; c != nil {
		for name, sch := range contractSchemas() {
			schemas[name] = sch
		}
		for p, op := range contractOperations(c, errResp) {
			paths[p] = map[string]any{"get": op}
		}
	}
	title, description := g.App, contractDescription
	bearerDesc := "The session token the app's sign-in answers with (or a route's own credential, per its x-auth), as `Authorization: Bearer <token>`."
	if c := g.Contract; c != nil {
		if c.Title != "" {
			title = c.Title
		}
		if c.Description != "" {
			description = c.Description
		}
		if c.Bearer != "" {
			bearerDesc = c.Bearer
		}
	}
	doc := map[string]any{
		"openapi":           "3.1.0",
		"info":              map[string]any{"title": title, "version": "", "description": description},
		"paths":             paths,
		"components":        map[string]any{"schemas": schemas, "securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "opaque", "description": bearerDesc}}},
		"security":          []map[string]any{{"bearer": []string{}}},
		"x-schema-version":  schemaVersion,
		"x-stream-events":   streamEvents,
		"x-mutation-events": mutationEvents(g),
		"x-facet-catalog":   []any{},
	}
	// The version is the content: the sha256 of the document without it, so
	// identical declarations publish identical versions.
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	doc["info"].(map[string]any)["version"] = hex.EncodeToString(sum[:])
	return doc
}

// streamParams are a stream route's parameters: its {path} values, then the
// two ways a reconnecting client names its resume point.
func streamParams(st ir.Stream) []map[string]any {
	var out []map[string]any
	for _, p := range st.Params {
		entry := map[string]any{"name": p, "in": "path", "required": true, "schema": strSchema()}
		for _, d := range st.ParamDocs {
			if d.Name == p {
				if d.Description != "" {
					entry["description"] = d.Description
				}
				if len(d.Enum) > 0 {
					entry["schema"] = map[string]any{"type": "string", "enum": d.Enum}
				}
			}
		}
		out = append(out, entry)
	}
	return append(out,
		map[string]any{"name": "Last-Event-ID", "in": "header", "required": false, "schema": strSchema(), "description": "The last `id:` seen; missed frames are replayed after it."},
		map[string]any{"name": "last_event_id", "in": "query", "required": false, "schema": strSchema(), "description": "The same resume point for a client that reconnects by hand; the header wins."})
}

// helloEventSchema is the payload of every stream's connect frame.
func helloEventSchema() map[string]any {
	dt := map[string]any{"type": "string", "format": "date-time"}
	return objSchema(map[string]any{"session_id": strSchema(), "contract_version": strSchema(), "schema_version": strSchema(), "server_time": dt},
		"contract_version", "schema_version", "server_time", "session_id")
}

// dispatchOperation is a dispatching route's entry in paths: one object body
// (JSON or a form) carrying the tag and whatever the variant it names takes;
// every variant, with its fields and reply, is in x-mutation-events.
func dispatchOperation(a ir.API, g *ir.IR) map[string]any {
	tag := "type"
	for _, m := range g.Messages {
		if m.Name == a.Dispatch {
			tag = m.TagName()
		}
	}
	bodySchema := map[string]any{"type": "object", "additionalProperties": true,
		"properties": map[string]any{tag: strSchema()}, "required": []string{tag}}
	opID, summary, description := a.Operation, a.Summary, a.Description
	if opID == "" {
		opID = operationIDFor(a.Method, a.Path)
	}
	if summary == "" {
		summary = "One " + tag + " per call, as a form or a JSON object."
	}
	if description == "" {
		description = "See x-mutation-events for every " + tag + ", its fields, and the JSON result it answers with (204 when it has none)."
	}
	op := map[string]any{
		"operationId": opID,
		"summary":     summary,
		"description": description,
		"x-since":     a.Since,
		"security":    []map[string]any{{"bearer": []string{}}},
		"requestBody": map[string]any{"required": true, "content": map[string]any{
			"application/json":                  map[string]any{"schema": bodySchema},
			"application/x-www-form-urlencoded": map[string]any{"schema": bodySchema},
		}},
		"responses": map[string]any{"default": map[string]any{"description": "Per " + tag + "; see x-mutation-events."}},
	}
	if a.Rate != "" {
		op["x-rate-limit"] = rateLimitDoc(a.Rate)
	}
	return op
}

// mutationEvents is x-mutation-events: every variant of a tagged union a
// route accepts — a dispatching route's (`api POST "/events" -> Mutation`),
// documented, with the reply its action answers; or a whole-body route's
// (`postEvent(ev: ClientEvent)`) — its discriminant and its fields.
func mutationEvents(g *ir.IR) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, a := range g.APIs {
		if a.Dispatch == "" {
			continue
		}
		for _, m := range g.Messages {
			if m.Name != a.Dispatch {
				continue
			}
			for _, v := range m.Variants {
				if seen[v.WireName()] {
					continue
				}
				seen[v.WireName()] = true
				fields := []map[string]any{}
				for _, f := range v.Fields {
					fd := map[string]any{"name": f.Name, "type": mutationFieldType(f), "required": !f.Optional && f.Default == ""}
					if f.Description != "" {
						fd["description"] = f.Description
					}
					if len(f.Enum) > 0 {
						fd["enum"] = f.Enum
					}
					fields = append(fields, fd)
				}
				ev := map[string]any{"event_type": v.WireName(), "summary": v.Summary, "x-since": v.Since, "fields": fields}
				if v.BodyType != "" {
					ev["body"] = map[string]any{"oneOf": []any{ref(schemaNameOf(v.BodyType, g)), map[string]any{"type": "null"}}}
				}
				for _, act := range g.Actions {
					if act.Name == v.Action && act.Ret != "" && isWireTypeOrMessage(act.Ret, g) {
						r := ref(schemaNameOf(act.Ret, g))
						if act.RetList {
							r = map[string]any{"type": "array", "items": r}
						}
						ev["result"] = map[string]any{"oneOf": []any{r, map[string]any{"type": "null"}}}
					}
				}
				out = append(out, ev)
			}
		}
	}
	for _, a := range g.APIs {
		if a.Method == http.MethodGet {
			continue
		}
		var act *ir.Action
		for i := range g.Actions {
			if g.Actions[i].Name == a.Action {
				act = &g.Actions[i]
			}
		}
		if act == nil {
			continue
		}
		inPath := map[string]bool{}
		for _, p := range a.Params {
			inPath[p] = true
		}
		var body []ir.Param
		for _, p := range act.Params {
			if !inPath[p.Name] && p.Name != a.Bearer {
				body = append(body, p)
			}
		}
		if len(body) != 1 {
			continue
		}
		for _, m := range g.Messages {
			if m.Name != body[0].Type {
				continue
			}
			for _, v := range m.Variants {
				if seen[v.WireName()] {
					continue
				}
				seen[v.WireName()] = true
				fields := []map[string]any{}
				for _, f := range v.Fields {
					fields = append(fields, map[string]any{"name": f.Name, "type": mutationFieldType(f), "required": !f.Optional})
				}
				out = append(out, map[string]any{"event_type": v.Name, "summary": "", "x-since": a.Since, "fields": fields})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["event_type"].(string) < out[j]["event_type"].(string) })
	return out
}

// mutationFieldType is a mutation field's JSON type name.
func mutationFieldType(f ir.WireField) string {
	t := "object"
	switch f.Type {
	case "int", "money", "date":
		t = "integer"
	case "float", "number":
		t = "number"
	case "bool":
		t = "boolean"
	case "text", "datetime":
		t = "string"
	}
	if f.List {
		return "array"
	}
	return t
}

// isWireTypeOrMessage reports whether name is a declared wire `type` or
// `message` — the shapes whose own schema IS a request body, not a field
// wrapped inside one (see buildContract's wholeBodyWireType).
func isWireTypeOrMessage(name string, g *ir.IR) bool {
	for _, t := range g.Types {
		if t.Name == name {
			return true
		}
	}
	for _, m := range g.Messages {
		if m.Name == name {
			return true
		}
	}
	return false
}

// wireSchema is the OpenAPI schema of one fct type name.
func wireSchema(typ string, list bool, g *ir.IR, usedEntities map[string]bool) map[string]any {
	var sch map[string]any
	switch typ {
	case "int", "money", "date":
		sch = map[string]any{"type": "integer"}
	case "float", "number":
		sch = map[string]any{"type": "number"}
	case "bool":
		sch = map[string]any{"type": "boolean"}
	case "text":
		sch = map[string]any{"type": "string"}
	case "datetime":
		sch = map[string]any{"type": "string", "format": "date-time"}
	case "bytes":
		// An action parameter bound from an uploaded file's multipart form
		// field (see internal/ir/build.go's `bytes`-parameter validation) —
		// OpenAPI's own convention for a file upload field.
		sch = map[string]any{"type": "string", "format": "binary"}
	case "json":
		sch = map[string]any{"description": "Any JSON value."}
	default:
		isEnum := false
		for _, en := range g.Enums {
			if en.Name == typ {
				isEnum = true
				vals := append([]string{}, en.Values...)
				sort.Strings(vals)
				sch = map[string]any{"type": "string", "enum": vals}
			}
		}
		if !isEnum {
			for _, e := range g.Entities {
				if e.Name == typ && usedEntities != nil {
					usedEntities[typ] = true
				}
			}
			sch = map[string]any{"$ref": "#/components/schemas/" + schemaNameOf(typ, g)}
		}
	}
	if list {
		return map[string]any{"type": "array", "items": sch}
	}
	return sch
}

// schemaNameOf is the contract schema name of a declared type/message/entity
// name: a wire type's `as "…"` alias when it has one, else the name itself.
func schemaNameOf(typ string, g *ir.IR) string {
	for _, t := range g.Types {
		if t.Name == typ {
			return t.SchemaName()
		}
	}
	return typ
}

// collectReturnStatuses reports every `return … status N` in a body.
func collectReturnStatuses(st ir.Stmt, f func(int)) {
	if st.Op == "return" && st.Status != 0 {
		f(st.Status)
	}
	for _, b := range st.Body {
		collectReturnStatuses(b, f)
	}
	for _, b := range st.Else {
		collectReturnStatuses(b, f)
	}
}

// collectCheckStatuses reports every explicit `check … status N` in a body,
// so the contract lists the codes the endpoint can actually answer with.
func collectCheckStatuses(st ir.Stmt, f func(int)) {
	if st.Op == "check" && st.Status != 0 {
		f(st.Status)
	}
	for _, b := range st.Body {
		collectCheckStatuses(b, f)
	}
	for _, b := range st.Else {
		collectCheckStatuses(b, f)
	}
}

// messageReferenced reports whether a message is named as a type anywhere a
// schema would reference it: a wire type's field, an action's parameter or
// reply, a stream event's payload.
func messageReferenced(name string, g *ir.IR) bool {
	for _, t := range g.Types {
		for _, f := range t.Fields {
			if f.Type == name {
				return true
			}
		}
	}
	for _, m := range g.Messages {
		for _, v := range m.Variants {
			for _, f := range v.Fields {
				if f.Type == name {
					return true
				}
			}
		}
	}
	for _, a := range g.Actions {
		if a.Ret == name {
			return true
		}
		for _, p := range a.Params {
			if p.Type == name {
				return true
			}
		}
	}
	for _, st := range g.Streams {
		for _, ev := range st.Events {
			if ev.Type == name {
				return true
			}
		}
	}
	return false
}
