package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/internal/registry"
	"facet/runtime"
)

// `facet doctor [file.fct]` — is this project set up correctly, and if not,
// what exactly is wrong?
//
// Every one of these failures already had a way to be discovered: an obscure
// parse cascade when the manifest pins a newer toolchain, a connection error
// three commands later when the datastore is unreachable, a secret that was
// never set and quietly became ephemeral. `doctor` asks all of them at once,
// before anything is running, and answers with the fix.
//
// It delegates every judgement rather than re-deriving it — the toolchain range
// is checked by registry.CheckToolchainRange (the same call the compiler makes
// on every build), the configuration by runtime.Config.Warnings (the same list
// `facet config` prints), the datastore by opening it through the runtime's own
// Migrate dry-run. There is no second opinion about anything here, only one
// place that asks.

// checkState is how a single check came out.
type checkState int

const (
	statusOK checkState = iota
	statusWarn
	statusFail
)

func (s checkState) mark() string {
	switch s {
	case statusWarn:
		return "!"
	case statusFail:
		return "✗"
	}
	return "✓"
}

// check is one diagnosis: what was examined, what was found, and — when
// something is wrong — what to do about it.
type check struct {
	State  checkState
	Name   string
	Detail string
	Fix    string
}

// doctorOpts is what there is to diagnose. Either a source tree (entry, which is
// compiled here) or an already-compiled graph (what a release binary carries and
// what `facet serve` holds) — the checks that read a project's files are simply
// absent in the second case, because there are no files to read.
type doctorOpts struct {
	entry      string // source entry file, when there is a source tree
	subject    string // what to name in the header (defaults to entry)
	graph      *ir.IR // an already-compiled graph — set by a built artifact
	production bool   // judge this as a deployment, not as a working copy
}

// cmdDoctor runs every check and returns a process exit code: 0 when nothing
// failed (warnings are not failures — a dev machine legitimately has no secret
// and no database), 1 when something is actually broken.
//
// `--production` changes the standard, not the checks: the configuration
// warnings that are acceptable on a laptop become failures, and the deployment
// posture (durability, TLS, the secrets this graph needs, the datastore
// identity's privilege, the admin console) is examined as well.
func cmdDoctor(args []string) int {
	entry := firstNonFlag(args)
	if entry == "" {
		entry = soleEntry(".")
	}
	return runDoctor(doctorOpts{
		entry:      entry,
		production: hasFlag(args, "--production", "-production", "--prod"),
	})
}

// runDoctor is the diagnosis itself, shared by `facet doctor` and by a built
// artifact's own `doctor` — one list of checks, whether or not a source tree
// exists.
func runDoctor(o doctorOpts) int {
	var checks []check
	checks = append(checks, checkToolchain())

	graph := o.graph
	source := graph == nil
	dir := "."
	if source {
		if o.entry != "" {
			dir = filepath.Dir(o.entry)
		}
		checks = append(checks, checkManifest(dir))
		checks = append(checks, checkLock(dir)...)
		g, compiled := checkCompiles(o.entry)
		graph, checks = g, append(checks, compiled)
	} else {
		checks = append(checks, check{statusOK, "app",
			fmt.Sprintf("%s: %d entities, %d actions, %d views (compiled in — no source needed)",
				graph.App, len(graph.Entities), len(graph.Actions), len(graph.Pages)), ""})
	}

	checks = append(checks, checkRouteShadowing(graph)...)
	checks = append(checks, checkDatastore(graph))
	if o.production {
		checks = append(checks, productionChecks(graph, false)...)
	} else {
		checks = append(checks, checkConfig(false)...)
	}
	if source {
		checks = append(checks, checkHygiene(dir)...)
	}

	subject := o.subject
	if subject == "" {
		subject = o.entry
	}
	writeDoctor(os.Stdout, subject, o.production, checks)
	for _, c := range checks {
		if c.State == statusFail {
			return 1
		}
	}
	return 0
}

// checkToolchain reports the running compiler. It is never a failure; it is the
// number every other check is relative to.
func checkToolchain() check {
	return check{statusOK, "toolchain", "facet " + registry.ToolchainVersion, ""}
}

// checkManifest validates the project's own facet.json against the running
// toolchain — the same range check every remote import gets, run early and
// explained, instead of surfacing as a parse cascade on the next build.
func checkManifest(dir string) check {
	path := filepath.Join(dir, "facet.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return check{statusWarn, "manifest", "no facet.json — nothing pins the toolchain this project needs",
			"facet new writes one; add {\"facet\": \">=" + registry.ToolchainVersion + "\"}"}
	}
	if err != nil {
		return check{statusFail, "manifest", err.Error(), ""}
	}
	m, err := registry.ParseManifest(b)
	if err != nil {
		return check{statusFail, "manifest", err.Error(), "fix the JSON in facet.json"}
	}
	if m.Facet == "" {
		return check{statusWarn, "manifest", "facet.json has no `facet` range — the toolchain is unpinned",
			"add \"facet\": \">=" + registry.ToolchainVersion + "\" to facet.json"}
	}
	subject := m.Name
	if subject == "" {
		subject = "this project"
	}
	if err := registry.CheckToolchainRange(m.Facet, subject); err != nil {
		return check{statusFail, "manifest", err.Error(), "upgrade the toolchain, or relax the `facet` range in facet.json"}
	}
	return check{statusOK, "manifest", fmt.Sprintf("facet.json pins facet %s — satisfied by %s", m.Facet, registry.ToolchainVersion), ""}
}

// checkLock reports the pinned remote facets. A lock that names modules is the
// reproducibility guarantee; a branch pin is not one, and says so.
func checkLock(dir string) []check {
	lock, err := registry.LoadLock(dir)
	if err != nil {
		return []check{{statusFail, "facet.lock", err.Error(), "fix or delete facet.lock, then run `facet get`"}}
	}
	if len(lock.Modules) == 0 {
		return []check{{statusOK, "dependencies", "no remote facets — this project builds offline", ""}}
	}
	keys := make([]string, 0, len(lock.Modules))
	for k := range lock.Modules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []check{{statusOK, "dependencies",
		fmt.Sprintf("%d remote facet(s) pinned in facet.lock (written by facet %s)", len(keys), lock.Facet), ""}}
	for _, k := range keys {
		m := lock.Modules[k]
		state, fix := statusOK, ""
		// A tag is an immutable release; a branch name is a moving target that
		// makes the next fetch a different build.
		if !strings.HasPrefix(m.Version, "v") {
			state = statusWarn
			fix = "facet add " + k + "@<vX.Y.Z> — pin a release, not a branch"
		}
		out = append(out, check{state, "  " + k, m.Version + " (" + short(m.Commit) + ")", fix})
	}
	return out
}

// checkCompiles is the one check that is really the whole toolchain: it runs
// the compiler. It also returns the graph, so the datastore check can be about
// this app's schema rather than about a connection in the abstract.
func checkCompiles(entry string) (*ir.IR, check) {
	if entry == "" {
		return nil, check{statusWarn, "app", "no entry file found here",
			"run `facet doctor <file.fct>`, or name it `app.fct`"}
	}
	graph, err := compile.File(entry)
	if err != nil {
		line, msg := errLocation(err)
		where := entry
		if line > 0 {
			where = fmt.Sprintf("%s:%d", entry, line)
		}
		return nil, check{statusFail, "app", where + ": " + msg, "facet check " + entry + " — fix the source"}
	}
	return graph, check{statusOK, "app", fmt.Sprintf("%s compiles: %d entities, %d actions, %d views",
		entry, len(graph.Entities), len(graph.Actions), len(graph.Pages)), ""}
}

// checkDatastore actually opens the datastore and reconciles this app's schema
// as a dry run — the real connection, with nothing written. An unreachable
// store is a warning, not a failure: `facet dev` runs on an in-memory store on
// purpose, and that is the common case on a laptop.
func checkDatastore(graph *ir.IR) check {
	url := os.Getenv("FACET_DATABASE_URL")
	where := runtime.StoreDescription("")
	if graph == nil {
		return check{statusWarn, "datastore", where + " — not probed (the app did not compile)", ""}
	}
	plan, err := runtime.Migrate(graph, false)
	if err != nil {
		state, fix := statusWarn, "start the datastore, or set FACET_DATABASE_URL — `facet dev` works without one (in-memory)"
		if url != "" {
			// An explicitly configured store that cannot be reached is a real
			// misconfiguration, not a laptop default.
			state = statusFail
			fix = "check FACET_DATABASE_URL and that the datastore is running"
		}
		return check{state, "datastore", where + " unreachable: " + err.Error(), fix}
	}
	if len(plan) > 0 {
		return check{statusWarn, "datastore",
			fmt.Sprintf("%s reachable, but the schema is %d change(s) behind the app", where, len(plan)),
			"facet migrate <file.fct>   (--plan to see the statements first)"}
	}
	return check{statusOK, "datastore", where + " reachable, schema up to date", ""}
}

// checkConfig reports the runtime configuration exactly as `facet config`
// judges it — one check per warning it raises, so each has its own fix line.
//
// production raises every one of them from a warning to a failure. That is the
// whole difference between the two modes here: the list of problems is the
// runtime's, unchanged; what changes is whether an unset FACET_SECRET is a note
// on a laptop or a reason not to boot.
func checkConfig(production bool) []check {
	cfg := runtime.ResolveConfig()
	warnings := cfg.Warnings()
	state := statusWarn
	if production {
		state = statusFail
	}
	out := make([]check, 0, len(warnings))
	for _, w := range warnings {
		if staleStoreWarning(w, cfg.DatabaseURL) {
			continue
		}
		// Config.Warnings writes "<problem> — <what to do>." — split on the first
		// sentence so the fix lands in the fix column.
		detail, fix := w, ""
		if i := strings.Index(w, " — "); i >= 0 {
			detail, fix = w[:i], strings.TrimSpace(w[i+len(" — "):])
		}
		out = append(out, check{state, "config", detail, fix})
	}
	if len(out) == 0 {
		return []check{{statusOK, "config", "complete and production-safe", ""}}
	}
	return out
}

// staleStoreWarning drops the one configuration warning that two parts of the
// runtime disagree about: Config.Warnings still says FACET_DATABASE_URL must be
// a postgres:// URL, while the store layer (openStore / StoreDescription) makes
// FacetQL the stack's native and default backend. Reporting a facetql:// URL as
// a misconfiguration would refuse to boot the datastore the project is built on.
//
// Nothing is judged here that the runtime does not already judge — this only
// declines to repeat a line the runtime's own store layer contradicts.
func staleStoreWarning(warning, url string) bool {
	return strings.HasPrefix(url, "facetql://") && strings.Contains(warning, "not a postgres:// URL")
}

// ── production readiness ─────────────────────────────────────────────────────
//
// These are the checks that only mean something about a *deployment*: they ask
// where the rows will still be after a restart, whether the session cookie can
// be read off the wire, whether the secrets this particular graph needs are
// present, what the datastore thinks this app is allowed to do, and what is
// exposed at /admin. They are printed by `facet doctor --production`, by a built
// artifact's `doctor`, and run as `facet serve`'s preflight — one list, so a
// deployment that passes the command that prints it passes the one that boots it.

// productionChecks is the deployment posture of this graph in this environment.
// memory says the caller asked, explicitly, to run against the volatile
// in-memory store — in which case the datastore configuration is not a finding,
// because there is no datastore in play. Nothing else about the deployment is
// relaxed by it: the volatility itself is reported, loudly, by checkDurability.
func productionChecks(graph *ir.IR, memory bool) []check {
	var out []check
	out = append(out, checkDurability(memory))
	// --memory is a statement that this run is a smoke test or a demo, not a
	// deployment — so the configuration warnings stay warnings for it. Escalating
	// them anyway would make the honest way to run a local artifact require
	// --force, and a --force typed by habit disables the checks that do matter.
	for _, c := range checkConfig(!memory) {
		if memory && strings.Contains(c.Detail, "FACET_DATABASE_URL") {
			continue
		}
		out = append(out, c)
	}
	out = append(out, checkTLS(graph)...)
	out = append(out, checkSecrets(graph)...)
	if c, ok := checkStoreIdentity(); ok {
		out = append(out, c)
	}
	if c, ok := checkAdminConsole(graph); ok {
		out = append(out, c)
	}
	return out
}

// checkDurability answers the only question that matters about a store on the
// day after a deploy: is anything written to it still there tomorrow?
//
// The in-memory store is not a lesser database, it is a volatile one — `facet
// dev` chooses it silently when nothing is configured, and that silence is why a
// demo's rows vanish on restart. Here it is never silent and never implicit: it
// is reached only by typing --memory, so this reports it rather than blocks it —
// the operator has already said the word, and a second flag to confirm the first
// one teaches people to type the confirmation without reading it.
func checkDurability(memory bool) check {
	if memory {
		return check{statusWarn, "durability", "the in-memory store is in use (--memory) — VOLATILE: every row is lost when this process exits",
			"for anything but a smoke test or a demo, drop --memory and set FACET_DATABASE_URL"}
	}
	url := os.Getenv("FACET_DATABASE_URL")
	if url == "" {
		return check{statusWarn, "durability", "no FACET_DATABASE_URL — the store defaults to facetql://localhost:8080, which does not exist inside most containers",
			"name the datastore explicitly: FACET_DATABASE_URL=facetql://<host>:8080"}
	}
	return check{statusOK, "durability", "rows persist to " + runtime.StoreDescription("") + " (" + redactedURL(url) + ")", ""}
}

// checkTLS examines everything about transport security that the configuration
// warnings do not already cover.
//
// The first thing to be honest about: this runtime terminates no TLS. Serve is
// an HTTP listener with production timeouts, so a Facet app in production is
// always behind something that terminates TLS — a load balancer, an ingress, a
// reverse proxy — and "TLS is configured" means the app is configured to behave
// as if it is behind one. The two ways to get that wrong that the app itself can
// see are a cookie that is allowed onto a plaintext hop (FACET_SECURE_COOKIES,
// judged by the configuration warnings) and a URL this app itself will call or
// redirect to over http://, which is checked here from the graph.
func checkTLS(graph *ir.IR) []check {
	out := []check{{statusOK, "tls", "this process serves plain HTTP — terminate TLS in front of it (proxy, ingress or load balancer) and set FACET_SECURE_COOKIES=1", ""}}
	if redirect := os.Getenv("FACET_OIDC_REDIRECT"); strings.HasPrefix(redirect, "http://") && !isLoopback(redirect) {
		out = append(out, check{statusFail, "tls", "FACET_OIDC_REDIRECT is http:// — the identity provider will return the authorization code over plaintext",
			"use the https:// address of the public endpoint"})
	}
	if graph != nil {
		for _, s := range graph.Services {
			if strings.HasPrefix(s.URL, "http://") && !isLoopback(s.URL) {
				out = append(out, check{statusWarn, "tls",
					fmt.Sprintf("service %s calls out over plaintext (%s)", s.Name, s.URL),
					"point the service at its https:// endpoint"})
			}
		}
	}
	return out
}

// checkSecrets asks for the secrets THIS graph needs, which is a different
// question from whether the master secret is set: a webhook names the
// environment variable holding its HMAC key, an OIDC issuer implies a client
// secret, billing implies a signing secret for the provider's callbacks. Each
// one that is missing has a working fallback or a disabled feature behind it,
// so none of them is loud at runtime — which is exactly why they are asked for
// here, before the deployment discovers it in a webhook that authenticates
// nothing an external system can actually sign.
func checkSecrets(graph *ir.IR) []check {
	var out []check
	if graph != nil {
		for _, w := range graph.Webhooks {
			if w.Secret == "" {
				out = append(out, check{statusWarn, "secrets",
					fmt.Sprintf("webhook %s names no secret env var — its HMAC key is derived from FACET_SECRET", w.Path),
					"declare the secret in the app so the sender and this app share a key you can rotate"})
				continue
			}
			if os.Getenv(w.Secret) == "" {
				out = append(out, check{statusFail, "secrets",
					fmt.Sprintf("webhook %s expects %s and it is not set — every signature is checked against a fallback key the sender does not have", w.Path, w.Secret),
					"set " + w.Secret + " to the key the sender signs with"})
			}
		}
	}
	if os.Getenv("FACET_OIDC_ISSUER") != "" {
		for _, name := range []string{"FACET_OIDC_CLIENT_ID", "FACET_OIDC_CLIENT_SECRET", "FACET_OIDC_REDIRECT"} {
			if os.Getenv(name) == "" {
				out = append(out, check{statusFail, "secrets",
					"FACET_OIDC_ISSUER is set but " + name + " is not — single sign-on will not complete",
					"set " + name + ", or unset FACET_OIDC_ISSUER"})
			}
		}
	}
	if os.Getenv("FACET_BILLING") != "" && os.Getenv("FACET_BILLING") != "0" && os.Getenv("FACET_BILLING_WEBHOOK_SECRET") == "" {
		out = append(out, check{statusFail, "secrets",
			"billing is on but FACET_BILLING_WEBHOOK_SECRET is not set — provider callbacks cannot be authenticated",
			"set FACET_BILLING_WEBHOOK_SECRET to the provider's signing secret"})
	}
	if len(out) == 0 {
		return []check{{statusOK, "secrets", "every secret this graph names is present", ""}}
	}
	return out
}

// checkStoreIdentity asks FacetQL what this app's token is allowed to do.
//
// The privilege matters because the two admin-only endpoints (/admin/indexes and
// /admin/references) are the ones `facet migrate` uses to declare access paths
// and cascade rules — a *build-time* privilege. An app that serves traffic needs
// none of it (see fqStore.Init, which starts anyway when it cannot reconcile),
// so a serving app holding an admin identity is credential excess: a token that
// leaks out of a running web process can drop every index and every referential
// rule in the datastore.
//
// The probe is one authenticated GET, and its status code is the whole answer:
// 200 means this identity is an admin, 401/403 means it is not. The second
// return reports whether there was anything to check at all.
func checkStoreIdentity() (check, bool) {
	raw := os.Getenv("FACET_DATABASE_URL")
	if !strings.HasPrefix(raw, "facetql://") {
		return check{}, false // Postgres privilege is the database's own to answer
	}
	base, token, err := facetQLEndpoint(raw)
	if err != nil {
		return check{statusWarn, "store identity", err.Error(), "FACET_DATABASE_URL=facetql://[token@]host:port"}, true
	}
	if token == "" {
		return check{statusWarn, "store identity", "no FacetQL token — this app connects unauthenticated",
			"set FACETQL_TOKEN, or put the token in FACET_DATABASE_URL (facetql://<token>@host:port)"}, true
	}
	req, err := http.NewRequest(http.MethodGet, base+"/admin/indexes", nil)
	if err != nil {
		return check{statusWarn, "store identity", err.Error(), ""}, true
	}
	req.Header.Set("x-api-key", token)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return check{statusWarn, "store identity", "could not ask FacetQL what this token may do: " + err.Error(),
			"start the datastore, then run this again"}, true
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return check{statusWarn, "store identity",
			"this app's FacetQL token is an ADMIN identity — /admin/indexes answered it; a leak from this process can drop every index and cascade rule",
			"serve with a non-admin token and keep the admin one for `migrate` (it is the only step that declares indexes and references)"}, true
	case http.StatusUnauthorized, http.StatusForbidden:
		return check{statusOK, "store identity", "least privilege: the token serves data and is not a FacetQL admin", ""}, true
	default:
		return check{statusWarn, "store identity",
			fmt.Sprintf("FacetQL answered %d when asked about this token's privilege", resp.StatusCode), ""}, true
	}
}

// facetQLEndpoint splits a facetql:// URL into the HTTP base URL and the token,
// falling back to FACETQL_TOKEN — the same two places the store reads them from.
func facetQLEndpoint(raw string) (base, token string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("FACET_DATABASE_URL %q is not a URL: %w", raw, err)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("FACET_DATABASE_URL %q has no host (expected facetql://[token@]host:port)", raw)
	}
	scheme := "http"
	if u.Query().Get("tls") == "1" {
		scheme = "https"
	}
	if u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			token = pw
		} else {
			token = u.User.Username()
		}
	}
	if token == "" {
		token = os.Getenv("FACETQL_TOKEN")
	}
	return scheme + "://" + u.Host, token, nil
}

// checkRouteShadowing reports any declared view whose path the runtime's own
// fixed endpoints already own outright — the exact defect that cost the
// storefront app real time: it wrote a view at /admin, the runtime's generated
// admin console answers that path unconditionally (runtime/server.go registers
// it ahead of the app's catch-all page handler, regardless of --production or
// whether `auth` is even declared), and the view was silently never served.
// `facet routes` marks the same thing per-route; this is that finding folded
// into the one place doctor asks every question, run whether or not the app
// even declares `auth` and whether or not --production was passed — a dead
// route is a real defect in dev too, not only in prod.
//
// The reserved-path list itself lives once, in routes.go, sourced from
// runtime/server.go's Handler() (read-only to this package); this check reuses
// it rather than keeping a second opinion about what the runtime serves.
func checkRouteShadowing(graph *ir.IR) []check {
	if graph == nil {
		return nil
	}
	var out []check
	for _, p := range graph.Pages {
		r, hit := shadowingRuntimePath(p.Path)
		if !hit {
			continue
		}
		out = append(out, check{statusFail, "routes",
			fmt.Sprintf("view %q at %s is never served — %s", p.Name, p.Path, r.describe()),
			"move this view to a path none of the runtime's fixed endpoints own (`facet routes --all` lists them)"})
	}
	return out
}

// checkAdminConsole reports the generated admin dashboard, which is served by
// default. It is gated on an admin session and CSRF-checked, so this is not a
// hole — but it is a full read/write surface over every table, reachable from
// the internet, that a deployment may not have decided to expose.
func checkAdminConsole(graph *ir.IR) (check, bool) {
	if graph == nil || !graph.Auth {
		return check{}, false // without `auth` there are no admin sessions to gate it
	}
	if !runtime.AdminEnabled() {
		return check{statusOK, "admin console", "/admin is off (FACET_ADMIN=0)", ""}, true
	}
	return check{statusWarn, "admin console",
		"/admin serves a read/write dashboard over every entity (admin session required)",
		"set FACET_ADMIN=0 if this deployment should not expose it"}, true
}

// isLoopback reports whether a URL points back at this machine, where plaintext
// is not a transport risk.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// redactedURL hides a datastore URL's credentials while keeping its host, so a
// diagnosis can be pasted into an issue. The userinfo is cut out textually
// rather than replaced through net/url, which would percent-encode the mask and
// print facetql://%2A%2A%2A@host — technically a redaction, and unreadable.
func redactedURL(raw string) string {
	scheme := strings.Index(raw, "://")
	at := strings.LastIndex(raw, "@")
	if scheme < 0 || at < scheme {
		return raw
	}
	return raw[:scheme+3] + "***@" + raw[at+1:]
}

// checkHygiene catches the two mistakes that are only ever noticed after they
// have been committed: a .env that is not ignored, and a facet.lock that is.
func checkHygiene(dir string) []check {
	path := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(path)
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, ".env")); statErr == nil {
			return []check{{statusFail, "hygiene", "a .env is present and there is no .gitignore — the secret is one commit from being public",
				"add a .gitignore with `.env` in it"}}
		}
		return []check{{statusWarn, "hygiene", "no .gitignore", "add one (facet new writes one)"}}
	}
	var out []check
	lines := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	if !lines[".env"] {
		out = append(out, check{statusFail, "hygiene", ".gitignore does not ignore .env — FACET_SECRET can be committed",
			"add `.env` to .gitignore"})
	}
	if lines["facet.lock"] {
		out = append(out, check{statusFail, "hygiene", ".gitignore ignores facet.lock — a fresh clone will not resolve the same facet bytes",
			"remove `facet.lock` from .gitignore and commit the lock"})
	}
	if len(out) == 0 {
		out = append(out, check{statusOK, "hygiene", ".gitignore covers .env and keeps facet.lock", ""})
	}
	return out
}

// short renders a commit SHA at the length a human reads.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// writeDoctor prints the diagnosis in the columnar house style, then a verdict.
func writeDoctor(w io.Writer, subject string, production bool, checks []check) {
	if subject == "" {
		subject = "this directory"
	}
	mode := ""
	if production {
		mode = " (production)"
	}
	fmt.Fprintf(w, "facet doctor%s — %s\n\n", mode, subject)
	warn, fail := writeChecks(w, checks)
	fmt.Fprintln(w)
	switch {
	case fail > 0:
		fmt.Fprintf(w, "%d problem(s), %d warning(s) — fix the ✗ lines above.\n", fail, warn)
	case warn > 0 && production:
		fmt.Fprintf(w, "nothing blocking, %d warning(s) — each one is a decision this deployment is making.\n", warn)
	case warn > 0:
		fmt.Fprintf(w, "no problems, %d warning(s) — fine for development, read them before you ship.\n", warn)
	case production:
		fmt.Fprintln(w, "ready to ship: nothing to fix.")
	default:
		fmt.Fprintln(w, "healthy: nothing to fix.")
	}
}

// writeChecks prints the check lines themselves and returns how many were
// warnings and how many were failures. `facet serve`'s preflight prints the same
// lines without a header or a verdict, so the two commands cannot come to render
// the same finding differently.
func writeChecks(w io.Writer, checks []check) (warn, fail int) {
	nameW := 10
	for _, c := range checks {
		nameW = max(nameW, len(c.Name))
	}
	for _, c := range checks {
		switch c.State {
		case statusWarn:
			warn++
		case statusFail:
			fail++
		}
		fmt.Fprintf(w, "  %s %-*s  %s\n", c.State.mark(), nameW, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "    %-*s  → %s\n", nameW, "", c.Fix)
		}
	}
	return warn, fail
}
