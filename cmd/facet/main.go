// facet — the Facet toolchain. One application graph in, every target out.
//
//	facet new <name>|. [-t app|site|lib]  scaffold a new project (see new.go)
//	facet build <file.fct>        compile and print the IR (the application graph)
//	facet build --release <file>  package the app as one self-contained binary (see release.go)
//	facet run   <file.fct> [addr] compile and serve the web + API projections
//	facet serve <file.fct>        the production server: no watcher, $PORT, preflight (see serve.go)
//	facet routes <file.fct>       every route the app serves (see routes.go)
//	facet doctor [file.fct]       is this project set up correctly? (see doctor.go)
//	facet lang                    the language's node kinds/controls/builtins/modifiers, derived live (see lang.go)
//	facet migrate <file.fct>      reconcile the database schema (--plan to dry-run)
//	facet deploy <file.fct>       container + systemd config (--production, see new.go)
//	facet version                 print the toolchain version
//
// One binary is two programs. A copy of this executable with an application
// bundle appended to it IS that application — it serves the app it carries and
// nothing else (release.go). embeddedApp is therefore the first question main
// asks, before any argument is looked at.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"facet/internal/compile"
	"facet/internal/lsp"
	"facet/internal/parser"
	"facet/internal/registry"
	"facet/runtime"
)

func main() {
	// Fold a local .env into the environment (real env wins) so config/secrets can
	// live in a file during development. Harmless when absent.
	runtime.LoadDotEnv(".env")

	// Is this the toolchain, or an app built with it? A release binary is this
	// same executable with an application bundle appended, and it must behave as
	// that application — `./blog` serves Blog, it does not offer to scaffold a
	// project.
	if app, ok := embeddedApp(); ok {
		os.Exit(runApp(app, os.Args[1:]))
	}

	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("facet %s\n", registry.ToolchainVersion)
		return
	case "new":
		if err := cmdNew(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	case "lsp":
		// The editor language server speaks LSP over stdio.
		if err := lsp.Serve(os.Stdin, os.Stdout); err != nil {
			fatal(err)
		}
		return
	case "config":
		if len(os.Args) > 2 && (os.Args[2] == "--gen-secret" || os.Args[2] == "-gen-secret") {
			fmt.Println(runtime.GenerateSecret())
			return
		}
		fmt.Print(runtime.ResolveConfig().Report())
		return
	case "add":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: facet add <github.com/owner/repo>[@version]")
			os.Exit(2)
		}
		if err := cmdAdd(os.Args[2]); err != nil {
			fatal(err)
		}
		return
	case "get", "install":
		if err := cmdGet(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	case "update":
		if err := cmdUpdate(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	case "why":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: facet why <github.com/owner/repo> [entry.fct]")
			os.Exit(2)
		}
		if err := cmdWhy(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	case "publish":
		if err := cmdPublish(); err != nil {
			fatal(err)
		}
		return
	case "vendor":
		if err := cmdVendor(); err != nil {
			fatal(err)
		}
		return
	case "check":
		os.Exit(cmdCheck(os.Args[2:]))
	case "ir":
		os.Exit(cmdIR(os.Args[2:]))
	case "inspect":
		os.Exit(cmdInspect(os.Args[2:]))
	case "routes":
		os.Exit(cmdRoutes(os.Args[2:]))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	case "lang":
		os.Exit(cmdLang(os.Args[2:]))
	case "serve":
		// serve resolves its own input: source to compile, or a compiled graph to
		// load as-is, so it never requires the compile the generic path below does.
		os.Exit(cmdServe(os.Args[2:]))
	case "build", "explain", "run", "dev", "console", "seed", "test", "migrate", "backup", "restore", "deploy", "generate":
		// handled below
	default:
		usage()
	}

	if len(os.Args) < 3 {
		usage()
	}
	cmd, file := os.Args[1], os.Args[2]
	// A flag may come before the file (`facet build --release app.fct`), so the
	// file is the first argument that is not one.
	if strings.HasPrefix(file, "-") {
		if file = firstOperand(os.Args[2:]); file == "" {
			usage()
		}
	}
	// compile.File resolves any `import "..."` modules relative to this file and
	// merges them before placement, so a multi-file app compiles like a single one.
	graph, err := compile.File(file)
	if err != nil {
		// Editors/automation can ask for the error as structured JSON (source
		// location + message) with --json; humans get the one-line form.
		if hasFlag(os.Args[3:], "--json", "-json") {
			out, _ := json.MarshalIndent(diagnosticsFor(file, err), "", "  ")
			fmt.Fprintln(os.Stderr, string(out))
		} else {
			fmt.Fprintf(os.Stderr, "compile error: %v\n", err)
		}
		os.Exit(1)
	}

	switch cmd {
	case "build":
		// The IR is what `build` has always emitted, and it is genuinely
		// deployable — `facet serve graph.json` needs no compiler. --release goes
		// one step further and packages that same IR into an executable that
		// needs no facet either.
		if hasFlag(os.Args[2:], "--release", "-release") {
			os.Exit(cmdRelease(graph, file, os.Args[2:]))
		}
		out, _ := json.MarshalIndent(graph, "", "  ")
		fmt.Println(string(out))
	case "explain":
		fmt.Printf("Placement for %s — where each piece runs, and why.\n", graph.App)
		fmt.Println("Computed by the compiler from each declaration's shape; never authored.")
		fmt.Println()
		fmt.Println("STATE")
		for _, s := range graph.States {
			where, why := "SERVER", "authoritative — per-session state the authority owns"
			if s.Placement == "client" {
				where, why = "CLIENT", "@client — ephemeral, browser-local; never reaches the authority"
			}
			fmt.Printf("  %-18s %-7s %s\n", s.Name, where, why)
		}
		fmt.Println("\nACTIONS")
		for _, a := range graph.Actions {
			fmt.Printf("  %-18s %-7s %s\n", a.Name, strings.ToUpper(a.Placement), a.Reason)
		}
	case "run":
		addr := ":7373"
		if len(os.Args) > 3 && !strings.HasPrefix(os.Args[3], "-") {
			addr = os.Args[3]
		}
		srv, err := runtime.New(graph)
		if err != nil {
			fatal(err)
		}
		srv.StartJobs()
		url := runtime.BrowseURL(addr)
		fmt.Printf("facet %s: %s running at %s\n", registry.ToolchainVersion, graph.App, url)
		fmt.Printf("  web projection  %s/\n", url)
		fmt.Printf("  api projection  %s/api\n", url)
		fmt.Printf("  data store      %s\n", runtime.StoreDescription(graph.App))
		fmt.Printf("  security        %s\n", runtime.SecurityDescription())
		fmt.Printf("  operations      %s\n", runtime.OpsDescription())
		fmt.Printf("  enterprise      %s\n", runtime.EnterpriseDescription())
		if runtime.AdminEnabled() {
			fmt.Printf("  admin console   %s/admin\n", url)
		}
		// Serve blocks until SIGINT/SIGTERM, then drains in-flight requests, stops
		// the job workers, and closes the database — a deploy-safe shutdown.
		if err := srv.Serve(addr); err != nil {
			fatal(err)
		}
	case "dev":
		addr := ":7373"
		if len(os.Args) > 3 && !strings.HasPrefix(os.Args[3], "-") {
			addr = os.Args[3]
		}
		if err := runtime.RunDev(file, addr); err != nil {
			fatal(err)
		}
	case "console":
		if err := runtime.Console(graph); err != nil {
			fatal(err)
		}
	case "seed":
		seedFile := sidecar(file, ".seed.json")
		dry := false
		for _, a := range os.Args[3:] {
			switch {
			case a == "--dry" || a == "-dry":
				dry = true
			case !strings.HasPrefix(a, "-"):
				seedFile = a
			}
		}
		raw, err := os.ReadFile(seedFile)
		if err != nil {
			fatal(err)
		}
		n, err := runtime.Seed(graph, raw, dry)
		if err != nil {
			fatal(err)
		}
		where := "the database"
		if dry {
			where = "the in-memory store (dry run — nothing persisted)"
		}
		fmt.Printf("facet: seeded %d row(s) into %s\n", n, where)
	case "test":
		testFile := sidecar(file, ".test.json")
		for _, a := range os.Args[3:] {
			if !strings.HasPrefix(a, "-") {
				testFile = a
			}
		}
		raw, err := os.ReadFile(testFile)
		if err != nil {
			fatal(err)
		}
		_, failed, err := runtime.RunTests(graph, raw, os.Stdout)
		if err != nil {
			fatal(err)
		}
		if failed > 0 {
			os.Exit(1)
		}
	case "deploy":
		if hasFlag(os.Args[2:], "--production", "-production", "--prod") {
			if err := scaffoldProduction(graph, file); err != nil {
				fatal(err)
			}
			return
		}
		if err := scaffoldDeploy(graph.App); err != nil {
			fatal(err)
		}
	case "generate":
		dir := "mobile"
		for _, a := range os.Args[3:] {
			if !strings.HasPrefix(a, "-") {
				dir = a
			}
		}
		written, err := runtime.GenerateMobile(graph, dir)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("facet: generated %d mobile client file(s) for %s:\n", len(written), graph.App)
		for _, p := range written {
			fmt.Printf("  %s\n", p)
		}
	case "backup":
		out := os.Stdout
		for _, a := range os.Args[3:] {
			if !strings.HasPrefix(a, "-") {
				f, err := os.Create(a)
				if err != nil {
					fatal(err)
				}
				defer f.Close()
				out = f
			}
		}
		if err := runtime.Backup(graph, out); err != nil {
			fatal(err)
		}
		fmt.Fprintln(os.Stderr, "facet: backup written")
	case "restore":
		in := os.Stdin
		for _, a := range os.Args[3:] {
			if !strings.HasPrefix(a, "-") {
				f, err := os.Open(a)
				if err != nil {
					fatal(err)
				}
				defer f.Close()
				in = f
			}
		}
		n, err := runtime.Restore(graph, in)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("facet: restored %d row(s)\n", n)
	case "migrate":
		apply := true
		for _, a := range os.Args[3:] {
			if a == "--plan" || a == "-plan" {
				apply = false
			}
		}
		plan, err := runtime.Migrate(graph, apply)
		if err != nil {
			fatal(err)
		}
		if len(plan) == 0 {
			fmt.Println("facet: schema is up to date — nothing to migrate")
			return
		}
		if apply {
			fmt.Printf("facet: applied %d schema change(s):\n", len(plan))
		} else {
			fmt.Printf("facet: %d pending schema change(s) (dry run — pass without --plan to apply):\n", len(plan))
		}
		for _, stmt := range plan {
			fmt.Printf("  %s\n", stmt)
		}
	}
}

// sidecar turns app.fct into app<suffix> (e.g. app.seed.json), the conventional
// default location for an app's seed/test file.
func sidecar(file, suffix string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + suffix
}

// ── read-only inspection commands ─────────────────────────────────────────────
//
// check/ir/inspect each compile the app (the only real capability they need) and
// project the resulting IR. They never serve, never touch the database, never
// write files. Each returns a process exit code so main can `os.Exit` it.

// CheckResult is the machine-readable result of `facet check --json`.
type CheckResult struct {
	OK          bool         `json:"ok"`
	File        string       `json:"file"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// cmdCheck compiles the app and reports success or diagnostics, without running
// it — the fast "does this build?" command for CI and editors. With --json it
// emits a stable {ok, file, diagnostics} object; otherwise a one-line summary.
func cmdCheck(args []string) int {
	file := firstNonFlag(args)
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: facet check <file.fct> [--json]")
		return 2
	}
	jsonOut := hasFlag(args, "--json", "-json")
	graph, err := compile.File(file)
	if err != nil {
		if jsonOut {
			out, _ := json.MarshalIndent(CheckResult{OK: false, File: file, Diagnostics: diagnosticsFor(file, err)}, "", "  ")
			fmt.Println(string(out))
		} else {
			fmt.Fprintf(os.Stderr, "facet: %s\n", err)
		}
		return 1
	}
	if jsonOut {
		out, _ := json.MarshalIndent(CheckResult{OK: true, File: file, Diagnostics: []Diagnostic{}}, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("facet: %s — ok (%d entities, %d actions, %d views)\n",
			graph.App, len(graph.Entities), len(graph.Actions), len(graph.Pages))
	}
	return 0
}

// cmdIR dumps the compiled IR — the compiler's terminal artifact — as JSON, so
// editors and tooling can consume the application graph directly. Indented by
// default; --compact emits single-line JSON for piping. On a compile error it
// writes structured diagnostics to stderr so stdout stays clean.
func cmdIR(args []string) int {
	file := firstNonFlag(args)
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: facet ir <file.fct> [--compact]")
		return 2
	}
	graph, err := compile.File(file)
	if err != nil {
		out, _ := json.MarshalIndent(diagnosticsFor(file, err), "", "  ")
		fmt.Fprintln(os.Stderr, string(out))
		return 1
	}
	out, err := marshalIR(graph, hasFlag(args, "--compact", "-compact"))
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(out))
	return 0
}

// cmdInspect prints a structured summary of what the app compiles to — entities,
// state placement, action placement + reasons, views/routes, services, jobs —
// derived entirely from the IR. Human-readable by default; --json for tooling.
func cmdInspect(args []string) int {
	file := firstNonFlag(args)
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: facet inspect <file.fct> [--json]")
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
	report := buildInspect(graph)
	if jsonOut {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		writeInspectText(os.Stdout, report)
	}
	return 0
}

// hasFlag reports whether any of names appears verbatim in args.
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == n {
				return true
			}
		}
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  facet new <name>|. [-t app|site|lib] scaffold a new project (\".\" = here; --list for the templates)")
	fmt.Fprintln(os.Stderr, "  facet dev <file.fct> [addr]    run with hot reload (in-memory if no DB)")
	fmt.Fprintln(os.Stderr, "  facet run <file.fct> [addr]    serve the web + API projections")
	fmt.Fprintln(os.Stderr, "  facet serve [file] [--port N]  production server: no watcher, $PORT, preflight checks")
	fmt.Fprintln(os.Stderr, "  facet check <file.fct> [--json] compile-only; report ok or diagnostics")
	fmt.Fprintln(os.Stderr, "  facet build <file.fct> [--json] compile and print the IR (--json for error diagnostics)")
	fmt.Fprintln(os.Stderr, "  facet build --release <file.fct> package the app as one self-contained binary (-o, --base)")
	fmt.Fprintln(os.Stderr, "  facet ir <file.fct> [--compact] dump the compiled IR as JSON for tooling")
	fmt.Fprintln(os.Stderr, "  facet inspect <file.fct> [--json] structured summary of what the app compiles to")
	fmt.Fprintln(os.Stderr, "  facet routes <file.fct> [--json] every route the app serves (web, API, webhooks)")
	fmt.Fprintln(os.Stderr, "  facet doctor [file.fct] [--production]  the toolchain, the manifest pin, the datastore, the config")
	fmt.Fprintln(os.Stderr, "  facet lang                     the language's node kinds, controls, builtins and modifiers, derived live")
	fmt.Fprintln(os.Stderr, "  facet console <file.fct>       interactive REPL against the app")
	fmt.Fprintln(os.Stderr, "  facet test <file.fct> [tests]  run the app's behavior tests")
	fmt.Fprintln(os.Stderr, "  facet seed <file.fct> [data]   load fixture rows (--dry for in-memory)")
	fmt.Fprintln(os.Stderr, "  facet migrate <file.fct>       reconcile the database schema (--plan to dry-run)")
	fmt.Fprintln(os.Stderr, "  facet backup  <file.fct> [out]   write a logical snapshot (stdout by default)")
	fmt.Fprintln(os.Stderr, "  facet restore <file.fct> [in]    replay a snapshot into the database (stdin by default)")
	fmt.Fprintln(os.Stderr, "  facet deploy <file.fct> [--production]  Dockerfile + compose (--production: release image, facetql, systemd)")
	fmt.Fprintln(os.Stderr, "  facet generate <file.fct> [dir]  emit native mobile clients (Swift/Kotlin/TypeScript)")
	fmt.Fprintln(os.Stderr, "  facet add <github.com/owner/repo>[@version]  pin a remote facet in facet.lock")
	fmt.Fprintln(os.Stderr, "  facet get [file.fct]           fetch locked remote facets into the cache")
	fmt.Fprintln(os.Stderr, "  facet update [<ref>]           re-resolve remote facets to their latest version")
	fmt.Fprintln(os.Stderr, "  facet why <ref> [file.fct]     show how a remote facet enters the build")
	fmt.Fprintln(os.Stderr, "  facet publish                  validate + tag + push a facet release (in a facet repo)")
	fmt.Fprintln(os.Stderr, "  facet vendor                   copy remote facets into ./facet_modules (offline builds)")
	fmt.Fprintln(os.Stderr, "  facet config [--gen-secret]    show resolved config (or mint a FACET_SECRET)")
	fmt.Fprintln(os.Stderr, "  facet lsp                      run the editor language server (stdio)")
	fmt.Fprintln(os.Stderr, "  facet version                  print the toolchain version")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "facet:", err)
	os.Exit(1)
}

// ── registry commands ────────────────────────────────────────────────────────
//
// These manage remote facets — public GitHub repos imported as
// `import "github.com/owner/repo"`. Versions live in facet.lock (one pin per
// repo), never in the source, so a facet used by five files is updated in one
// place. build/run/dev fetch missing-but-locked deps automatically.

// cmdAdd resolves a ref (optionally @version) and writes its facet.lock entry.
// It does not edit .fct files — the user adds the import line themselves.
func cmdAdd(arg string) error {
	ref, form := splitAtVersion(arg)
	if !registry.IsRemote(ref) {
		return fmt.Errorf("`facet add` needs a remote ref like github.com/owner/repo")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	res, err := registry.New(dir)
	if err != nil {
		return err
	}
	if err := res.Add(ref, form); err != nil {
		return err
	}
	if err := res.Save(); err != nil {
		return err
	}
	fmt.Printf("facet: added %s — now add `import %q` to your app and commit facet.lock\n", ref, ref)
	return nil
}

// cmdGet ensures every remote import is fetched and present in the cache. With a
// file it compiles (which resolves + pins anything missing); otherwise it just
// re-hydrates the cache from the committed lock — the fresh-clone path.
func cmdGet(args []string) error {
	if file := firstNonFlag(args); file != "" {
		if _, err := compile.File(file); err != nil {
			return fmt.Errorf("compile error: %w", err)
		}
		fmt.Println("facet: dependencies resolved and cached")
		return nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	res, err := registry.New(dir)
	if err != nil {
		return err
	}
	if err := res.EnsureAll(); err != nil {
		return err
	}
	fmt.Printf("facet: %d module(s) present in the cache\n", len(res.Modules()))
	return nil
}

// cmdUpdate re-resolves dependencies to their latest allowed version and
// rewrites the lock.
func cmdUpdate(args []string) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	res, err := registry.New(dir)
	if err != nil {
		return err
	}
	if ref := firstNonFlag(args); ref != "" {
		if err := res.Update(ref); err != nil {
			return err
		}
	} else if err := res.UpdateAll(); err != nil {
		return err
	}
	return res.Save()
}

// cmdWhy prints the import path(s) by which a remote facet enters the build.
func cmdWhy(args []string) error {
	ref := args[0]
	key, ok := registry.RepoKey(ref)
	if !ok {
		return fmt.Errorf("`facet why` needs a remote ref like github.com/owner/repo")
	}
	entry := ""
	if len(args) > 1 {
		entry = args[1]
	} else {
		entry = soleEntry(".")
	}
	if entry == "" {
		return fmt.Errorf("specify the entry file: facet why %s <app.fct>", ref)
	}
	abs, err := filepath.Abs(entry)
	if err != nil {
		return err
	}
	res, err := registry.New(filepath.Dir(abs))
	if err != nil {
		return err
	}

	var chains [][]string
	visited := map[string]bool{}
	var walk func(absFile string, chain []string)
	walk = func(absFile string, chain []string) {
		src, err := os.ReadFile(absFile)
		if err != nil {
			return
		}
		app, err := parser.Parse(string(src))
		if err != nil {
			return
		}
		dir := filepath.Dir(absFile)
		for _, imp := range app.Imports {
			label := imp
			if k, ok := registry.RepoKey(imp); ok {
				label = k
			}
			next := append(append([]string{}, chain...), label)
			if label == key {
				chains = append(chains, next)
				continue
			}
			p, err := res.Resolve(imp, dir)
			if err != nil || visited[p] {
				continue
			}
			visited[p] = true
			walk(p, next)
		}
	}
	walk(abs, []string{filepath.Base(abs)})

	if len(chains) == 0 {
		fmt.Printf("facet: nothing in %s imports %s\n", filepath.Base(abs), key)
		return nil
	}
	fmt.Printf("facet: %s is imported by:\n", key)
	for _, c := range chains {
		fmt.Printf("  %s\n", strings.Join(c, " → "))
	}
	return nil
}

// cmdPublish validates a facet repo's manifest, proves it compiles, ensures a
// clean tree, then creates and pushes the v<version> tag. It is a thin helper
// around git; the manual equivalent is `git tag vX.Y.Z && git push origin vX.Y.Z`.
func cmdPublish() error {
	b, err := os.ReadFile("facet.json")
	if err != nil {
		return fmt.Errorf("`facet publish` must run in a facet repo (no facet.json here)")
	}
	m, err := registry.ParseManifest(b)
	if err != nil {
		return err
	}
	if m.Name == "" {
		return fmt.Errorf("facet.json needs a `name` (github.com/owner/repo)")
	}
	if m.Version == "" {
		return fmt.Errorf("facet.json needs a `version` (e.g. 1.0.0)")
	}
	if origin, err := gitOutput("remote", "get-url", "origin"); err == nil {
		if k := repoKeyFromGitURL(strings.TrimSpace(origin)); k != "" && k != m.Name {
			return fmt.Errorf("facet.json declares name %q but git origin is %q", m.Name, k)
		}
	}
	entry, err := chooseEntry(".", m)
	if err != nil {
		return err
	}
	if _, err := compile.File(entry); err != nil {
		return fmt.Errorf("facet build failed — fix the facet before publishing: %w", err)
	}
	if out, err := gitOutput("status", "--porcelain"); err != nil {
		return err
	} else if strings.TrimSpace(out) != "" {
		return fmt.Errorf("working tree is not clean — commit your changes before publishing")
	}
	tag := "v" + strings.TrimPrefix(m.Version, "v")
	if _, err := gitOutput("rev-parse", "--verify", "--quiet", "refs/tags/"+tag); err == nil {
		return fmt.Errorf("tag %s already exists — bump the version in facet.json", tag)
	}
	if err := gitRun("tag", tag); err != nil {
		return err
	}
	if err := gitRun("push", "origin", tag); err != nil {
		return err
	}
	fmt.Printf("facet: published %s@%s\n", m.Name, tag)
	return nil
}

// cmdVendor copies every resolved facet into ./facet_modules for fully offline,
// air-gapped builds (the resolver prefers the vendored copy when present).
func cmdVendor() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	res, err := registry.New(dir)
	if err != nil {
		return err
	}
	done, err := res.Vendor()
	if err != nil {
		return err
	}
	if len(done) == 0 {
		fmt.Println("facet: no remote dependencies to vendor")
		return nil
	}
	fmt.Printf("facet: vendored %d module(s) into ./facet_modules:\n", len(done))
	for _, d := range done {
		fmt.Printf("  %s\n", d)
	}
	return nil
}

// splitAtVersion splits "github.com/owner/repo@v1.2.3" into the ref and the
// selection form (empty form means latest).
func splitAtVersion(arg string) (ref, form string) {
	if i := strings.IndexByte(arg, '@'); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}

// firstNonFlag returns the first argument that is not a -flag, or "".
func firstNonFlag(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// valueFlags are the flags that consume the argument after them. A flag's value
// is not an operand, and reading it as one is how `facet serve --port 8080`
// comes to look for an app called "8080".
var valueFlags = map[string]bool{
	"-o": true, "--out": true, "--base": true,
	"-p": true, "--port": true, "-port": true,
	"--addr": true, "-addr": true,
	"-t": true, "--template": true, "-template": true,
}

// firstOperand returns the first argument that is neither a flag nor a flag's
// value — the file a command is about, whichever side of the flags it is written
// on. `--flag=value` carries its value inside itself and consumes nothing.
func firstOperand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if valueFlags[a] {
			i++ // skip the value this flag takes
		}
	}
	return ""
}

// soleEntry guesses an app's entry file in dir: app.fct if present, else the
// only .fct file, else "".
func soleEntry(dir string) string {
	if fi, err := os.Stat(filepath.Join(dir, "app.fct")); err == nil && !fi.IsDir() {
		return filepath.Join(dir, "app.fct")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var fcts []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
			fcts = append(fcts, filepath.Join(dir, e.Name()))
		}
	}
	if len(fcts) == 1 {
		return fcts[0]
	}
	return ""
}

// chooseEntry picks a facet repo's entry file: the manifest main, else main.fct,
// else the sole .fct in the directory.
func chooseEntry(dir string, m *registry.Manifest) (string, error) {
	if m.Main != "" {
		return filepath.Join(dir, m.Main), nil
	}
	if fi, err := os.Stat(filepath.Join(dir, "main.fct")); err == nil && !fi.IsDir() {
		return filepath.Join(dir, "main.fct"), nil
	}
	if e := soleEntry(dir); e != "" {
		return e, nil
	}
	return "", fmt.Errorf("set `main` in facet.json — the repo root has no single .fct entry file")
}

// repoKeyFromGitURL normalizes a git remote URL into a github.com/owner/repo
// key, supporting both SSH (git@github.com:owner/repo.git) and HTTPS forms.
func repoKeyFromGitURL(url string) string {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	switch {
	case strings.HasPrefix(url, "git@github.com:"):
		return "github.com/" + strings.TrimPrefix(url, "git@github.com:")
	case strings.HasPrefix(url, "https://github.com/"):
		return "github.com/" + strings.TrimPrefix(url, "https://github.com/")
	case strings.HasPrefix(url, "ssh://git@github.com/"):
		return "github.com/" + strings.TrimPrefix(url, "ssh://git@github.com/")
	}
	return ""
}

func gitOutput(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return string(out), err
}

func gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
