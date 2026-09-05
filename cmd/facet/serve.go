package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/internal/registry"
	"facet/runtime"
)

// `facet serve` — the production counterpart of `facet dev`.
//
//	facet serve [<file.fct>|<graph.json>] [--port N] [--addr host:port]
//
// The two commands that existed before this one are development commands wearing
// different clothes:
//
//   - `facet dev` watches the source tree, hot-swaps the graph on save, and
//     silently falls back to a volatile in-memory store when no database is
//     configured. Every one of those is wrong in production.
//   - `facet run` is closer — it opens the real store and serves — but it takes
//     its address as a bare positional argument, ignores $PORT (which is how
//     every container platform tells a process where to listen), and boots
//     whatever configuration it is handed without a word about whether that
//     configuration is safe to expose.
//
// `serve` is the missing one: no watcher, no reload, no in-memory fallback, the
// address resolved from flags *or the environment*, and a preflight that runs the
// same production checks `facet doctor --production` prints and refuses to boot
// on a failure. It is also the command a built artifact runs when it is executed
// with no arguments (see release.go), so a deployment's entry point and a
// developer's `facet serve` are the same code path — the thing that runs in
// production is the thing that was tested locally.
//
// It takes a compiled graph as readily as source: a `.json` argument is loaded as
// IR (what `facet build` prints), so serving needs the compiler only if what you
// hand it is source.

// defaultAddr is where a Facet app listens when nothing says otherwise. The
// wildcard host is deliberate: a process in a container that binds 127.0.0.1 is
// unreachable from outside it, which is the single most common first-deploy
// failure.
const defaultAddr = ":7373"

// cmdServe resolves the app to serve — an argument, or the sole entry file in
// this directory — and serves it.
func cmdServe(args []string) int {
	// firstOperand, not firstNonFlag: `facet serve --port 8080` names no file, and
	// the port is not one.
	path := firstOperand(args)
	if path == "" {
		path = soleEntry(".")
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: facet serve <file.fct|graph.json> [--port N] [--addr host:port]")
		fmt.Fprintln(os.Stderr, "  (no argument: the sole .fct in this directory, or app.fct)")
		return 2
	}
	graph, err := loadGraph(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet: %v\n", err)
		return 1
	}
	return serveGraph(graph, args, path)
}

// loadGraph reads an application graph from either form it exists in: source,
// which is compiled, or the IR itself, which is not. A deployment that ships the
// IR (`facet build app.fct > graph.json`) never needs the compiler again — the
// same property the release binary has, in a form that predates it.
func loadGraph(path string) (*ir.IR, error) {
	if strings.HasSuffix(path, ".json") {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		graph := &ir.IR{}
		if err := json.Unmarshal(raw, graph); err != nil {
			return nil, fmt.Errorf("%s is not a compiled graph: %w", path, err)
		}
		if graph.App == "" {
			return nil, fmt.Errorf("%s is JSON but carries no app — is it the output of `facet build`?", path)
		}
		return graph, nil
	}
	graph, err := compile.File(path)
	if err != nil {
		return nil, fmt.Errorf("compile error: %w", err)
	}
	return graph, nil
}

// serveGraph is the production boot: preflight, open the real datastore, start
// the job workers, and serve until a signal drains it. subject is what to call
// this app in the banner (a path, or the artifact's description).
func serveGraph(graph *ir.IR, args []string, subject string) int {
	addr := resolveAddr(args)
	memory := hasFlag(args, "--memory", "-memory")
	force := hasFlag(args, "--force", "-force")

	// Preflight. The checks are doctor's — one definition of "is this deployment
	// safe", printed by `facet doctor --production` and enforced here, so the two
	// can never come to disagree about what production means.
	checks := productionChecks(graph, memory)
	failed := 0
	for _, c := range checks {
		if c.State == statusFail {
			failed++
		}
	}
	if len(checks) > 0 {
		fmt.Fprintln(os.Stderr, "facet serve: preflight")
		writeChecks(os.Stderr, checks)
	}
	if failed > 0 && !force {
		fmt.Fprintf(os.Stderr, "\nfacet serve: refusing to start — %d production check(s) failed above.\n", failed)
		fmt.Fprintln(os.Stderr, "  fix them, or pass --force to start anyway (and read them again first).")
		return 1
	}

	var (
		srv *runtime.Server
		err error
	)
	if memory {
		// The one way to run without a datastore, and it is never the default:
		// `facet dev` chooses the in-memory store silently when none is
		// configured, and that silence is exactly what loses a demo's data. Here
		// it is asked for by name, it is in the preflight above, and it is said
		// again on the line below — three places, none of them silent.
		fmt.Fprintln(os.Stderr, "facet serve: --memory — the in-memory store is VOLATILE; every row is lost when this process exits")
		srv, err = runtime.NewInMemory(graph)
	} else {
		srv, err = runtime.New(graph)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "facet:", err)
		return 1
	}
	srv.StartJobs()

	url := runtime.BrowseURL(addr)
	fmt.Printf("facet %s: serving %s on %s\n", registry.ToolchainVersion, graph.App, url)
	fmt.Printf("  source        %s\n", subject)
	fmt.Printf("  data store    %s\n", storeLabel(memory))
	fmt.Printf("  security      %s\n", runtime.SecurityDescription())
	fmt.Printf("  operations    %s\n", runtime.OpsDescription())
	fmt.Printf("  enterprise    %s\n", runtime.EnterpriseDescription())
	fmt.Printf("  logs          JSON on stderr at %s (FACET_LOG_LEVEL)\n", logLevel())
	fmt.Printf("  routes        %d web, %d api, %d webhook\n", countKind(graph, "web"), countKind(graph, "api"), countKind(graph, "webhook"))
	if runtime.AdminEnabled() {
		fmt.Printf("  admin console %s/admin (FACET_ADMIN=0 removes it)\n", url)
	}
	// Serve blocks until SIGINT/SIGTERM, then drains in-flight requests, stops the
	// job workers and closes the store — what makes a rolling deploy safe.
	if err := srv.Serve(addr); err != nil {
		fmt.Fprintln(os.Stderr, "facet:", err)
		return 1
	}
	return 0
}

// resolveAddr decides where to listen, in the order a deployment expects to be
// obeyed: an explicit flag, then the environment, then the default. $PORT is
// honored because that is how container platforms assign a port, and a process
// that ignores it never receives traffic; $FACET_ADDR is for the deployments
// that need to name an interface as well.
func resolveAddr(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--addr" || a == "-addr":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--addr="):
			return strings.TrimPrefix(a, "--addr=")
		case a == "--port" || a == "-port" || a == "-p":
			if i+1 < len(args) {
				return ":" + strings.TrimPrefix(args[i+1], ":")
			}
		case strings.HasPrefix(a, "--port="):
			return ":" + strings.TrimPrefix(strings.TrimPrefix(a, "--port="), ":")
		}
	}
	if v := os.Getenv("FACET_ADDR"); v != "" {
		return v
	}
	if v := os.Getenv("PORT"); v != "" {
		if _, err := strconv.Atoi(strings.TrimPrefix(v, ":")); err == nil {
			return ":" + strings.TrimPrefix(v, ":")
		}
	}
	return defaultAddr
}

// storeLabel describes where this process's rows will live.
func storeLabel(memory bool) string {
	if memory {
		return "in-memory (VOLATILE — nothing is persisted)"
	}
	return runtime.StoreDescription("")
}

// logLevel reports the verbosity the runtime's structured logger will use, read
// from the same variable it reads (FACET_LOG_LEVEL), defaulting as it does.
func logLevel() string {
	switch strings.ToLower(os.Getenv("FACET_LOG_LEVEL")) {
	case "debug":
		return "debug"
	case "warn", "warning":
		return "warn"
	case "error":
		return "error"
	}
	return "info"
}

// countKind counts the routes of one kind, derived from the IR by the same
// function `facet routes` prints — the banner cannot claim a surface the route
// table does not list.
func countKind(graph *ir.IR, kind string) int {
	n := 0
	for _, r := range buildRoutes(graph, false) {
		if r.Kind == kind {
			n++
		}
	}
	return n
}
