package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"facet/internal/ir"
)

// Declared `api` endpoints — the app's typed HTTP contract (ir.API).
//
// Each route binds a server-placed action. The request supplies the action's
// parameters by name: `{name}` path segments first, then the query string
// (every method) and, for POST/PUT/PATCH, the fields of a JSON object body.
// Values arrive as text or JSON scalars and are coerced to the parameter's
// declared type exactly as the generic `/api/<action>` projection coerces its
// positional args. The action runs under the caller's session (cookie or
// bearer token), its `requires` gate is the route's auth, and its reply value
// is the response body — bare, not wrapped in the `{deltas, value}` envelope
// the browser's event channel uses, because a native client wants the DTO.
//
// Failure shapes are the contract's: a failed `requires` is 401 (no session)
// or 403 (a session the gate refuses); a failed `check` is its own `status`
// or 422; every error body is `{"error": "<message>"}`. The route's rate
// class (read | write | auth) meters it against a limiter of its own, so a
// burst of writes cannot starve reads.

// compiledAPI is one ir.API with its path split for matching.
type compiledAPI struct {
	decl ir.API
	segs []string // literal segments, "" for a {param} slot
	act  *ir.Action
}

func (s *Server) compileAPIs() []compiledAPI {
	out := make([]compiledAPI, 0, len(s.ir.APIs))
	for _, a := range s.ir.APIs {
		act := s.byAction[a.Action]
		if act == nil {
			continue
		}
		var segs []string
		for _, seg := range strings.Split(strings.Trim(a.Path, "/"), "/") {
			if strings.HasPrefix(seg, "{") {
				segs = append(segs, "")
			} else {
				segs = append(segs, seg)
			}
		}
		out = append(out, compiledAPI{decl: a, segs: segs, act: act})
	}
	// Literal routes before parameterized ones at the same length, so
	// `/api/v2/me` wins over `/api/v2/{id}` however they were declared.
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := literalCount(out[i].segs), literalCount(out[j].segs)
		return li > lj
	})
	return out
}

// wholeBodyParam names the single non-path parameter of a's action when its
// type is a declared wire `type` or `message` and there is exactly one such
// parameter — the same rule runtime/contract.go's wholeBodyWireType uses for
// the OpenAPI requestBody, kept in one place logically even though the two
// call sites can't share code (one walks a compiledAPI, the other an ir.API).
func wholeBodyParam(a *compiledAPI, g *ir.IR) (string, bool) {
	inPath := map[string]bool{}
	for _, name := range a.decl.Params {
		inPath[name] = true
	}
	var bodyParams []ir.Param
	for _, p := range a.act.Params {
		if !inPath[p.Name] {
			bodyParams = append(bodyParams, p)
		}
	}
	if len(bodyParams) != 1 || !isWireTypeOrMessage(bodyParams[0].Type, g) {
		return "", false
	}
	return bodyParams[0].Name, true
}

// bytesParams is the non-path parameters of a's action typed `bytes` — each
// one an uploaded file's multipart form field — or nil if there are none.
func bytesParams(a *compiledAPI) []ir.Param {
	inPath := map[string]bool{}
	for _, name := range a.decl.Params {
		inPath[name] = true
	}
	var out []ir.Param
	for _, p := range a.act.Params {
		if !inPath[p.Name] && p.Type == "bytes" {
			out = append(out, p)
		}
	}
	return out
}

// bindMultipartBody parses a multipart/form-data request into bound: each
// `bytes`-typed parameter is read as a file field (named after the
// parameter), stored through the same upload mechanism POST /upload uses
// (runtime/upload.go's storeUpload), and bound to the stored file's public
// URL — the "path/id" a `bytes` parameter's value actually is at runtime,
// exactly as an ordinary JSON body parameter is bound to its decoded value.
// Every other, non-`bytes` parameter is read as an ordinary form value field.
func (s *Server) bindMultipartBody(w http.ResponseWriter, r *http.Request, a *compiledAPI, bound map[string]any) error {
	cap := singleUploadCap()
	r.Body = http.MaxBytesReader(w, r.Body, cap)
	if err := r.ParseMultipartForm(cap); err != nil {
		return fmt.Errorf("request body must be multipart/form-data")
	}
	inPath := map[string]bool{}
	for _, name := range a.decl.Params {
		inPath[name] = true
	}
	for _, p := range a.act.Params {
		if inPath[p.Name] {
			continue
		}
		if p.Type == "bytes" {
			file, hdr, err := r.FormFile(p.Name)
			if err != nil {
				continue // an absent optional file; the caller's "missing parameter" check handles a required one
			}
			name, err := s.storeUpload(file, hdr.Filename)
			file.Close()
			if err != nil {
				return err
			}
			bound[p.Name] = mediaPathPrefix + name
			continue
		}
		if _, ok := bound[p.Name]; ok {
			continue
		}
		if v := r.FormValue(p.Name); v != "" {
			bound[p.Name] = v
		}
	}
	return nil
}

func literalCount(segs []string) int {
	n := 0
	for _, s := range segs {
		if s != "" {
			n++
		}
	}
	return n
}

// matchAPI finds the declared route for a request and the values its path
// parameters bound, in declaration order.
func matchAPI(apis []compiledAPI, r *http.Request) (*compiledAPI, []string, bool) {
	path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	pathMatched := false
	for i := range apis {
		a := &apis[i]
		if len(a.segs) != len(path) {
			continue
		}
		var vals []string
		ok := true
		for k, seg := range a.segs {
			if seg == "" {
				vals = append(vals, path[k])
			} else if seg != path[k] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		pathMatched = true
		if a.decl.Method == r.Method {
			return a, vals, true
		}
	}
	return nil, nil, pathMatched
}

// serveDeclaredAPI answers a request that matches a declared route and reports
// whether it did; anything else falls through to the built-in mux.
func (s *Server) serveDeclaredAPI(w http.ResponseWriter, r *http.Request, apis []compiledAPI) bool {
	if len(apis) == 0 {
		return false
	}
	a, pathVals, pathMatched := matchAPI(apis, r)
	if a == nil {
		if pathMatched {
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			return true
		}
		return false
	}
	if lim := s.rateClass(a.decl.Rate); lim != nil && !lim.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, "rate limited")
		return true
	}

	// Bind the action's parameters by name.
	bound := map[string]any{}
	for i, name := range a.decl.Params {
		if i < len(pathVals) {
			bound[name] = pathVals[i]
		}
	}
	q := r.URL.Query()
	for _, p := range a.act.Params {
		if _, ok := bound[p.Name]; ok {
			continue
		}
		if v := q.Get(p.Name); v != "" || q.Has(p.Name) {
			bound[p.Name] = v
		}
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		if bytesParams(a) != nil {
			// A `bytes`-typed body parameter uploads a file: the request is
			// multipart/form-data, not JSON (internal/ir/build.go refuses a
			// `bytes` parameter on a GET/DELETE route, so POST/PUT/PATCH is
			// the only case this reaches).
			if err := s.bindMultipartBody(w, r, a, bound); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return true
			}
		} else {
			var body map[string]any
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err.Error() != "EOF" {
				apiError(w, http.StatusBadRequest, "request body must be a JSON object")
				return true
			}
			// A route whose only body-bound parameter is typed as a declared
			// wire `type` or `message` takes the whole decoded body as that
			// one parameter's value (the message IS the body — see
			// runtime/contract.go's matching wholeBodyWireType, the golden
			// shape a tagged-union mutation endpoint like `POST /events`
			// wants: one object with its own `type`/discriminant field, not
			// `{"paramName": {...}}`). Every other route keeps binding the
			// body's own top-level fields to same-named parameters, unchanged.
			if name, ok := wholeBodyParam(a, s.ir); ok {
				bound[name] = body
			} else {
				for k, v := range body {
					if _, ok := bound[k]; !ok {
						bound[k] = v
					}
				}
			}
		}
	}
	args := make([]any, len(a.act.Params))
	for i, p := range a.act.Params {
		v, ok := bound[p.Name]
		if !ok {
			if !p.Optional {
				apiError(w, http.StatusBadRequest, fmt.Sprintf("missing parameter %q", p.Name))
				return true
			}
			args[i] = zero(p.Type)
			continue
		}
		cv, ok := coerceParam(v, p.Type)
		if !ok {
			apiError(w, http.StatusBadRequest, fmt.Sprintf("parameter %q expects %s", p.Name, p.Type))
			return true
		}
		args[i] = cv
	}

	sid := s.sidForRequest(r)
	if a.decl.Auth == "session" && sid == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		apiError(w, http.StatusUnauthorized, "sign in to call this endpoint")
		return true
	}
	// A route with no `requires` gate (Auth != "session") is reachable by a
	// caller with no session yet — a public signup/login-shaped write. Unlike a
	// GET, which must stay read-only (sidForRequest never mints; see its own
	// doc), a write here needs a REAL, distinct session before the action runs:
	// runActionLocked's ensureSession(sid) treats sid as a session's storage
	// key, and leaving it "" for every anonymous caller would collapse them
	// all onto the one session literally keyed by "" — the same shared,
	// racing identity for every concurrent unauthenticated write. `session`
	// mints one and signs it into the response the exact way the generic
	// `/api/<action>` POST path already does, so this matches that existing,
	// already-correct behavior instead of adding a second rule.
	if sid == "" && r.Method != http.MethodGet {
		sid = s.session(w, r)
	}
	_, value, status, msg := s.runActionValue(sid, a.act, args)
	if status != http.StatusOK {
		apiError(w, status, msg)
		return true
	}
	if a.decl.Status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	if a.decl.Ret == "" {
		w.WriteHeader(a.decl.Status)
		w.Write([]byte("{}"))
		return true
	}
	body, err := json.Marshal(value)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "reply could not be encoded")
		return true
	}
	if r.Method == http.MethodGet {
		// Conditional GET: the ETag is the reply's content hash, so a client
		// that already holds this exact answer gets 304 and no body. Every
		// declared GET is conditional (the contract says so: x-conditional-get).
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:16]) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if strings.Contains(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	w.WriteHeader(a.decl.Status)
	w.Write(body)
	return true
}

func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// rateClass answers the limiter a route's rate class meters against, created
// on first use from FACET_RATE_LIMIT_<CLASS> (requests per minute per IP;
// FACET_RATE_LIMIT is the fallback). An unclassified route is unmetered here
// (the generic projections keep their own global limiter).
func (s *Server) rateClass(class string) *rateLimiter {
	if class == "" {
		return nil
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateClasses == nil {
		s.rateClasses = map[string]*rateLimiter{}
	}
	if l, ok := s.rateClasses[class]; ok {
		return l
	}
	per := rateLimitFromEnvClass(class)
	l := newRateLimiter(per)
	s.rateClasses[class] = l
	return l
}
