package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"facet/internal/compile"
	"facet/internal/ir"
)

// `facet routes <file.fct> [--json] [--all]` — the whole surface this app
// exposes, in one screen.
//
// An app author writes views, entities and actions; the runtime turns those
// into an HTTP surface that is nowhere written down. A route table is the
// question every framework user asks first ("what does this thing serve, and
// what guards it?"), and here it is not authored at all — it is derived,
// exactly, from the IR the compiler already produced:
//
//	web      one route per `view`, with its :params and its `requires` guard
//	api      GET /api/<Entity> per entity, POST /api/<Action> per SERVER action
//	webhook  POST <path> per declared `webhook`
//	runtime  the fixed endpoints every Facet app serves (with --all)
//
// Nothing is looked up in the runtime and nothing is guessed: a client-placed
// action is not callable over the API, so it is not listed as one, and the IR
// says which is which.

// RouteEntry is one served endpoint.
type RouteEntry struct {
	Kind     string   `json:"kind"`               // web | api | webhook | runtime
	Method   string   `json:"method"`             // GET, POST, GET/POST, …
	Path     string   `json:"path"`               // the URL pattern
	Name     string   `json:"name,omitempty"`     // the view/entity/action it comes from
	Requires string   `json:"requires,omitempty"` // guarding policy, when there is one
	Params   []string `json:"params,omitempty"`   // :param names, in path order
	Note     string   `json:"note,omitempty"`     // what it is, for the human table
	Shadowed string   `json:"shadowed,omitempty"` // set when a runtime built-in wins this path instead
}

// RouteReport is `facet routes --json`.
type RouteReport struct {
	App    string       `json:"app"`
	Routes []RouteEntry `json:"routes"`
}

// cmdRoutes compiles the app and prints its route table.
func cmdRoutes(args []string) int {
	file := firstNonFlag(args)
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: facet routes <file.fct> [--json] [--all]")
		return 2
	}
	jsonOut := hasFlag(args, "--json", "-json")
	graph, err := compile.File(file)
	if err != nil {
		if jsonOut {
			out, _ := json.MarshalIndent(diagnosticsFor(file, err), "", "  ")
			fmt.Fprintln(os.Stderr, string(out))
		} else {
			fmt.Fprintf(os.Stderr, "facet: %s\n", err)
		}
		return 1
	}
	report := RouteReport{App: graph.App, Routes: buildRoutes(graph, hasFlag(args, "--all", "-all"))}
	if jsonOut {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		writeRoutesText(os.Stdout, report)
	}
	return 0
}

// buildRoutes derives every endpoint from the IR. withRuntime adds the fixed
// endpoints the runtime serves for every app — true information, but the same
// for every project, so it is behind --all rather than in the way.
func buildRoutes(g *ir.IR, withRuntime bool) []RouteEntry {
	var out []RouteEntry

	// Web: one per view. Pages carry the guard and the params; Routes is the
	// same set without the node trees, so Pages is the richer source.
	//
	// A view's path is checked against the runtime's own fixed endpoints
	// (reservedRuntimePaths, below) before it is trusted: the mux those
	// endpoints are registered on always prefers the more specific pattern over
	// the app's catch-all page handler, in every case, regardless of what order
	// either side registers in. A view at /admin (or under /admin/) is never
	// dispatched — the server answers with the generated admin console instead
	// — and `facet routes` reporting it as served would be the same lie that
	// cost the storefront app real time.
	for _, p := range g.Pages {
		note := ""
		if p.Screen {
			note = "composed screen"
		}
		entry := RouteEntry{
			Kind: "web", Method: "GET", Path: p.Path, Name: p.Name,
			Requires: p.Requires, Params: p.Params, Note: note,
		}
		if r, hit := shadowingRuntimePath(p.Path); hit {
			entry.Shadowed = r.describe()
		}
		out = append(out, entry)
	}

	// API: the JSON projection of the same graph. An entity is a collection to
	// read; a server-placed action is an endpoint to invoke. A client-placed
	// action runs in the browser and is deliberately not callable here.
	for _, e := range g.Entities {
		if authManaged(g, e.Name) {
			continue // the runtime owns this table and never projects it as app data
		}
		out = append(out, RouteEntry{
			Kind: "api", Method: "GET", Path: "/api/" + e.Name, Name: e.Name,
			Note: fmt.Sprintf("list rows (%d fields; ?by=&desc=&limit=&<field>=)", len(e.Fields)),
		})
	}
	for _, a := range g.Actions {
		if a.Placement != ir.Server {
			continue
		}
		out = append(out, RouteEntry{
			Kind: "api", Method: "POST", Path: "/api/" + a.Name, Name: a.Name,
			Requires: requireNames(a.Requires), Note: signature(a),
		})
	}

	// Webhooks: inbound endpoints external systems POST to, each authenticated
	// with its own secret and running one action with system authority.
	for _, w := range g.Webhooks {
		out = append(out, RouteEntry{
			Kind: "webhook", Method: "POST", Path: w.Path, Name: w.Action,
			Note: "HMAC-authenticated; runs " + w.Action,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return kindRank(out[i].Kind) < kindRank(out[j].Kind)
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})

	if withRuntime {
		out = append(out, runtimeRoutes(g)...)
	}
	return out
}

// reservedRuntimePath is one path pattern the runtime's own mux registers for
// every app (runtime/server.go, func (s *Server) Handler()), independent of
// anything the app declares. A Prefix pattern matches that path and everything
// under it — the same rule Go's http.ServeMux applies to a pattern ending in
// "/" — and, exactly like ServeMux, it is matched by specificity, never by
// registration order: the app's own catch-all page handler is mounted at "/",
// the least specific pattern there is, so every one of these always wins over
// it, with no warning from the runtime.
//
// This is the single copy of that list. `facet routes --all` renders it as the
// human-readable "runtime" rows, and shadowingRuntimePath cross-checks every
// declared view against the very same data, so the two can never drift apart —
// exactly what the task that added this file asked this package to avoid.
type reservedRuntimePath struct {
	Pattern string // mux registration pattern: "/x" (exact) or "/x/" (prefix)
	Method  string
	Note    string
	Cond    string // "" if always registered; else the condition that gates it
}

// reservedRuntimePaths is sourced from runtime/server.go's Handler(), read-only
// to this package (fct/cmd owns this file; fct/runtime does not). Update it
// only by re-reading that registration list, never by guessing.
func reservedRuntimePaths() []reservedRuntimePath {
	return []reservedRuntimePath{
		{Pattern: "/facet.js", Method: "GET", Note: "the client runtime"},
		{Pattern: "/event", Method: "POST", Note: "client → authority event transport"},
		{Pattern: "/live", Method: "GET", Note: "server-sent events: durable-state changes"},
		{Pattern: "/region", Method: "GET", Note: "server-sent region fragment"},
		{Pattern: "/api", Method: "GET", Note: "the API schema: entities, invocable actions, derives"},
		{Pattern: "/api/", Method: "GET/POST", Note: "per-entity and per-action API routes"},
		{Pattern: "/upload", Method: "POST", Note: "file upload"},
		{Pattern: "/upload/", Method: "POST", Note: "resumable upload (init/chunk/finish/abort)"},
		{Pattern: "/uploads/", Method: "GET", Note: "uploaded media"},
		{Pattern: "/admin", Method: "GET/POST", Note: "generated admin console (set FACET_ADMIN=0 to disable)"},
		{Pattern: "/admin/", Method: "GET/POST", Note: "generated admin console (set FACET_ADMIN=0 to disable)"},
		{Pattern: "/billing/webhook", Method: "POST", Note: "billing provider webhook"},
		{Pattern: "/healthz", Method: "GET", Note: "liveness probe"},
		{Pattern: "/readyz", Method: "GET", Note: "readiness probe"},
		{Pattern: "/metrics", Method: "GET", Note: "Prometheus scrape target"},
		{Pattern: "/dev/reload", Method: "GET", Note: "dev-mode live reload", Cond: "facet dev / facet serve --dev only"},
		{Pattern: "/auth/oidc/login", Method: "GET", Note: "OIDC sign-in redirect", Cond: "only when OIDC is configured"},
		{Pattern: "/auth/oidc/callback", Method: "GET", Note: "OIDC callback", Cond: "only when OIDC is configured"},
	}
}

// shadows reports whether path is one the mux would route to this pattern's
// handler rather than to the app's own "/" catch-all — i.e. whether an app view
// declared at path is unreachable. It mirrors http.ServeMux's own rule: a
// prefix pattern (trailing "/") matches itself and everything beneath it; an
// exact pattern matches only that literal path.
func (r reservedRuntimePath) shadows(path string) bool {
	if strings.HasSuffix(r.Pattern, "/") {
		return path == r.Pattern || strings.HasPrefix(path, r.Pattern)
	}
	return path == r.Pattern
}

// describe renders one line naming which built-in wins and why, for the loud,
// by-name warning `facet routes` and `facet doctor` both print.
func (r reservedRuntimePath) describe() string {
	if r.Cond != "" {
		return fmt.Sprintf("the runtime's built-in %s (%s; %s)", r.Pattern, r.Note, r.Cond)
	}
	return fmt.Sprintf("the runtime's built-in %s (%s)", r.Pattern, r.Note)
}

// shadowingRuntimePath returns the first reserved runtime pattern that would
// intercept path before the app's view router ever saw it, if any.
func shadowingRuntimePath(path string) (reservedRuntimePath, bool) {
	for _, r := range reservedRuntimePaths() {
		if r.shadows(path) {
			return r, true
		}
	}
	return reservedRuntimePath{}, false
}

// runtimeRoutes are the endpoints the server mounts for every app, regardless
// of what the app declares. They are listed last and only with --all: they are
// real, and they are the same everywhere. Rendered straight from
// reservedRuntimePaths so this list and the shadow check above can never say
// two different things about what the runtime serves.
func runtimeRoutes(g *ir.IR) []RouteEntry {
	var rs []RouteEntry
	for _, r := range reservedRuntimePaths() {
		note := r.Note
		if r.Cond != "" {
			note += " (" + r.Cond + ")"
		}
		rs = append(rs, RouteEntry{Kind: "runtime", Method: r.Method, Path: r.Pattern, Note: note})
	}
	return rs
}

func kindRank(kind string) int {
	switch kind {
	case "web":
		return 0
	case "api":
		return 1
	case "webhook":
		return 2
	}
	return 3
}

// authManaged reports whether an entity is the identity table `auth` injects
// rather than one the author declared. The runtime never projects it over the
// public API or the live stream — it is read through /admin and the dedicated
// endpoints — so listing GET /api/FacetUser would be a lie.
func authManaged(g *ir.IR, name string) bool {
	return g.Auth && name == authUserEntity
}

// authUserEntity is the name of the managed identity table. `auth` is the only
// thing that puts it in a compiled graph; the runtime's other reserved tables
// (tenancy, billing) are injected at server construction and never appear in an
// IR, so they cannot reach this file.
const authUserEntity = "FacetUser"

// requireNames renders an action's policy guards as one comma-separated string.
func requireNames(reqs []ir.Require) string {
	if len(reqs) == 0 {
		return ""
	}
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Name
		if len(r.Args) > 0 {
			names[i] += "(…)"
		}
	}
	return strings.Join(names, ", ")
}

// signature renders an action's parameter list, e.g. `publish(id: int)` — what
// goes in the POST body's `args` array, in order.
func signature(a ir.Action) string {
	parts := make([]string, len(a.Params))
	for i, p := range a.Params {
		t := p.Type
		if p.Optional {
			t += "?"
		}
		parts[i] = p.Name + ": " + t
	}
	return a.Name + "(" + strings.Join(parts, ", ") + ")"
}

// writeRoutesText prints the table in the same columnar house style as
// `facet inspect` and `facet explain`.
func writeRoutesText(w io.Writer, r RouteReport) {
	fmt.Fprintf(w, "%s — every route this app serves\n", r.App)
	if len(r.Routes) == 0 {
		fmt.Fprintln(w, "\n  (no routes — this graph declares no views, entities or actions)")
		return
	}
	// Column widths from the content, so a long path does not wrap the table.
	methodW, pathW := 6, 4
	for _, e := range r.Routes {
		methodW = max(methodW, len(e.Method))
		pathW = max(pathW, len(e.Path))
	}
	kind, guarded, shadowed := "", false, false
	for _, e := range r.Routes {
		guarded = guarded || e.Requires != ""
		shadowed = shadowed || e.Shadowed != ""
		if e.Kind != kind {
			kind = e.Kind
			fmt.Fprintf(w, "\n%s\n", strings.ToUpper(kind)+" "+kindLegend(kind))
		}
		right := e.Name
		if e.Note != "" {
			if right != "" {
				right += "  — " + e.Note
			} else {
				right = e.Note
			}
		}
		fmt.Fprintf(w, "  %-*s %-*s  %s\n", methodW, e.Method, pathW, e.Path, right)
		if e.Requires != "" {
			fmt.Fprintf(w, "  %-*s %-*s  requires %s\n", methodW, "", pathW, "", e.Requires)
		}
		if e.Shadowed != "" {
			fmt.Fprintf(w, "  %-*s %-*s  WARNING: never served — shadowed by %s\n", methodW, "", pathW, "", e.Shadowed)
		}
	}
	if guarded {
		fmt.Fprintln(w, "\n  a guarded route is refused by the authority, and the client hides every link to it")
	}
	if shadowed {
		fmt.Fprintln(w, "\n  a SHADOWED route is dead: the runtime's built-in endpoint answers that path first and this")
		fmt.Fprintln(w, "  view is never reached. Move it to a path none of the runtime's fixed endpoints own (facet")
		fmt.Fprintln(w, "  routes --all lists them), or expect this app's visitors to see the built-in instead.")
	}
}

func kindLegend(kind string) string {
	switch kind {
	case "web":
		return "(the rendered projection — one route per view)"
	case "api":
		return "(the JSON projection — entities to read, server actions to invoke)"
	case "webhook":
		return "(inbound: external systems POST here)"
	case "runtime":
		return "(served for every Facet app)"
	}
	return ""
}
