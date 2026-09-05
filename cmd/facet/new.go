package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"facet/internal/compile"
	"facet/internal/ir"
	"facet/internal/registry"
)

// `facet new` — the front door of the framework.
//
//	facet new <name>                    the default template (app)
//	facet new <name> --template site    a multi-page site
//	facet new <name> --template lib     an importable facet library
//	facet new --list                    what the templates are
//
// The contract this file exists to keep is narrow and absolute: what it writes
// COMPILES. Every template is verified by compiling the project it just wrote,
// with the same compiler `facet build` uses, before the command reports success
// — a scaffolder that emits source the toolchain rejects is worse than none, and
// the only way to be sure is to run the compiler rather than to trust the
// literals below.
//
// The generated `facet.json` is marshalled from registry.Manifest and pinned to
// registry.ToolchainVersion, so the manifest a new project carries can never
// disagree with the toolchain that wrote it, or with the struct the resolver
// parses it back into. Nothing here restates a version.

// project is everything a template needs to know about what is being created:
// the directory it lands in, and the identifier every `app` block declares.
type project struct {
	Dir  string // the path passed on the command line, e.g. "blog" or "../blog"
	Name string // the directory's base name, e.g. "blog" — the manifest name
	App  string // the declared app identifier, e.g. "Blog" — an upper ident
	// FacetsImport is "" unless --facets asked for the standard library, in
	// which case it is the relative import prefix pointing at a local sibling
	// `facets/` checkout (e.g. "../../facets") found by findSiblingFacets.
	// --facets refuses outright rather than ever setting this to the published
	// registry path: that registry has only v0.1.0, which predates most of the
	// library (see cmdNew).
	FacetsImport string
}

// template is one starting point: what it is for, the entry file `facet dev`
// takes, the files it writes, and what to tell the user to run next.
type template struct {
	Name    string
	Summary string
	Entry   string
	// Files renders the whole project. Every value is file content; keys are
	// slash-separated paths relative to the project directory.
	Files func(p project) map[string]string
	// Next is the "you are here, now do this" list printed after scaffolding.
	Next func(p project) []string
}

// templates is the registry of starting points, in the order `--list` prints
// them. They are deliberately few and genuinely different in shape: an app is
// data + actions + auth, a site is routes + layout + type, a library is
// components + a manifest and no app at all.
var templates = []template{
	{
		Name:    "app",
		Summary: "entities, actions, auth and policies — a list/detail flow with drafts (the default)",
		Entry:   "app.fct",
		Files:   appFiles,
		Next: func(p project) []string {
			return []string{
				"facet dev app.fct          # run with hot reload (no database needed)",
				"facet seed app.fct --dry   # load the fixture rows in memory",
				"facet test app.fct         # run the behavior tests",
				"facet routes app.fct       # every route this app serves",
			}
		},
	},
	{
		Name:    "site",
		Summary: "a multi-page site: one layout with a slot, a self-marking nav, light + dark themes",
		Entry:   "site.fct",
		Files:   siteFiles,
		Next: func(p project) []string {
			return []string{
				"facet dev site.fct         # serve the site with hot reload",
				"facet routes site.fct      # every route this site serves",
				"# add a page: write pages/pricing.fct, then import it from site.fct",
			}
		},
	},
	{
		Name:    "lib",
		Summary: "an importable facet library: components + a publishable manifest, no app",
		Entry:   "main.fct",
		Files:   libFiles,
		Next: func(p project) []string {
			return []string{
				"facet dev example.fct      # the gallery that exercises every component",
				"facet check main.fct       # what an importer compiles",
				"# set `name` in facet.json to your github.com/owner/repo, then: facet publish",
			}
		},
	},
}

// cmdNew parses `facet new`'s arguments and scaffolds the chosen template.
func cmdNew(args []string) error {
	tmplName := "app"
	name := ""
	withFacets := false
	force := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--list" || a == "-list":
			listTemplates()
			return nil
		case a == "--facets" || a == "-facets":
			withFacets = true
		case a == "--force" || a == "-force":
			force = true
		case a == "--template" || a == "-template" || a == "-t":
			if i+1 >= len(args) {
				return fmt.Errorf("--template needs a value (%s)", templateNames())
			}
			i++
			tmplName = args[i]
		case strings.HasPrefix(a, "--template="):
			tmplName = strings.TrimPrefix(a, "--template=")
		case strings.HasPrefix(a, "-t="):
			tmplName = strings.TrimPrefix(a, "-t=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q — usage: facet new <name>|. [--template %s] [--facets] [--force]", a, strings.Join(templateNames(), "|"))
		case name == "":
			name = a
		default:
			return fmt.Errorf("`facet new` takes one name (got %q and %q)", name, a)
		}
	}
	if name == "" {
		return fmt.Errorf("usage: facet new <name>|.  [--template %s] [--facets] [--force]  (facet new --list)\n  \".\" scaffolds into the current directory instead of creating a new one", strings.Join(templateNames(), "|"))
	}
	tmpl, ok := findTemplate(tmplName)
	if !ok {
		return fmt.Errorf("unknown template %q — pick one of: %s", tmplName, strings.Join(templateNames(), ", "))
	}
	if withFacets && tmpl.Name != "app" {
		return fmt.Errorf("--facets applies to the `app` template; the published `facets` library has no components the %s template composes yet", tmpl.Name)
	}

	facetsImport := ""
	if withFacets {
		dir, version, ok := findSiblingFacets(name)
		if !ok {
			return fmt.Errorf(`--facets: refusing to scaffold a broken import.

The published registry carries only %s@v0.1.0, whose main has no layout/,
navigation/, marketing/, auth/ or data/ — nearly every component the library
has today. Pinning it would compile, then fail later with confusing import
errors the first time a template composes any of them.

No local facets/ checkout was found near %q either (this looks for a
"facets" directory, walking upward from it, whose facet.json declares %s).

Fix one of the two: place (or clone) the facets repo next to this project so
--facets can import it directly by relative path, or drop --facets and start
with plain views — nothing here stops you from adding facets by hand later.`,
				facetsRegistryPath, name, facetsRegistryPath)
		}
		facetsImport = dir
		fmt.Printf("--facets: using the local checkout at %s (v%s) — relative import, not the published registry (which has only v0.1.0)\n", dir, version)
	}
	return scaffold(tmpl, name, facetsImport, force)
}

// listTemplates prints the starting points and what each one is for.
func listTemplates() {
	fmt.Println("facet new templates:")
	for _, t := range templates {
		fmt.Printf("  %-6s %s\n", t.Name, t.Summary)
	}
	fmt.Println("\n  facet new <name>                  the app template")
	fmt.Println("  facet new .                        scaffold into the current directory")
	fmt.Println("  facet new <name> --template site  a different one")
	fmt.Println("  facet new <name> --facets         compose a local `facets/` checkout found near <name> (app template)")
	fmt.Println("  facet new <name> --force          write even if files with those names already exist")
}

// facetsRegistryPath is the module path the `facets` standard library
// publishes under. Its registry history has exactly one tag, v0.1.0 — the
// original core batch — which predates layout/, navigation/, marketing/,
// auth/ and data/. Pinning it is not a usable starting point for a project
// written today, so --facets never scaffolds an import of it; see
// findSiblingFacets and cmdNew.
const facetsRegistryPath = "github.com/F33D3R-Inc/facets"

// findSiblingFacets looks for a local, unpublished checkout of the `facets`
// library near where a new project is being scaffolded — the arrangement every
// app actually built on this framework so far has ended up in by hand, because
// the registry copy is too far behind to use (see facetsRegistryPath). It walks
// upward from the project's parent directory (the project itself may not exist
// yet, and its own directory is never itself named "facets" in practice) up to
// the filesystem root, looking for a directory named "facets" whose facet.json
// declares this exact module — so an unrelated folder that happens to be named
// "facets" is never mistaken for the library. On a hit it returns the forward-
// slashed relative import path from the project to that checkout (e.g.
// "../../facets", matching the by-hand convention `website/` already uses) and
// the version its manifest declares.
func findSiblingFacets(projectDir string) (relImport, version string, ok bool) {
	projAbs, err := filepath.Abs(projectDir)
	if err != nil {
		return "", "", false
	}
	search := filepath.Dir(projAbs)
	for i := 0; i < 12; i++ {
		candidate := filepath.Join(search, "facets")
		if b, err := os.ReadFile(filepath.Join(candidate, "facet.json")); err == nil {
			if m, err := registry.ParseManifest(b); err == nil && m.Name == facetsRegistryPath {
				rel, err := filepath.Rel(projAbs, candidate)
				if err != nil {
					return "", "", false
				}
				return filepath.ToSlash(rel), m.Version, true
			}
		}
		parent := filepath.Dir(search)
		if parent == search {
			break
		}
		search = parent
	}
	return "", "", false
}

func templateNames() []string {
	names := make([]string, len(templates))
	for i, t := range templates {
		names[i] = t.Name
	}
	return names
}

func findTemplate(name string) (template, bool) {
	for _, t := range templates {
		if t.Name == name {
			return t, true
		}
	}
	return template{}, false
}

// scaffold writes a template into dir — which may be "." to target the current
// directory instead of creating a new one — and then proves it: the project it
// just wrote is handed to the compiler, and `facet new` only reports success if
// the compiler accepts it. When --facets found a local checkout, that same
// compile is what resolves the relative import and proves it actually works.
//
// Nothing is refused merely because dir already exists — "." always exists,
// and scaffolding into an empty or unrelated directory is legitimate — only
// because a file this template would write is already there. force overwrites
// those; without it, every conflict is named before anything is touched.
func scaffold(tmpl template, dir, facetsImport string, force bool) error {
	base := filepath.Base(filepath.Clean(dir))
	if dir == "." {
		if wd, err := os.Getwd(); err == nil {
			base = filepath.Base(wd)
		}
	}
	p := project{Dir: dir, Name: base, App: appIdent(base), FacetsImport: facetsImport}

	files := tmpl.Files(p)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	if !force {
		var conflicts []string
		for _, name := range names {
			target := filepath.Join(dir, filepath.FromSlash(name))
			if _, err := os.Stat(target); err == nil {
				conflicts = append(conflicts, name)
			}
		}
		if len(conflicts) > 0 {
			where := dir
			if dir == "." {
				where = "the current directory"
			}
			return fmt.Errorf("%s already has %s — pass --force to overwrite", where, strings.Join(conflicts, ", "))
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(files[name]), 0o644); err != nil {
			return err
		}
	}

	entry := filepath.Join(dir, tmpl.Entry)
	graph, err := compile.File(entry)
	if err != nil {
		// The project is left on disk: the error is the interesting artifact, and
		// deleting the evidence helps nobody.
		return fmt.Errorf("scaffolded %s but it does not compile — this is a toolchain bug, please report it:\n  %v", dir, err)
	}

	where := dir + "/"
	if dir == "." {
		where = "the current directory"
	}
	fmt.Printf("created %s from the %s template — %d files\n", where, tmpl.Name, len(names))
	for _, name := range names {
		fmt.Printf("  %s\n", name)
	}
	fmt.Printf("\nverified: %s compiles (%d entities, %d actions, %d views) on facet %s\n",
		tmpl.Entry, len(graph.Entities), len(graph.Actions), len(graph.Pages), registry.ToolchainVersion)
	fmt.Println("\nnext:")
	if dir != "." {
		fmt.Printf("  cd %s\n", dir)
	}
	for _, line := range tmpl.Next(p) {
		fmt.Printf("  %s\n", line)
	}
	return nil
}

// appIdent turns a directory name into the upper identifier an `app` block
// declares: "my-blog" and "my_blog 2" both become "MyBlog2". A name that cannot
// start an identifier is prefixed rather than rejected, so `facet new 2048`
// works.
func appIdent(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if upper {
				b.WriteRune(unicode.ToUpper(r))
				upper = false
			} else {
				b.WriteRune(r)
			}
		default:
			upper = true
		}
	}
	out := b.String()
	if out == "" {
		return "App"
	}
	if !unicode.IsLetter(rune(out[0])) {
		return "App" + out
	}
	return out
}

// manifest renders the project's facet.json. The toolchain pin is read from
// registry.ToolchainVersion — the one definition of "what version is this" —
// and the document is marshalled from registry.Manifest, the very struct the
// resolver parses it back into, so a field that is renamed there cannot leave
// this scaffolder writing a manifest nothing reads.
func manifest(name, main, description string) string {
	m := registry.Manifest{
		Name:        name,
		Version:     "0.1.0",
		Main:        main,
		Description: description,
		Facet:       ">=" + registry.ToolchainVersion,
		License:     "MIT",
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil { // unreachable: Manifest is plain strings
		return ""
	}
	return string(b) + "\n"
}

// gitignore is what a Facet project must not commit: build output, local
// binaries, the data and uploads the runtime writes beside the app, the
// vendored module tree, and the environment file holding FACET_SECRET.
//
// facet.lock is deliberately absent — it is the pin that makes a fresh clone
// resolve the identical remote facet bytes, so it is committed, not ignored.
const gitignore = `# Build output and local toolchain binaries
dist/
.bin/

# Written by the runtime beside the app
facet-uploads/
*.db
*.sqlite

# Vendored remote facets (facet.lock IS committed — this cache is not)
facet_modules/

# Local configuration and secrets — never commit these
.env
.env.local
`

// ── the app template ─────────────────────────────────────────────────────────

func appFiles(p project) map[string]string {
	return map[string]string{
		"app.fct":            appFct(p),
		"facet.json":         manifest(p.Name, "app.fct", "A Facet application."),
		".gitignore":         gitignore,
		".env.example":       envExample,
		"app.seed.json":      appSeed,
		"app.test.json":      appTest,
		"Dockerfile":         dockerfile,
		".dockerignore":      dockerignore,
		"docker-compose.yml": dockerCompose,
	}
}

func appFct(p project) string {
	// --facets composes one component out of the published standard library, so
	// the project starts with a real remote import, a real facet.lock pin, and a
	// worked example of `use`. Both forms of the feed row are written out in
	// full rather than assembled from indent fragments: this is significant
	// whitespace, and the readable version is the one you can count.
	imports, entryRow := "", `                box:
                    link "{e.title}" -> "/entry/{e.id}"
                    text "by {e.author}" class "x-meta"
`
	if p.FacetsImport != "" {
		imports = "# The `facets` standard library — a local sibling checkout, imported by\n" +
			"# relative path (not the published registry: see `facet new --list`).\n" +
			"import \"" + p.FacetsImport + "/ui/avatar.fct\"\n\n"
		entryRow = `                row:
                    use Avatar(e.author)
                    box:
                        link "{e.title}" -> "/entry/{e.id}"
                        text "by {e.author}" class "x-meta"
`
	}
	return imports + `# ` + p.App + ` — one declarative graph. The compiler decides what runs on the
# authority (the server) and what runs in the browser; nothing below says
# "frontend" or "backend", and nothing below writes a fetch, a route table or a
# SQL statement.
#
#   facet dev app.fct       run with hot reload (in-memory store, no database)
#   facet routes app.fct    every route this app serves
#   facet inspect app.fct   what it compiles to, and where each piece runs
#
# For a real database, point at Postgres or FacetQL and use ` + "`facet run`" + `:
#   export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/db
#   export FACET_SECRET=$(facet config --gen-secret)
app ` + p.App + `:
    # Built-in identity: signup/login/logout, the ` + "`actor`" + ` and ` + "`role`" + ` references,
    # and a managed user table. The first account created becomes the admin.
    auth

    theme:
        accent "#2f4bf0"
        radius "10px"
        maxwidth "760px"

    theme dark:
        accent "#7c93ff"

    # The runtime ships a legible default stylesheet; this is only the delta
    # this app asks for. Every selector is anchored on [data-fa-mount] — the app
    # root — so it outweighs the default sheet's two-class rules.
    css:
        [data-fa-mount] .x-bar { display: flex; align-items: center; gap: 14px; flex-wrap: wrap;
                   padding-bottom: 14px; margin-bottom: 20px;
                   border-bottom: 1px solid var(--fa-border); }
        [data-fa-mount] .x-bar .fa-link { text-decoration: none; color: var(--fa-muted); }
        [data-fa-mount] .x-bar .fa-link:hover { color: var(--fa-fg); }
        [data-fa-mount] .x-who { margin-left: auto; color: var(--fa-muted); font-size: 14px; }
        [data-fa-mount] .x-meta { color: var(--fa-muted); font-size: 13px; }
        [data-fa-mount] .x-empty { color: var(--fa-muted); padding: 28px 0; text-align: center; }

    # ── data ────────────────────────────────────────────────────────────────
    # An entity is durable, shared, authoritative data: a real table with typed,
    # indexed columns. The compiler builds the index for any field it sees
    # filtered, ordered or related, so nothing here declares one.
    entity Entry:
        id: int
        author: text
        title: text @required
        body: text
        published: bool
        created: int

    # ── state ───────────────────────────────────────────────────────────────
    # @client state is ephemeral and browser-local; the authority never sees it.
    # These are the form cells, named apart from the action parameters they feed
    # so the two can never be confused for one another.
    state draftTitle: text = "" @client
    state draftBody: text = "" @client
    state username: text = "" @client
    state password: text = "" @client

    # A derive is a computed read. It re-evaluates when what it reads changes.
    derive published: int = count(e in Entry where e.published)

    # ── authorization ───────────────────────────────────────────────────────
    # A policy is enforced by the authority no matter what the client sends.
    # ` + "`mine`" + ` is row-level: the argument is the row being acted on.
    policy member:
        actor != "guest"

    policy mine(id: int):
        Entry(id).author == actor

    # ── actions ─────────────────────────────────────────────────────────────
    # An action is the only thing that changes data. Placement is inferred:
    # these write durable rows, so the compiler runs them on the authority.
    action write(title: text, body: text):
        requires member
        check title != "" "a title is required"
        add Entry { author: actor, title: title, body: body, published: false, created: now() }

    action publish(id: int):
        requires mine(id)
        set Entry(id).published = true

    action unpublish(id: int):
        requires mine(id)
        set Entry(id).published = false

    action discard(id: int):
        requires mine(id)
        remove Entry(id)

    # ── the shared chrome ───────────────────────────────────────────────────
    # One layout, one ` + "`slot`" + `. Every view renders inside it, so the bar is
    # written once and every page has it.
    layout Shell:
        box:
            row class "x-bar":
                link "` + p.App + `" -> "/"
                link "Drafts" -> "/drafts"
                box class "x-who":
                    if actor == "guest":
                        text "not signed in"
                    if actor != "guest":
                        text "{actor} · {role}"
            slot

    # ── routes ──────────────────────────────────────────────────────────────
    view Home at "/" in Shell:
        meta title "` + p.App + `"
        meta description "{published} published entries."
        box:
            if actor == "guest":
                box:
                    text "Sign in to write. The first account created is the admin."
                    input bind username placeholder "username"
                    input bind password placeholder "password"
                    row:
                        button "sign up" -> signup(username, password)
                        button "log in" -> login(username, password)
            if actor != "guest":
                box:
                    input bind draftTitle placeholder "title"
                    input bind draftBody placeholder "say something"
                    row:
                        button "save draft" -> write(draftTitle, draftBody)
                        button "log out" -> logout
            text "{published} published" class "x-meta"
            for e in Entry where e.published by created desc limit 50:
` + entryRow + `            if !exists(e in Entry where e.published):
                text "Nothing published yet." class "x-empty"

    # ` + "`requires`" + ` on a view guards the ROUTE: the authority refuses to render
    # it, and the client hides every link that leads here.
    view Drafts at "/drafts" in Shell requires member:
        meta title "Drafts — ` + p.App + `"
        box:
            text "Your drafts" class "x-meta"
            for e in Entry where e.author == actor && !e.published by created desc limit 50:
                row:
                    link "{e.title}" -> "/entry/{e.id}"
                    button "publish" -> publish(e.id)
                    button "discard" -> discard(e.id)
            if !exists(e in Entry where e.author == actor && !e.published):
                text "No drafts yet — write one on the home page." class "x-empty"

    # A ` + "`:param`" + ` segment is bound by name for the whole view, including its
    # metadata. The row is matched with ` + "`for`" + ` rather than read with Entry(id) so
    # the actions receive a real int id, not the text the URL carried.
    view EntryPage at "/entry/:id" in Shell:
        meta title "{Entry(id).title} — ` + p.App + `"
        meta description "{Entry(id).body}"
        box:
            for e in Entry where e.id == id by id desc limit 1:
                box:
                    text "{e.title}"
                    text "by {e.author}" class "x-meta"
                    text "{e.body}"
                    if e.author == actor:
                        row:
                            if !e.published:
                                button "publish" -> publish(e.id)
                            if e.published:
                                button "unpublish" -> unpublish(e.id)
                            button "discard" -> discard(e.id)
            if !exists(e in Entry where e.id == id):
                text "No such entry." class "x-empty"
`
}

const appSeed = `{
  "Entry": [
    { "author": "ada",   "title": "Hello, Facet",   "body": "One graph, every projection.", "published": true,  "created": 1718800000 },
    { "author": "grace", "title": "A second entry", "body": "Written on the authority.",    "published": true,  "created": 1718800100 },
    { "author": "ada",   "title": "Still a draft",  "body": "Not published yet.",           "published": false, "created": 1718800200 }
  ]
}
`

const appTest = `{
  "tests": [
    {
      "name": "a member can write a draft",
      "as": { "actor": "ada", "role": "member" },
      "steps": [
        { "run": "write", "args": ["First", "hello"] },
        { "expect": "count(Entry)", "equals": 1 }
      ]
    },
    {
      "name": "a title is required",
      "as": { "actor": "ada", "role": "member" },
      "steps": [
        { "run": "write", "args": ["", "no title"], "fails": "a title is required" },
        { "expect": "count(Entry)", "equals": 0 }
      ]
    },
    {
      "name": "you may publish only your own entry",
      "steps": [
        { "as": { "actor": "ada", "role": "member" },   "run": "write",   "args": ["Mine", "body"] },
        { "as": { "actor": "bob", "role": "member" },   "run": "publish", "args": [1], "fails": "forbidden" },
        { "as": { "actor": "ada", "role": "member" },   "run": "publish", "args": [1] },
        { "expect": "count(e in Entry where e.published)", "equals": 1 }
      ]
    }
  ]
}
`

// ── the site template ────────────────────────────────────────────────────────

func siteFiles(p project) map[string]string {
	return map[string]string{
		"site.fct":       siteFct(p),
		"chrome.fct":     chromeFct(p),
		"pages/home.fct": sitePageHome(p),
		"pages/docs.fct": sitePageDocs(p),
		"facet.json":     manifest(p.Name, "site.fct", "A Facet site."),
		".gitignore":     gitignore,
		".env.example":   envExample,
	}
}

func siteFct(p project) string {
	return `# ` + p.Name + ` — a multi-page site.
#
# This file is the substrate and nothing else: the two palettes, the type scale,
# the band rhythm, and the one ` + "`layout`" + ` every page renders inside. Every page
# lives in pages/, every reusable piece in chrome.fct.
#
#   facet dev site.fct      serve on http://localhost:7373 with hot reload
#   facet routes site.fct   every route this site serves
#
# To add a page: write pages/<name>.fct, declare
# ` + "`view Name at \"/path\" in Site:`" + ` in it, and add one import line here.
import "chrome.fct"
import "pages/home.fct"
import "pages/docs.fct"

app ` + p.App + `:
    # ── palette ─────────────────────────────────────────────────────────────
    # Light is the base; ` + "`theme dark:`" + ` lowers to
    # @media(prefers-color-scheme: dark), so the whole site restyles from these
    # lines and nothing below has to know which mode it is in.
    theme:
        bg "#ffffff"
        fg "#0b0e14"
        muted "#5b6674"
        accent "#2f4bf0"
        border "#e4e7ec"
        card-border "#e4e7ec"
        radius "10px"
        maxwidth "1080px"
        font "16px/1.6 ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif"

    theme dark:
        bg "#07080b"
        fg "#e9edf4"
        muted "#8b95a5"
        accent "#7c93ff"
        border "#1d222c"
        card-border "#1d222c"

    css:
        # The runtime's default sheet is built for an APP: it centres a narrow
        # column and makes every nested box a bordered card. A site is neither,
        # so both are turned off here and re-asked-for by the one wrapper that
        # means it (.x-card).
        body { margin: 0; max-width: none; padding: 0; background: var(--fa-bg);
               -webkit-font-smoothing: antialiased; }
        [data-fa-mount] .fa-box { border: none; border-radius: 0; padding: 0; gap: 0; background: none; }
        [data-fa-mount] .fa-row { gap: 0; }

        # ── measure and rhythm ──────────────────────────────────────────────
        # A page is a stack of full-bleed bands; the measure comes from .x-page,
        # which every band wraps around its own content.
        [data-fa-mount] .x-page { max-width: var(--fa-maxwidth); margin: 0 auto; padding: 0 24px; }
        [data-fa-mount] .x-band { padding: 72px 0; border-top: 1px solid var(--fa-border); }
        [data-fa-mount] .x-band-first { padding: 84px 0 72px; border-top: none; }
        @media (max-width: 720px) {
            [data-fa-mount] .x-band { padding: 48px 0; }
            [data-fa-mount] .x-band-first { padding: 44px 0 48px; }
        }

        # ── the bar ─────────────────────────────────────────────────────────
        [data-fa-mount] .x-nav { border-bottom: 1px solid var(--fa-border); position: sticky; top: 0;
                   background: var(--fa-bg); z-index: 10; }
        [data-fa-mount] .x-nav-inner { display: flex; align-items: center; gap: 20px; padding: 14px 24px;
                         flex-wrap: wrap; max-width: var(--fa-maxwidth); margin: 0 auto; }
        [data-fa-mount] .x-brand a.fa-link { font-weight: 700; letter-spacing: -.02em;
                        text-decoration: none; color: var(--fa-fg); }
        [data-fa-mount] .x-nav-links { display: flex; gap: 6px; margin-left: auto; flex-wrap: wrap; }
        [data-fa-mount] .x-navitem a.fa-link { display: block; padding: 6px 10px; border-radius: 7px;
                          text-decoration: none; color: var(--fa-muted); }
        [data-fa-mount] .x-navitem a.fa-link:hover { color: var(--fa-fg); }
        [data-fa-mount] .x-navitem-on a.fa-link { color: var(--fa-fg); background: var(--fa-border); }

        # ── type scale ──────────────────────────────────────────────────────
        # A ` + "`text`" + ` node is a span, so anything that behaves like a block says so.
        # Four sizes, no exceptions.
        [data-fa-mount] .x-display, [data-fa-mount] .x-h2,
        [data-fa-mount] .x-lede, [data-fa-mount] .x-small { display: block; }
        [data-fa-mount] .x-display { font-size: clamp(34px, 5vw, 56px); line-height: 1.04;
                       letter-spacing: -.035em; font-weight: 680; max-width: 22ch;
                       text-wrap: balance; }
        [data-fa-mount] .x-h2 { font-size: 26px; line-height: 1.15; letter-spacing: -.02em;
                  font-weight: 640; }
        [data-fa-mount] .x-lede { font-size: 19px; line-height: 1.5; color: var(--fa-muted);
                    max-width: 60ch; }
        [data-fa-mount] .x-small { font-size: 14px; line-height: 1.55; color: var(--fa-muted);
                     max-width: 66ch; }

        # ── pieces ──────────────────────────────────────────────────────────
        [data-fa-mount] .x-stack { display: flex; flex-direction: column; gap: 18px; }
        [data-fa-mount] .x-grid { display: grid; gap: 22px; margin-top: 28px;
                    grid-template-columns: repeat(auto-fit, minmax(min(100%, 240px), 1fr)); }
        [data-fa-mount] .x-card { border: 1px solid var(--fa-card-border); border-radius: var(--fa-radius);
                    padding: 20px; display: flex; flex-direction: column; gap: 8px; }
        [data-fa-mount] .x-cta a.fa-link { display: inline-block; background: var(--fa-accent);
                   color: #fff; padding: 11px 20px; border-radius: 8px;
                   text-decoration: none; font-weight: 600; }
        [data-fa-mount] .x-foot { border-top: 1px solid var(--fa-border); padding: 36px 0 48px; }

    # ── the one layout ──────────────────────────────────────────────────────
    # Everything on every page renders inside .x-site, which is what makes the
    # overrides above reachable. The bar and the footer are here because they are
    # the same on every page.
    layout Site:
        box class "x-site":
            use SiteNav()
            slot
            use SiteFooter()
`
}

func chromeFct(p project) string {
	return `# Site chrome — the pieces every page reuses. Nothing here is a new primitive;
# each is a wrapper around the substrate site.fct already declared.
app ` + p.App + `:
    # ── bands ───────────────────────────────────────────────────────────────
    # A full-bleed horizontal band with the page measure inside it. Two names
    # rather than a variant parameter: a component has at most one ` + "`slot`" + `, so a
    # match with an arm per variant would need one slot per arm. The size is in
    # the name.
    component Band():
        box class "x-band":
            box class "x-page":
                slot
    component BandFirst():
        box class "x-band x-band-first":
            box class "x-page":
                slot

    # ── the nav ─────────────────────────────────────────────────────────────
    # ` + "`route`" + ` is the path being rendered. A nav item compares it to its own
    # destination and marks itself, so the bar lives in the shared layout and no
    # page is ever told which page it is.
    component NavLink(label: text, path: text):
        if route == path:
            box class "x-navitem x-navitem-on":
                link "{label}" -> "{path}"
        if route != path:
            box class "x-navitem":
                link "{label}" -> "{path}"

    component SiteNav():
        box class "x-nav":
            row class "x-nav-inner":
                box class "x-brand":
                    link "` + p.App + `" -> "/"
                row class "x-nav-links":
                    use NavLink("Home", "/")
                    use NavLink("Docs", "/docs")

    component SiteFooter():
        box class "x-foot":
            box class "x-page":
                text "© ` + p.App + `. Built with the Facet stack." class "x-small"

    # ── content pieces ──────────────────────────────────────────────────────
    component Card(title: text, body: text):
        box class "x-card":
            text "{title}" class "x-h2"
            text "{body}" class "x-small"

    component Section(title: text):
        box class "x-stack":
            text "{title}" class "x-h2"
            slot
`
}

func sitePageHome(p project) string {
	return `# The home page. One view, one route, rendered inside the ` + "`Site`" + ` layout.
import "../chrome.fct"

app ` + p.App + `:
    view Home at "/" in Site:
        meta title "` + p.App + `"
        meta description "A site built with the Facet stack: one declarative graph, every projection."
        use BandFirst():
            box class "x-stack":
                text "One graph. Every projection." class "x-display"
                text "This page, its navigation, its metadata and its dark mode are one declarative source — no template engine, no router, no build step." class "x-lede"
                box class "x-cta":
                    link "Read the docs" -> "/docs"
        use Band():
            use Section("What is already here"):
                box class "x-grid":
                    use Card("One layout", "site.fct declares a single layout with a slot; every page renders inside it.")
                    use Card("A self-marking nav", "NavLink compares route to its own destination, so no page is told which page it is.")
                    use Card("Two palettes", "theme and theme dark are declared once and the whole site follows the reader's system setting.")
`
}

func sitePageDocs(p project) string {
	return `# A second page. Adding a third is: this file, one import line in site.fct.
import "../chrome.fct"

app ` + p.App + `:
    view Docs at "/docs" in Site:
        meta title "Docs — ` + p.App + `"
        meta description "How this site is put together, and how to add to it."
        use BandFirst():
            box class "x-stack":
                text "Docs" class "x-display"
                text "How this site is put together." class "x-lede"
        use Band():
            use Section("The shape"):
                box class "x-grid":
                    use Card("site.fct", "The substrate: palettes, type scale, band rhythm, and the one layout. It imports every page.")
                    use Card("chrome.fct", "The reusable pieces — bands, the nav, the footer, cards. Components, not pages.")
                    use Card("pages/", "One file per route. Each imports ../chrome.fct and declares a view in the Site layout.")
        use Band():
            use Section("Adding a page"):
                text "Write pages/pricing.fct containing: import \"../chrome.fct\", then view Pricing at \"/pricing\" in Site: — then add import \"pages/pricing.fct\" to site.fct and a NavLink to it in chrome.fct. Run facet routes site.fct to confirm it is served." class "x-small"
`
}

// ── the library template ─────────────────────────────────────────────────────

func libFiles(p project) map[string]string {
	return map[string]string{
		"main.fct":          libMain(p),
		"layout/flow.fct":   libFlow(p),
		"ui/card.fct":       libCard(p),
		"ui/emptystate.fct": libEmpty(p),
		"example.fct":       libExample(p),
		"facet.json": manifest("github.com/your-org/"+p.Name, "main.fct",
			"A Facet component library."),
		".gitignore": gitignore,
	}
}

func libMain(p project) string {
	return `# ` + p.Name + ` — an importable facet library.
#
# This is the entry file named by ` + "`main`" + ` in facet.json, so an app that writes
#
#     import "github.com/your-org/` + p.Name + `"
#
# gets everything below in one line. Importing a single file
# (github.com/your-org/` + p.Name + `/ui/card.fct) works too and pulls in less.
#
# A library declares components and the CSS they need, and NO views: it has
# nothing to serve. ` + "`facet check main.fct`" + ` is exactly what an importer compiles.
#
# Every component ships the rules for the classes it applies, in its own file's
# ` + "`css:`" + ` block — a facet that renders a grid and leaves the grid rule in one host
# app is not distributable.
import "layout/flow.fct"
import "ui/card.fct"
import "ui/emptystate.fct"

app ` + p.App + `:
`
}

func libFlow(p project) string {
	return `# Flow — the two directions content runs in, as wrappers.
app ` + p.App + `:
    css:
        [data-fa-mount] .x-flow-stack { display: flex; flex-direction: column; gap: 20px; }
        [data-fa-mount] .x-flow-stack-tight { gap: 8px; }
        [data-fa-mount] .x-flow-row { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; }
        [data-fa-mount] .x-flow-row-end { justify-content: flex-end; }

    # A component has at most one slot, so a size parameter would need a match
    # with one slot per arm. The size goes in the name instead — which reads
    # better at the call site anyway.
    component Stack():
        box class "x-flow-stack":
            slot
    component StackTight():
        box class "x-flow-stack x-flow-stack-tight":
            slot
    component Row():
        row class "x-flow-row":
            slot
    component RowEnd():
        row class "x-flow-row x-flow-row-end":
            slot
`
}

func libCard(p project) string {
	return `# Card — a bordered panel with a title and a body, plus a slot variant for
# callers that need to put arbitrary nodes inside one.
app ` + p.App + `:
    css:
        [data-fa-mount] .x-card { border: 1px solid var(--fa-card-border);
                    border-radius: var(--fa-radius); padding: 20px;
                    display: flex; flex-direction: column; gap: 8px; }
        [data-fa-mount] .x-card-title { font-weight: 640; letter-spacing: -.01em; }
        [data-fa-mount] .x-card-body { color: var(--fa-muted); font-size: 14px; line-height: 1.55; }

    component Card(title: text, body: text):
        box class "x-card":
            text "{title}" class "x-card-title"
            text "{body}" class "x-card-body"

    component Panel():
        box class "x-card":
            slot
`
}

func libEmpty(p project) string {
	return `# EmptyState — what a list says when it has nothing in it. A list with no
# empty state is a bug report waiting to be filed.
app ` + p.App + `:
    css:
        [data-fa-mount] .x-empty { color: var(--fa-muted); text-align: center;
                     padding: 40px 20px; display: flex; flex-direction: column; gap: 6px; }
        [data-fa-mount] .x-empty-title { font-weight: 620; color: var(--fa-fg); }

    component EmptyState(title: text, hint: text):
        box class "x-empty":
            text "{title}" class "x-empty-title"
            text "{hint}"
`
}

func libExample(p project) string {
	return `# The gallery: a real app that composes every component the library exports.
# It is not published (facet.json names main.fct as the entry) — it exists so
# ` + "`facet dev example.fct`" + ` shows the library, and so a change that breaks a
# component fails a build here before it reaches an importer.
import "main.fct"

app ` + p.App + `Gallery:
    theme:
        radius "10px"
        maxwidth "820px"

    view Gallery at "/":
        meta title "` + p.App + ` — component gallery"
        meta description "Every component in the ` + p.Name + ` library, rendered."
        use Stack():
            use StackTight():
                text "` + p.App + `"
                text "Every component in this library, rendered."
            use Row():
                use Card("Stack", "A column with real gaps between its children.")
                use Card("Row", "A horizontal line of things that wraps.")
            use Panel():
                use StackTight():
                    text "Panel"
                    text "The same surface as Card, with a slot instead of two strings."
            use EmptyState("Nothing here yet", "This is what a list says when it is empty.")
            use RowEnd():
                text "RowEnd pushes its children to the right."
`
}

// ── deploy assets ────────────────────────────────────────────────────────────
//
// Written by the app template and by `facet deploy <file.fct>`, which drops the
// same four files into an existing project.

// scaffoldDeploy writes the container/deploy assets into the current directory
// for a one-command deploy. Existing files are left untouched.
func scaffoldDeploy(app string) error {
	if err := writeKeeping(map[string]string{
		"Dockerfile":         dockerfile,
		".dockerignore":      dockerignore,
		"docker-compose.yml": dockerCompose,
		".env.example":       envExample,
	}); err != nil {
		return err
	}
	fmt.Printf("\nfacet: deploy assets ready for %s.\n", app)
	fmt.Println("one command (app + Postgres):")
	fmt.Println("  cp .env.example .env   # set FACET_SECRET (facet config --gen-secret)")
	fmt.Println("  docker compose up --build")
	return nil
}

// writeKeeping writes each file, in a stable order, and never overwrites: an
// operator who has edited their compose file must not lose it to a regenerate.
// Keys are slash-separated paths relative to the working directory, exactly as
// in a template's Files map, so the two writers share one convention.
func writeKeeping(files map[string]string) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		target := filepath.FromSlash(name)
		if _, err := os.Stat(target); err == nil {
			fmt.Printf("  kept     %s (already present)\n", name)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(files[name]), 0o644); err != nil {
			return err
		}
		fmt.Printf("  wrote    %s\n", name)
	}
	return nil
}

// ── production deployment ────────────────────────────────────────────────────
//
// `facet deploy <file.fct> --production` writes the configuration for running a
// *built app* — the single binary `facet build --release` produces — rather than
// the toolchain-plus-source arrangement the files above describe. The difference
// is the whole point of a release artifact, and it shows up in every one of them:
// the image has no toolchain and no source in it, the compose stack points at a
// real FacetQL instance and migrates with a separate admin identity, and the
// systemd unit runs one executable as a locked-down non-root user.
//
// They land in deploy/ so that regenerating them can never touch the development
// Dockerfile and compose file a project already has at its root.

// deployment is what the production templates need to know: what the app is
// called, what its binary is called, and which source file compiles it.
type deployment struct {
	App    string // the declared app identifier, e.g. "Blog"
	Binary string // the artifact's file name, e.g. "blog"
	Entry  string // the entry .fct, e.g. "app.fct"
}

// scaffoldProduction writes the production deployment set for a compiled app.
func scaffoldProduction(graph *ir.IR, entry string) error {
	d := deployment{App: graph.App, Binary: strings.ToLower(graph.App), Entry: filepath.Base(entry)}
	files := map[string]string{
		"deploy/Dockerfile":               prodDockerfile(d),
		"deploy/Dockerfile.dockerignore":  prodDockerignore(d),
		"deploy/docker-compose.yml":       prodCompose(d),
		"deploy/" + d.Binary + ".service": prodSystemd(d),
		"deploy/.env.example":             prodEnvExample(d),
	}
	if err := writeKeeping(files); err != nil {
		return err
	}
	fmt.Printf("\nfacet: production deployment ready for %s — it runs the release binary, not the toolchain.\n", d.App)
	fmt.Println("\nbuild the artifact and check it:")
	fmt.Printf("  %-34s # -> dist/%s (one file, no toolchain needed to run it)\n", "facet build --release "+d.Entry, d.Binary)
	fmt.Printf("  %-34s # production readiness, judged from the artifact itself\n", "./dist/"+d.Binary+" doctor")
	fmt.Println("\ncontainer stack (app + FacetQL + a migration step):")
	fmt.Println("  cp deploy/.env.example deploy/.env   # set FACET_SECRET: facet config --gen-secret")
	fmt.Println("  docker compose -f deploy/docker-compose.yml up --build -d")
	fmt.Println("\nor on a host with systemd:")
	fmt.Printf("  sudo install -m755 dist/%s /usr/local/bin/%s\n", d.Binary, d.Binary)
	fmt.Printf("  sudo install -m600 deploy/.env /etc/facet/%s.env\n", d.Binary)
	fmt.Printf("  sudo install -m644 deploy/%s.service /etc/systemd/system/ && sudo systemctl enable --now %s\n", d.Binary, d.Binary)
	return nil
}

// prodDockerfile builds the release artifact with the toolchain image and then
// throws the toolchain away: the shipped layer holds one executable and nothing
// else — no compiler, no source, no shell to exec if something gets in.
func prodDockerfile(d deployment) string {
	return `# syntax=docker/dockerfile:1
# Production image for ` + d.App + ` — one self-contained binary.
#
#   docker build -f deploy/Dockerfile -t ` + d.Binary + `:latest .
#
# Stage 1 packages the app. ` + "`facet build --release`" + ` copies the toolchain binary
# and appends this app's compiled graph to it, so the artifact is produced with
# no Go toolchain anywhere in this build. (If the app imports remote facets, run
# ` + "`facet vendor`" + ` first so this stage needs no network.)
FROM ghcr.io/your-org/facet:latest AS build
WORKDIR /src
COPY . .
RUN ["/usr/local/bin/facet", "build", "--release", "` + d.Entry + `", "-o", "/home/nonroot/` + d.Binary + `"]

# Stage 2 is what actually ships: distroless static — no shell, no package
# manager, no interpreter — plus the app. Nothing from stage 1 comes with it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /home/nonroot/` + d.Binary + ` /usr/local/bin/` + d.Binary + `

# uid 65532. The app writes only to the volume below.
USER nonroot:nonroot
WORKDIR /data
VOLUME ["/data"]

# Behind TLS, only send the session cookie over HTTPS.
ENV FACET_SECURE_COOKIES=1 \
    FACET_UPLOAD_DIR=/data/uploads
EXPOSE 7373

# The app is its own health check: this image has no curl and no shell, and the
# binary already knows which address it listens on.
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/` + d.Binary + `", "healthcheck", "--port", "7373"]

# FACET_DATABASE_URL and FACET_SECRET come from the environment (compose, or the
# orchestrator's secret store). Nothing secret is baked into this image.
ENTRYPOINT ["/usr/local/bin/` + d.Binary + `"]
CMD ["serve", "--addr", "0.0.0.0:7373"]
`
}

// prodDockerignore is the build context for the production image, and it exists
// because it must differ from the project's own .dockerignore in exactly one
// entry: facet_modules. That directory is ignored at the root (it is a cache a
// working copy rebuilds), but a production image should be built from vendored
// facets so the build needs no network and resolves the same bytes every time —
// and the packaging stage compiles the app, so it needs them present.
//
// BuildKit reads `<dockerfile>.dockerignore` in preference to the context root's
// .dockerignore, which is what lets these two answers coexist.
func prodDockerignore(d deployment) string {
	return `# Build context for deploy/Dockerfile (BuildKit prefers this over .dockerignore).
# Unlike the root .dockerignore, facet_modules is NOT ignored: run "facet vendor"
# and the packaging stage compiles offline, from pinned bytes.
.git
dist
.bin
facet-uploads
deploy/.env
.env
.env.local
*.seed.json
*.test.json
`
}

// prodCompose wires the app to a real FacetQL, and separates the two identities
// the stack needs: an admin token that declares indexes and cascade rules at
// migrate time, and an ordinary token the serving process holds. The app never
// runs with the privilege to drop an index — see `doctor`'s store-identity check.
func prodCompose(d deployment) string {
	return `# Production stack for ` + d.App + `: the built app binary, the FacetQL instance it
# stores rows in, and the migration that runs before it serves.
#
#   cp deploy/.env.example deploy/.env      # then set the secrets it names
#   docker compose -f deploy/docker-compose.yml up --build -d
#
# The app is published on the loopback interface only: put a TLS terminator
# (nginx, Caddy, your load balancer) in front of it. FACET_SECURE_COOKIES=1 means
# the session cookie is HTTPS-only, so reaching this over plain http:// will not
# keep you signed in — that is the point.
name: ` + d.Binary + `

services:
  facetql:
    # Set this to the FacetQL image you run.
    image: ghcr.io/f33d3r-inc/facetql:latest
    environment:
      FACETQL_ADMIN_TOKEN: "${FACETQL_ADMIN_TOKEN:?set FACETQL_ADMIN_TOKEN in deploy/.env}"
    volumes:
      - facetql-data:/data
    restart: unless-stopped

  # One shot, before the app: reconcile the schema. This is the only step that
  # needs the admin identity — declaring indexes and referential rules is
  # admin-only in FacetQL, and serving traffic is not.
  migrate:
    build:
      context: ..
      dockerfile: deploy/Dockerfile
    command: ["migrate"]
    depends_on:
      - facetql
    environment:
      FACET_DATABASE_URL: "facetql://${FACETQL_ADMIN_TOKEN}@facetql:8080"
      FACET_SECRET: "${FACET_SECRET:?set FACET_SECRET in deploy/.env — mint one with facet config --gen-secret}"
    restart: "no"

  app:
    build:
      context: ..
      dockerfile: deploy/Dockerfile
    depends_on:
      migrate:
        condition: service_completed_successfully
    environment:
      # The serving identity: no admin privilege, so a leak from this process
      # cannot drop an index or a cascade rule.
      FACET_DATABASE_URL: "facetql://${FACETQL_APP_TOKEN:?set FACETQL_APP_TOKEN in deploy/.env}@facetql:8080"
      FACET_SECRET: ${FACET_SECRET}
      FACET_SECURE_COOKIES: "1"
      FACET_LOG_LEVEL: info
      # The generated /admin dashboard is a read/write surface over every table.
      FACET_ADMIN: "0"
    volumes:
      - ` + d.Binary + `-data:/data
    ports:
      - "127.0.0.1:7373:7373"
    restart: unless-stopped

volumes:
  facetql-data:
  ` + d.Binary + `-data:
`
}

// prodSystemd is the same deployment without containers: one binary, one
// environment file, one unprivileged user. The hardening directives are the ones
// that cost nothing here — this process needs no new privileges, no device
// access, and one writable directory.
func prodSystemd(d deployment) string {
	return `# systemd unit for ` + d.App + ` — the release binary, run as a non-root user.
#
#   sudo useradd --system --home /var/lib/` + d.Binary + ` --shell /usr/sbin/nologin ` + d.Binary + `
#   sudo install -m755 dist/` + d.Binary + ` /usr/local/bin/` + d.Binary + `
#   sudo install -d -m750 -o ` + d.Binary + ` -g ` + d.Binary + ` /etc/facet
#   sudo install -m600 -o ` + d.Binary + ` -g ` + d.Binary + ` deploy/.env /etc/facet/` + d.Binary + `.env
#   sudo install -m644 deploy/` + d.Binary + `.service /etc/systemd/system/
#   sudo systemctl daemon-reload && sudo systemctl enable --now ` + d.Binary + `
#
# Schema changes are a separate, privileged step — run before a restart:
#   sudo -u ` + d.Binary + ` FACET_DATABASE_URL=facetql://<admin-token>@host:8080 /usr/local/bin/` + d.Binary + ` migrate
[Unit]
Description=` + d.App + ` (Facet application)
Documentation=https://github.com/F33D3R-Inc/fct
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=` + d.Binary + `
Group=` + d.Binary + `
# Secrets live in a 0600 file owned by this user, never on the command line.
EnvironmentFile=/etc/facet/` + d.Binary + `.env
ExecStart=/usr/local/bin/` + d.Binary + ` serve --addr 127.0.0.1:7373
ExecReload=/bin/kill -HUP $MAINPID

# The runtime drains in-flight requests on SIGTERM within 25s, then closes the
# store; give it room to finish rather than killing it mid-request.
KillSignal=SIGTERM
TimeoutStopSec=40
Restart=on-failure
RestartSec=2

# /var/lib/` + d.Binary + `, created and owned by systemd — the only writable path.
StateDirectory=` + d.Binary + `
WorkingDirectory=/var/lib/` + d.Binary + `

# Hardening: this process needs no privileges, no devices and no other user's files.
NoNewPrivileges=true
PrivateTmp=true
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictNamespaces=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`
}

// prodEnvExample is the environment a production deployment must supply. Every
// line here is something `facet doctor --production` checks for, and the file is
// the answer to each of those checks.
func prodEnvExample(d deployment) string {
	return `# Production environment for ` + d.App + `. Copy to deploy/.env (git-ignored) and fill
# it in. Everything here is checked by:  ./dist/` + d.Binary + ` doctor
#
# Mint the master secret:  facet config --gen-secret
# It signs sessions, MFA secrets and encrypted columns — rotating it invalidates
# every session and makes @secret columns unreadable, so set it once and keep it.
FACET_SECRET=

# Where the rows live. FacetQL is the stack's own datastore; Postgres also works
# (postgres://user:pw@host:5432/db). Without this the app connects to
# facetql://localhost:8080, which does not exist inside a container.
FACET_DATABASE_URL=facetql://localhost:8080

# Two FacetQL identities, on purpose: the admin token declares indexes and
# cascade rules (` + d.Binary + ` migrate), the app token only reads and writes rows.
# Serving with the admin token is credential excess and doctor reports it.
FACETQL_ADMIN_TOKEN=
FACETQL_APP_TOKEN=

# This process serves plain HTTP; terminate TLS in front of it and leave this at
# 1 so the session cookie is never sent over a plaintext hop.
FACET_SECURE_COOKIES=1

# The generated /admin dashboard is a full read/write surface over every entity
# (admin session required). 0 removes the routes entirely.
FACET_ADMIN=0

# JSON logs on stderr: debug | info | warn | error
FACET_LOG_LEVEL=info
`
}

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
.bin
facet_modules
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
      # Quoted: the value contains a colon, which would otherwise end the scalar.
      FACET_SECRET: "${FACET_SECRET:?set FACET_SECRET in .env — mint one with facet config --gen-secret}"
      FACET_SECURE_COOKIES: "0"
    ports:
      - "7373:7373"

volumes:
  facet-data:
`

const envExample = `# Copy to .env (git-ignored). Real environment variables always override these.
# Mint a strong secret:  facet config --gen-secret
FACET_SECRET=

# Where the data lives. Unset, the dev tools use an in-memory store; ` + "`facet run`" + `
# needs a real one. FacetQL is the stack's own datastore; Postgres also works:
#   FACET_DATABASE_URL=facetql://localhost:8080
FACET_DATABASE_URL=postgres://facet:facet@localhost:5432/facet?sslmode=disable

# Set to 1 behind TLS in production so session cookies are HTTPS-only:
FACET_SECURE_COOKIES=0
`
