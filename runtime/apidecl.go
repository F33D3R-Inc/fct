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
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err.Error() != "EOF" {
			apiError(w, http.StatusBadRequest, "request body must be a JSON object")
			return true
		}
		for k, v := range body {
			if _, ok := bound[k]; !ok {
				bound[k] = v
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
