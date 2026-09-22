package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

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
	doc := buildContract(s.ir)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

func buildContract(g *ir.IR) map[string]any {
	schemas := map[string]any{
		"APIError": map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "string"}}, "required": []string{"error"}},
	}
	usedEntities := map[string]bool{}
	paths := map[string]map[string]any{}
	for _, a := range g.APIs {
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
		var params []map[string]any
		body := map[string]any{}
		var required []string
		var bodyWireParams []ir.Param // POST/PUT/PATCH params that bind from the JSON body, in order
		hasBytesBody := false         // a `bytes` param uploads a file: the body is multipart, not JSON
		for _, p := range act.Params {
			schema := wireSchema(p.Type, false, g, usedEntities)
			switch {
			case inPath[p.Name]:
				params = append(params, map[string]any{"name": p.Name, "in": "path", "required": true, "schema": schema})
			case a.Method == http.MethodGet || a.Method == http.MethodDelete:
				params = append(params, map[string]any{"name": p.Name, "in": "query", "required": !p.Optional, "schema": schema})
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
		op := map[string]any{
			"operationId": a.Action,
			"x-auth":      a.Auth,
			"x-since":     a.Since,
		}
		if a.Rate != "" {
			op["x-rate-limit"] = a.Rate
		}
		if a.Method == http.MethodGet && a.Ret != "" {
			op["x-conditional-get"] = true
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if wholeBodyWireType != "" {
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": wireSchema(wholeBodyWireType, false, g, usedEntities)}}}
		} else if len(body) > 0 {
			bodySchema := map[string]any{"type": "object", "properties": body}
			if len(required) > 0 {
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
		if a.Ret != "" && a.Status != http.StatusNoContent {
			okResp["content"] = map[string]any{"application/json": map[string]any{"schema": wireSchema(a.Ret, a.RetList, g, usedEntities)}}
		}
		responses[itoa(a.Status)] = okResp
		errRef := map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/APIError"}}}}
		with := func(code int, desc string) {
			e := map[string]any{"description": desc}
			for k, v := range errRef {
				e[k] = v
			}
			responses[itoa(code)] = e
		}
		with(400, "Bad Request")
		with(422, "Unprocessable Entity")
		if a.Method == http.MethodGet && a.Ret != "" {
			responses["304"] = map[string]any{"description": "Not Modified"}
		}
		if a.Auth == "session" {
			with(401, "Unauthorized")
			with(403, "Forbidden")
			op["security"] = []map[string]any{{"bearer": []string{}}}
		}
		if a.Rate != "" {
			with(429, "Too Many Requests")
		}
		for _, st := range act.Body {
			collectCheckStatuses(st, func(code int) { with(code, http.StatusText(code)) })
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
			props[f.Name] = wireSchema(f.Type, f.List, g, usedEntities)
			if !f.Optional {
				req = append(req, f.Name)
			}
		}
		sch := map[string]any{"type": "object", "properties": props}
		if len(req) > 0 {
			sch["required"] = req
		}
		schemas[t.Name] = sch
	}
	// A `message` is a tagged union: internally tagged on "type" with the
	// variant's own (snake_case) name as the discriminant value — see
	// ast.Message's doc. Each variant gets its own named schema (its fields
	// plus a `type` const), and the message's own name is the oneOf across
	// them, with an OpenAPI discriminator so a client can dispatch on the
	// same field the wire actually carries.
	for _, m := range g.Messages {
		var variantRefs []map[string]any
		mapping := map[string]any{}
		for _, v := range m.Variants {
			vSchemaName := m.Name + "_" + v.Name
			props := map[string]any{"type": map[string]any{"type": "string", "enum": []string{v.Name}}}
			req := []string{"type"}
			for _, f := range v.Fields {
				props[f.Name] = wireSchema(f.Type, f.List, g, usedEntities)
				if !f.Optional {
					req = append(req, f.Name)
				}
			}
			schemas[vSchemaName] = map[string]any{"type": "object", "properties": props, "required": req}
			ref := "#/components/schemas/" + vSchemaName
			variantRefs = append(variantRefs, map[string]any{"$ref": ref})
			mapping[v.Name] = ref
		}
		schemas[m.Name] = map[string]any{
			"oneOf":         variantRefs,
			"discriminator": map[string]any{"propertyName": "type", "mapping": mapping},
		}
	}
	for name := range usedEntities {
		for _, e := range g.Entities {
			if e.Name != name {
				continue
			}
			props := map[string]any{}
			for _, f := range e.Fields {
				if f.Secret || f.E2E {
					continue
				}
				props[f.Name] = wireSchema(f.Type, false, g, nil)
			}
			schemas[name] = map[string]any{"type": "object", "properties": props}
		}
	}
	var streamEvents []map[string]any
	for _, st := range g.Streams {
		for _, ev := range st.Events {
			streamEvents = append(streamEvents, map[string]any{
				"name": ev, "stream": st.Path, "auth": st.Auth,
				"payload": map[string]any{"$ref": "#/components/schemas/" + ev},
			})
		}
		paths[st.Path] = map[string]any{"get": map[string]any{
			"operationId": "stream" + strings.ReplaceAll(strings.Title(strings.ReplaceAll(strings.Trim(st.Path, "/"), "/", " ")), " ", ""),
			"x-auth":      st.Auth,
			"x-stream":    true,
			"responses":   map[string]any{"200": map[string]any{"description": "text/event-stream", "content": map[string]any{"text/event-stream": map[string]any{}}}},
		}}
	}
	doc := map[string]any{
		"openapi":    "3.0.3",
		"info":       map[string]any{"title": g.App, "version": ""},
		"paths":      paths,
		"components": map[string]any{"schemas": schemas, "securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}}},
	}
	// The version is the content: identical declarations, identical version.
	raw, _ := json.Marshal(map[string]any{"paths": paths, "schemas": schemas})
	sum := sha256.Sum256(raw)
	doc["info"].(map[string]any)["version"] = hex.EncodeToString(sum[:8])
	doc["x-schema-version"] = hex.EncodeToString(sum[:8])
	if len(streamEvents) > 0 {
		doc["x-stream-events"] = streamEvents
	}
	return doc
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
		sch = map[string]any{"type": "object"}
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
			sch = map[string]any{"$ref": "#/components/schemas/" + typ}
		}
	}
	if list {
		return map[string]any{"type": "array", "items": sch}
	}
	return sch
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
