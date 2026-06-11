# Facet Architecture — How to build a website

A hands-on guide. If you know Django's "start a project, add an app, run it,"
this will feel familiar — except your UI is server-rendered and updates live with
**no client-side framework, no API layer, and no JavaScript you write.**

> New here? Read the one-paragraph mental model, then just follow the steps.

## Mental model (the one thing to understand)

Your server holds the real state. The browser is a **display terminal**. You
write **facets** — server-rendered HTML fragments wired to events. When something
changes, the server re-renders the affected facet and pushes the new HTML over a
live connection; the browser swaps it in place. You never write fetch calls,
state management, or DOM code. The page is a live projection of the server.

---

## 1. Prerequisites

- Go 1.26+ (`go version`).
- This framework. Until it's published, work against a local checkout:
  ```sh
  git clone <your-fork> facet     # the framework repo (module fct.dev)
  ```

## 2. Create a project

```sh
go run ./cmd/fct new myapp        # from inside the framework repo
# (once published: `fct new myapp`)
cd myapp
```

Point it at the framework while it's unpublished, then run it:

```sh
go mod edit -replace fct.dev=/absolute/path/to/facet
go run .
# open http://localhost:7373
```

You now have a live page: a **Playground** with a `Home` facet and a working
`LikeButton`. Click the heart — it updates with no page reload.

## 3. What's in the project (Django comparison)

```
myapp/
  facets/          ← your UI, one .fct file per facet   (≈ Django templates/apps)
    home.fct
    like_button.fct
  main.go          ← wiring: compile facets, handle events, serve   (≈ urls.py + views.py)
  fct.toml         ← project config                                  (≈ settings.py)
  go.mod
```

There is no separate frontend, no `package.json`, no bundler. The page shell (the
**Playground**) is provided by the framework — you don't write `<html>`.

## 4. Anatomy of a facet

A facet has up to three blocks:

```
facet LikeButton:
    what:                      # the data this facet needs
        post: Post
        count: int
        liked: bool

    looks:                     # the HTML (server-rendered)
        <button data-action="post.like" data-post-id="{post.id}">
            if liked:
                ♥ {count}
            else:
                ♡ {count}
        </button>

    when post.like_toggled:    # what to do when this event fires
        replace LikeButton with event.payload
```

- **`what:`** — inputs (`name: Type`, one per line). A capitalized type (`Post`)
  is one of your Go structs.
- **`looks:`** — HTML with `{expr}` holes and `if`/`for`/`else` (block form shown,
  or inline `{ if liked }…{ end }`). Indentation is by feel; wrapped lines are OK.
- **`when <event>:`** — how the facet reacts to a server event.

`data-action="post.like"` on an element means "when clicked, send the event
`post.like` to the server." That's the only wiring you write on the HTML side.

## 5. How a click flows (no client code involved)

```
user clicks  →  runtime POSTs {type:"post.like", payload:{postId}} to /events
             →  YOUR Go handler runs, changes state, returns a new fragment
             →  the framework signs it and pushes it over SSE
             →  the runtime verifies the signature and swaps the node in place
```

The handler lives in `main.go`:

```go
app.On("post.like", func(ctx fa.Ctx) ([]fa.Event, error) {
    post.Liked = !post.Liked              // change real state (server-side)
    if post.Liked { post.Count++ } else { post.Count-- }
    return []fa.Event{{
        Op:       "replace",
        FacetID:  "LikeButton:post:" + post.ID,
        Fragment: string(like()),          // re-render the facet
    }}, nil
})
```

## 6. Add your own facet (tutorial)

**a.** Create `facets/greeting.fct`:

```
facet Greeting:
    what:
        name: str
    looks:
        <div class="greeting" data-action="greeting.wave">
            👋 Hello, {name} — click to wave back
        </div>
```

**b.** Render it on the page. In `main.go`, where the page content is built:

```go
func(r *http.Request) template.HTML {
    return c.MustRender("Greeting", map[string]any{"Name": "world"}) + like()
}
```

**c.** Handle its event (optional):

```go
app.On("greeting.wave", func(ctx fa.Ctx) ([]fa.Event, error) {
    return []fa.Event{{Op: "replace", FacetID: "Greeting",
        Fragment: string(c.MustRender("Greeting", map[string]any{"Name": "friend 👋"}))}}, nil
})
```

**d.** `go run .` and refresh. (Or `go run ./cmd/fct dev myapp` to auto-rebuild on
every `.fct` save.)

That's the whole loop: write a facet, render it, handle its events.

## 7. Where state lives

In your Go process — a variable, a database row, whatever. **Never in the
browser.** Because the server is authoritative, there are no stale caches, no
optimistic-update bugs, no sync logic. The client cannot hold state it shouldn't,
and pushed updates are HMAC-signed so a tampered fragment is rejected.

## 8. The CLI

| Command | Does |
|---|---|
| `fct new <dir>` | scaffold a project |
| `fct dev [dir]` | run, rebuilding on `.fct` change |
| `fct build <file.fct>` | compile a facet to template + manifest |
| `fct parse` / `fct lex` | inspect the compiler (debug) |

## 9. What's not built yet (so you're not surprised)

This is an early, working core. All 8 primitives compile and run (server + web
runtime): child-facet tags (`<Avatar/>` + `slot:`), typed data structs, rich
expressions, feed ranking, stream throttle/window, lifecycle transitions, signal
relay + TTL, vault client-side decrypt, and media mounting all work. Not yet:
per-primitive enforcement in the **native** runtimes (FacetKit / Compose), named
slots, client-side `if`/`for` in vault/media bodies, and non-Go backend targets.
See the roadmap in `README.md`.

---

For *what* the framework is and *why* it's built this way, see `README.md` and
`DECISIONS.md`.
