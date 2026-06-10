package fa

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

// RouteCtx carries the matched request, its path parameters, and the resolved
// identity to a page render function. It is the routing analogue of Ctx (which
// is for events).
type RouteCtx struct {
	R        *http.Request
	Params   map[string]string
	Identity string
	app      *App
}

// Param returns a path parameter captured by a `:name` segment (e.g. for the
// pattern "/u/:handle", Param("handle")), or "" if absent.
func (rc RouteCtx) Param(name string) string { return rc.Params[name] }

// Query returns a URL query value, or "".
func (rc RouteCtx) Query(name string) string { return rc.R.URL.Query().Get(name) }

// View builds an authorization View for render-time who: enforcement.
func (rc RouteCtx) View() View { return View{Identity: rc.Identity, R: rc.R} }

// PageFunc renders a route's content — the HTML that goes inside the Playground
// root mount. The framework wraps it in the document shell on a full load, or
// returns it as a fragment on client-side navigation.
type PageFunc func(RouteCtx) template.HTML

type route struct {
	segs   []seg
	title  string
	render PageFunc
}

// seg is one pattern segment: a literal, or a `:param` capture.
type seg struct {
	lit   string
	param string // non-empty => capture this segment under this name
}

func parsePattern(pattern string) []seg {
	parts := splitPath(pattern)
	segs := make([]seg, len(parts))
	for i, p := range parts {
		if strings.HasPrefix(p, ":") {
			segs[i] = seg{param: p[1:]}
		} else {
			segs[i] = seg{lit: p}
		}
	}
	return segs
}

// splitPath normalizes a URL/path into its non-empty segments. "/" → []; "/a/b/"
// → ["a","b"].
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (rt *route) match(path string) (map[string]string, bool) {
	parts := splitPath(path)
	if len(parts) != len(rt.segs) {
		return nil, false
	}
	var params map[string]string
	for i, s := range rt.segs {
		if s.param != "" {
			if params == nil {
				params = make(map[string]string)
			}
			params[s.param] = parts[i]
		} else if s.lit != parts[i] {
			return nil, false
		}
	}
	return params, true
}

// Route registers a page at a URL pattern. Patterns use `:name` for path
// parameters: Route("/u/:handle", "Profile", fn) matches /u/ada and exposes
// rc.Param("handle") == "ada". The title sets the page <title> (and is sent on
// client-side navigation so the tab updates). Chainable; call MountRouter to
// serve. More specific routes should be registered before catch-alls — the first
// match wins.
func (a *App) Route(pattern, title string, render PageFunc) *App {
	a.routes = append(a.routes, &route{segs: parsePattern(pattern), title: title, render: render})
	return a
}

// NotFound sets the content rendered for an unmatched path (HTTP 404). Optional;
// without it FA serves a plain 404.
func (a *App) NotFound(render PageFunc) *App {
	a.notFound = render
	return a
}

// MountRouter serves all registered routes on mux under GET, using opts as the
// base Playground chrome (CSS/theme/head shared across pages; a route's title
// overrides opts.Title). It coexists with Mount (which owns /sse, /events,
// /manifest.json, /fa-runtime.js); the router takes the catch-all GET /.
//
// Two response modes from the same route:
//   - normal load → the full Playground document (server-rendered HTML);
//   - client navigation (the runtime sends "FA-Nav: 1") → a JSON fragment
//     {title, html} that the runtime swaps into the root mount WITHOUT a page
//     reload, so the SSE connection and all live facets survive across pages.
func (a *App) MountRouter(mux *http.ServeMux, opts ShellOptions) {
	a.shellOpts = opts
	mux.HandleFunc("GET /", a.serveRoute)
}

func (a *App) serveRoute(w http.ResponseWriter, r *http.Request) {
	for _, rt := range a.routes {
		params, ok := rt.match(r.URL.Path)
		if !ok {
			continue
		}
		rc := RouteCtx{R: r, Params: params, Identity: a.identityOf(r), app: a}
		a.writePage(w, r, rt.title, rt.render(rc))
		return
	}
	// No route matched.
	w.WriteHeader(http.StatusNotFound)
	if a.notFound != nil {
		rc := RouteCtx{R: r, Identity: a.identityOf(r), app: a}
		a.writePage(w, r, "Not found", a.notFound(rc))
		return
	}
	if isNav(r) {
		writeNav(w, "Not found", template.HTML("<section><h1>404</h1><p>Not found.</p></section>"))
		return
	}
	http.Error(w, "404 not found", http.StatusNotFound)
}

// writePage emits a route's content in one of three shapes, by request header:
//   - FA-Native: 1 → a neutral view tree as JSON {title, tree} (iOS/Android);
//   - FA-Nav: 1    → an HTML fragment as JSON {title, html} (web client nav);
//   - otherwise    → the full Playground document (a normal browser load).
func (a *App) writePage(w http.ResponseWriter, r *http.Request, title string, content template.HTML) {
	if isNative(r) {
		writeNative(w, title, content)
		return
	}
	if isNav(r) {
		writeNav(w, title, content)
		return
	}
	opts := a.shellOpts
	if title != "" {
		opts.Title = title
	}
	secureHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(a.Page(content, opts)))
}

func isNav(r *http.Request) bool    { return r.Header.Get("FA-Nav") == "1" }
func isNative(r *http.Request) bool { return r.Header.Get("FA-Native") == "1" }

func writeNav(w http.ResponseWriter, title string, content template.HTML) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]string{"title": title, "html": string(content)})
}

// writeNative renders the content to a platform-neutral view tree so an iOS or
// Android client can build native views from it (no HTML, no WebView).
func writeNative(w http.ResponseWriter, title string, content template.HTML) {
	tree, err := ParseView(string(content))
	if err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]any{"title": title, "tree": tree})
}
