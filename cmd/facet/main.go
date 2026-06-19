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
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"facet/internal/compile"
	"facet/runtime"
)

// version is stamped at release time with -ldflags "-X main.version=…".
var version = "1.2.0"

func main() {
	if len(os.Args) < 2 {
		usage()
	}
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
	case "build", "run", "migrate":
		// handled below
	default:
		usage()
	}

	if len(os.Args) < 3 {
		usage()
	}
	cmd, file := os.Args[1], os.Args[2]
	src, err := os.ReadFile(file)
	if err != nil {
		fatal(err)
	}
	graph, err := compile.String(string(src))
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile error: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "build":
		out, _ := json.MarshalIndent(graph, "", "  ")
		fmt.Println(string(out))
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
		if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
			fatal(err)
		}
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
		"app.fct":    starterApp,
		"README.md":  starterReadme,
		".gitignore": "dist/\n",
	}
	for fname, content := range files {
		if err := os.WriteFile(filepath.Join(name, fname), []byte(content), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("created %s/\n\n", name)
	fmt.Println("next:")
	fmt.Printf("  cd %s\n", name)
	fmt.Println("  export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/yourdb")
	fmt.Println("  export FACET_SECRET=$(openssl rand -hex 32)   # signs cookies, encrypts @secret fields")
	fmt.Println("  facet run app.fct")
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  facet new <name>              scaffold a new project")
	fmt.Fprintln(os.Stderr, "  facet build <file.fct>        compile and print the IR")
	fmt.Fprintln(os.Stderr, "  facet run   <file.fct> [addr] serve the web + API projections")
	fmt.Fprintln(os.Stderr, "  facet migrate <file.fct>      reconcile the database schema (--plan to dry-run)")
	fmt.Fprintln(os.Stderr, "  facet version                 print the toolchain version")
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "facet:", err)
	os.Exit(1)
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
