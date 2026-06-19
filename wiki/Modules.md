# Modules & Imports

A Facet app does not have to be one file. A file can **import** other `.fct`
files, and the compiler merges every module into one graph *before* placement —
so a large app is many small files that point to each other, and a reusable
**facet** (a bundle of data + logic + UI) is just a module another app pulls in.

It is the same mechanism pointed at two needs:

- point it at **your own files** → split a big app into small ones (no more one
  growing monolith);
- point it at **someone else's module** → reuse a building block (the "library
  of facets" idea — a dislike button, an audit trail, a comments system).

## Importing

Put `import "path"` lines **above** the `app` header. Paths are resolved
relative to the importing file.

```
import "posts.fct"
import "shared/auth-extras.fct"

app MyApp:
    auth
    view Home at "/":
        ...
```

## What a module is

A module is just a normal `.fct` file — it has its own `app` header and is a
valid, runnable app on its own (handy for building/testing a facet in
isolation). When you import it, the compiler takes its declarations — entities,
enums, state, derives, policies, actions, jobs, components, layouts, views,
theme — and merges them into the importing app. The module's `app` *name* is
ignored on import; the root file names the application.

```
# posts.fct — a module: the Post data and the behavior that goes with it
app Posts:
    entity Post:
        id: int
        author: text
        body: text
        dislikes: int
        created: int

    derive postCount: int = count(Post)

    action post(body: text):
        add Post { author: actor, body: body, dislikes: 0, created: now() }

    action dislike(id: int) @optimistic:        # a self-contained capability:
        set Post(id).dislikes = Post(id).dislikes + 1
```

```
# app.fct — pulls the module in and adds auth + UI
import "posts.fct"

app Modular:
    auth
    state draft: text = "" @client

    view Home at "/":
        box:
            text "{postCount} posts"
            input bind draft placeholder "what's happening?"
            button "post" -> post(draft)
            for p in Post by created desc limit 50:
                box:
                    text "{p.author}: {p.body}"
                    button "👎 {p.dislikes}" -> dislike(p.id)
```

`app.fct` uses `Post`, `postCount`, `post`, and `dislike` — all defined in the
module — as if they were declared inline. Run the working example:

```sh
facet build examples/modular/app.fct    # see the merged IR
facet dev   examples/modular/app.fct     # run it (editing posts.fct hot-reloads too)
```

## Why this is the *right* way to package a facet

In a layered framework, a reusable piece has to declare which layer it belongs
to — frontend or backend — and keeping hundreds of community pieces agreeing
across those layers is brittle. Facet has no layers in the source:
**placement is computed.**

So a module can be a **whole vertical slice** — its data field, its server
action, and its button UI bundled together — and the compiler decides what runs
where *when it's merged into the host app*. The dislike facet above carries its
own `dislikes` column, its own `dislike` action, and (if you add one) its own
component, and it just works wherever it's imported. The author of the facet
never thinks about server vs. client, and neither does the person using it.

## Rules & guarantees

- **Resolution** — paths are relative to the importing file; absolute paths are
  allowed too.
- **De-duplication** — a module imported by two files is merged **once** (a
  diamond import is fine).
- **Cycles** — an import cycle is reported as an error, not looped forever.
- **Name collisions** — if two modules declare the same `entity`/`action`/
  `view`/… name, compilation fails with a clear message; rename one.
- **One placement pass** — placement, soundness, and dependency analysis all run
  over the *merged* graph, so a multi-file app has identical semantics to the
  same code in one file.
- **Hot reload** — `facet dev` watches every `.fct` in the project directory, so
  editing an imported module reloads the browser too.

## Local and remote

Everything above is **local** module composition — files on disk, resolved
relative to the importing file. The same `import` mechanism also pulls in
**remote facets** straight from GitHub:

```
import "github.com/acme/dislike"      # a published facet, fetched & pinned
import "./shared/auth.fct"            # still local, unchanged
```

A remote ref (`github.com/owner/repo[/path.fct]`) is fetched as an immutable,
commit-pinned snapshot, cached on disk, and recorded in a committed
`facet.lock` — then merged into your app exactly like a local module, so every
rule above (dedup, cycles, name collisions, one placement pass) applies
unchanged. Versions live in the lock (managed by `facet add`/`update`), never in
the `import` string. See **[The Registry](Registry.md)** for publishing and
consuming.

→ Back to **[Home](Home.md)** · see also **[The Registry](Registry.md)** and the
**[Language Reference](Language-Reference.md)**.
