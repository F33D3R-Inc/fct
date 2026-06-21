// facet — the Facet toolchain. One application graph in, every target out.
//
//	facet new <name>              scaffold a new project
//	facet build <file.fct>        compile and print the IR (the application graph)
//	facet run   <file.fct> [addr] compile and serve the web + API projections
//	facet migrate <file.fct>      reconcile the database schema (--plan to dry-run)
//	facet version                 print the toolchain version
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

// version is stamped at release time with -ldflags "-X main.version=…".
var version = "1.19.0"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	// The registry checks a facet's required-toolchain range against this version
	// and stamps it into facet.lock, so it needs to know what `facet` is running.
	registry.ToolchainVersion = version
	// Fold a local .env into the environment (real env wins) so config/secrets can
	// live in a file during development. Harmless when absent.
	runtime.LoadDotEnv(".env")
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("facet %s\n", version)
		return
	case "new":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: facet new <name>")
			os.Exit(2)
		}
		if err := scaffold(os.Args[2]); err != nil {
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
	case "build", "explain", "run", "dev", "console", "seed", "test", "migrate", "backup", "restore", "deploy", "generate":
		// handled below
	default:
		usage()
	}

	if len(os.Args) < 3 {
		usage()
	}
	cmd, file := os.Args[1], os.Args[2]
	// compile.File resolves any `import "..."` modules relative to this file and
	// merges them before placement, so a multi-file app compiles like a single one.
	graph, err := compile.File(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile error: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "build":
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
		fmt.Printf("facet %s: %s running at http://localhost%s\n", version, graph.App, addr)
		fmt.Printf("  web projection  http://localhost%s/\n", addr)
		fmt.Printf("  api projection  http://localhost%s/api\n", addr)
		fmt.Printf("  data store      %s\n", runtime.StoreDescription(graph.App))
		fmt.Printf("  security        %s\n", runtime.SecurityDescription())
		fmt.Printf("  operations      %s\n", runtime.OpsDescription())
		fmt.Printf("  enterprise      %s\n", runtime.EnterpriseDescription())
		if runtime.AdminEnabled() {
			fmt.Printf("  admin console   http://localhost%s/admin\n", addr)
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

// scaffold writes a new starter project into ./<name>.
func scaffold(name string) error {
	if _, err := os.Stat(name); err == nil {
		return fmt.Errorf("%q already exists", name)
	}
	if err := os.MkdirAll(name, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"app.fct":   starterApp,
		"README.md": starterReadme,
		// facet.lock is intentionally NOT ignored — it must be committed so a
		// fresh clone resolves the exact same remote facet bytes.
		".gitignore":         "dist/\nfacet-uploads/\nfacet_modules/\n.env\n",
		"Dockerfile":         dockerfile,
		".dockerignore":      dockerignore,
		"docker-compose.yml": dockerCompose,
		".env.example":       envExample,
		"app.seed.json":      starterSeed,
		"app.test.json":      starterTest,
	}
	for fname, content := range files {
		if err := os.WriteFile(filepath.Join(name, fname), []byte(content), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("created %s/\n\n", name)
	fmt.Println("next:")
	fmt.Printf("  cd %s\n", name)
	fmt.Println("  facet dev app.fct          # run with hot reload (no database needed)")
	fmt.Println("  facet test app.fct         # run the behavior tests")
	fmt.Println("  docker compose up          # or run the whole stack (app + Postgres)")
	return nil
}

// sidecar turns app.fct into app<suffix> (e.g. app.seed.json), the conventional
// default location for an app's seed/test file.
func sidecar(file, suffix string) string {
	return strings.TrimSuffix(file, filepath.Ext(file)) + suffix
}

// scaffoldDeploy writes the container/deploy assets into the current directory
// for a one-command deploy. Existing files are left untouched.
func scaffoldDeploy(app string) error {
	files := map[string]string{
		"Dockerfile":         dockerfile,
		".dockerignore":      dockerignore,
		"docker-compose.yml": dockerCompose,
		".env.example":       envExample,
	}
	wrote := 0
	for fname, content := range files {
		if _, err := os.Stat(fname); err == nil {
			fmt.Printf("  kept     %s (already present)\n", fname)
			continue
		}
		if err := os.WriteFile(fname, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote    %s\n", fname)
		wrote++
	}
	fmt.Printf("\nfacet: deploy assets ready for %s.\n", app)
	fmt.Println("one command (app + Postgres):")
	fmt.Println("  cp .env.example .env   # set FACET_SECRET (facet config --gen-secret)")
	fmt.Println("  docker compose up --build")
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  facet new <name>               scaffold a new project")
	fmt.Fprintln(os.Stderr, "  facet dev <file.fct> [addr]    run with hot reload (in-memory if no DB)")
	fmt.Fprintln(os.Stderr, "  facet run <file.fct> [addr]    serve the web + API projections")
	fmt.Fprintln(os.Stderr, "  facet build <file.fct>         compile and print the IR")
	fmt.Fprintln(os.Stderr, "  facet console <file.fct>       interactive REPL against the app")
	fmt.Fprintln(os.Stderr, "  facet test <file.fct> [tests]  run the app's behavior tests")
	fmt.Fprintln(os.Stderr, "  facet seed <file.fct> [data]   load fixture rows (--dry for in-memory)")
	fmt.Fprintln(os.Stderr, "  facet migrate <file.fct>       reconcile the database schema (--plan to dry-run)")
	fmt.Fprintln(os.Stderr, "  facet backup  <file.fct> [out]   write a logical snapshot (stdout by default)")
	fmt.Fprintln(os.Stderr, "  facet restore <file.fct> [in]    replay a snapshot into the database (stdin by default)")
	fmt.Fprintln(os.Stderr, "  facet deploy <file.fct>        write Dockerfile + compose for a one-command deploy")
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

const starterApp = `# Your Facet app. One declarative graph — the compiler decides what runs on the
# server (the authority) and what runs in the browser. Nothing here says
# "frontend" or "backend".
#
# Run it:   export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/db
#           export FACET_SECRET=$(openssl rand -hex 32)
#           facet run app.fct
# The first account you sign up becomes the admin.

app Starter:
    auth

    entity Post:
        id: int
        author: text
        body: text
        created: int

    state username: text = "" @client
    state password: text = "" @client
    state draft: text = "" @client

    derive postCount: int = count(Post)

    # Row-level authorization: you may delete only your own post. The authority
    # enforces it no matter what the client sends.
    policy mine(id: int):
        actor == Post(id).author

    # now() is the server clock, so the compiler runs post() on the authority.
    action post(body: text):
        add Post { author: actor, body: body, created: now() }

    action remove(id: int):
        requires mine(id)
        remove Post(id)

    view Home at "/":
        box:
            text "{postCount} posts"
            if actor == "guest":
                input bind username placeholder "username"
                input bind password placeholder "password"
                button "sign up" -> signup(username, password)
                button "log in" -> login(username, password)
            if actor != "guest":
                text "signed in as {actor} ({role})"
                button "log out" -> logout
                input bind draft placeholder "what's happening?"
                button "post" -> post(draft)
            for p in Post by created desc limit 50:
                box:
                    text "{p.author}: {p.body}"
                    button "delete" -> remove(p.id)
`

const starterReadme = `# Starter

A Facet application. The whole app is in ` + "`app.fct`" + `.

## Run

Facet stores data in Postgres. Point it at your database, then run:

    export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/yourdb
    facet run app.fct

Open http://localhost:7373. The same graph is also a JSON API at
http://localhost:7373/api. The first account you sign up becomes the admin.

## What's here

- ` + "`auth`" + ` — built-in users, signup/login/logout, ` + "`actor`" + ` and ` + "`role`" + `.
- ` + "`entity`" + ` — durable data (a Postgres table).
- ` + "`action`" + ` — the only thing that changes data; placement is inferred.
- ` + "`view ... at \"/\"`" + ` — a page; add more views at more routes.
- ` + "`for ... where ... by ... limit`" + ` — query/feed.
`

// ── deploy + DX scaffolding ──────────────────────────────────────────────────

const dockerfile = `# Container image for this Facet app. It runs on the published facet toolchain
# image — set it to your organization's image, or build one from the facet repo
# (the repo ships its own Dockerfile that produces this base).
FROM ghcr.io/your-org/facet:latest

# The whole app is one declarative graph.
COPY app.fct /app/app.fct

# Behind TLS, only send the session cookie over HTTPS.
ENV FACET_SECURE_COOKIES=1
EXPOSE 7373

# FACET_DATABASE_URL and FACET_SECRET are supplied by the environment (compose).
ENTRYPOINT ["facet", "run", "/app/app.fct", "0.0.0.0:7373"]
`

const dockerignore = `.git
dist
facet-uploads
*.seed.json
*.test.json
.env
`

const dockerCompose = `# One-command stack: the app plus its Postgres.
#   cp .env.example .env          # then set FACET_SECRET (facet config --gen-secret)
#   docker compose up --build
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: facet
      POSTGRES_PASSWORD: facet
      POSTGRES_DB: facet
    volumes:
      - facet-data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U facet"]
      interval: 3s
      timeout: 3s
      retries: 20

  app:
    build: .
    depends_on:
      db:
        condition: service_healthy
    environment:
      FACET_DATABASE_URL: postgres://facet:facet@db:5432/facet?sslmode=disable
      FACET_SECRET: ${FACET_SECRET:?set FACET_SECRET in .env — run: facet config --gen-secret}
      FACET_SECURE_COOKIES: "0"
    ports:
      - "7373:7373"

volumes:
  facet-data:
`

const envExample = `# Copy to .env (git-ignored). Real environment variables always override these.
# Mint a strong secret:  facet config --gen-secret
FACET_SECRET=

# Local Postgres (matches docker-compose.yml):
FACET_DATABASE_URL=postgres://facet:facet@localhost:5432/facet?sslmode=disable

# Set to 1 behind TLS in production so session cookies are HTTPS-only:
FACET_SECURE_COOKIES=0
`

const starterSeed = `{
  "Post": [
    { "author": "ada",   "body": "first post",   "created": 1718800000 },
    { "author": "grace", "body": "hello, facet", "created": 1718800100 }
  ]
}
`

const starterTest = `{
  "tests": [
    {
      "name": "a user can post",
      "as": { "actor": "ada", "role": "member" },
      "steps": [
        { "run": "post", "args": ["hello world"] },
        { "expect": "count(Post)", "equals": 1 }
      ]
    },
    {
      "name": "you may delete only your own post",
      "steps": [
        { "as": { "actor": "ada" }, "run": "post", "args": ["mine"] },
        { "as": { "actor": "bob" }, "run": "remove", "args": [1], "fails": "forbidden" },
        { "as": { "actor": "ada" }, "run": "remove", "args": [1] },
        { "expect": "count(Post)", "equals": 0 }
      ]
    }
  ]
}
`
