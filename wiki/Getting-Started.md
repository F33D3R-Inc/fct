# Getting Started

From nothing to a live, self-updating page in about two minutes.

## 1. Install Go

FA apps *are* Go programs — the server is a single Go binary. Install
**Go 1.26+** from <https://go.dev/dl/> and check:

```sh
go version
```

You do **not** need Node, npm, or any frontend toolchain. There is no bundler.

## 2. Install the `fct` CLI

```sh
go install github.com/F33D3R-Inc/fct/cmd/fct@latest
```

This puts `fct` in your Go bin directory (`go env GOBIN`, or
`$(go env GOPATH)/bin` — make sure it's on your `PATH`). Alternatively, grab a
prebuilt binary from the
[Releases page](https://github.com/F33D3R-Inc/fct/releases/latest).

Verify:

```sh
fct version
```

## 3. Create a project

```sh
fct new myapp
cd myapp
go run .
```

Open <http://localhost:7373>. You have a live page: a `Home` facet, an `About`
page, and a working `LikeButton`. Click the heart — it updates with no page
reload, because the server re-rendered it and pushed the new HTML over SSE.

For auto-rebuild on every `.fct` save, use:

```sh
fct dev
```

> **Port:** set `FA_ADDR` to override (`FA_ADDR=:8080 go run .`).

## 4. What you're looking at

```
myapp/
  facets/           ← your UI: one .fct file per facet
    home.fct
    about.fct
    like_button.fct
  main.go           ← all the wiring: compile facets, handle events, serve
  fct.toml          ← project config
  Dockerfile        ← production build (distroless static binary)
  go.mod
```

- **`facets/*.fct`** is where you spend your time. Each file declares a facet:
  what data it needs (`what:`), what HTML it renders (`looks:`), and how it
  reacts to events (`when:`).
- **`main.go`** is pure wiring. It compiles the facets at startup
  (`std.CompileDir("facets")` — no build step), declares one handler per event
  type (`app.On("post.like", …)`), registers routes (`app.Route("/", …)`), and
  serves. Read it top to bottom — it's under 100 lines.
- There is no `<html>` file to write. The framework provides the page shell
  (the **Playground**); your facets render inside it.

## 5. Make a change

Open `facets/home.fct`, change some text in the `looks:` block, save. If you're
running `fct dev`, the page rebuilds; otherwise restart `go run .`. That's the
edit loop.

## Next

→ **[Building Your First Website](Building-Your-First-Website.md)** — pages,
your own facets and events, a form, and login, built into a real little site.
