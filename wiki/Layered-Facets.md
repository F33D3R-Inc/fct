# Layered facets — typed bricks

A Facet app is built in **layers that snap together like Lego bricks**, and
composite down into **one surface**. You never see the layers in the running app
— they are how it is *assembled*, never how it *renders*. The compiler flattens
the whole stack into a single graph and runs placement over it exactly once.

There are four typed bricks. Each has studs that only fit compatible studs, so a
mismatched stack is a compile error, not a broken page.

| Brick | Keyword | What it is |
|---|---|---|
| **Playground** | `playground` | The baseplate. Global concerns (`auth`, `theme`) and the one `mount` for a wireframe. Accepts a wireframe and nothing else. |
| **Wireframe** | `wireframe` | Pure structure. Declares typed `socket`s and a `frame` that lays them out. No data, no behavior. |
| **UI facet** | `ui Name in <socket>` | Skin and presentation — `content` for a socket, plus components and client state. May not declare durable data. |
| **Data facet** | `data Name in <socket>` | Durable data, server logic, authorization — entities, actions, policies, derives — **and** its own `content` for a socket. |

The stack is ordered and constrained: a **data facet can't sit on bare
playground** — it has nothing to grab onto. It only snaps into a wireframe socket
that accepts `data`. That's the whole idea: the wireframe is the gatekeeper.

---

## The shape of a layered build

```
playground  ─ mounts ─▶  wireframe  ─ sockets ─▶  ui / data facets
   (auth, theme)          (frame: where           (content that snaps
                           regions live)            into each socket)
```

Run the **playground** file; `import` pulls every other brick into the pool, and
the compiler composes by kind and socket — not by who imported whom.

### Playground — the baseplate

```
import "wireframe.fct"

playground X:
    auth
    theme:
        bg "#15202b"
        accent "#1d9bf0"
        card-border "#38444d"
        maxwidth "1100px"
    mount Shell
```

### Wireframe — typed sockets + a frame

```
import "nav.fct"
import "feed.fct"
import "trends.fct"

wireframe Shell:
    socket nav: ui
    socket feed: data
    socket aside: ui
    frame:
        row:
            box:
                slot nav
            box:
                slot feed
            box:
                slot aside
```

A `socket <name>: <ui|data>` declares a typed slot. The `frame` is an ordinary
node tree (`row`/`box`/…) with a `slot <name>` marking where each socket's
content composites in. Because the frame is a [`row`](Views-and-UI.md#row), the
three regions sit side by side on a wide window and **stack into one column on a
narrow one** — responsive by construction.

### UI and data facets — the content that snaps in

```
ui Nav in nav:
    content:
        text "𝕏 · F33D3R"
        link "Home" -> "/"
        if actor != "guest":
            button "log out" -> logout
```

```
data Feed in feed:
    entity Tweet:
        id: int
        author: text
        body: text
        created: int
    policy member:
        actor != "guest"
    action tweet(body: text):
        requires member
        add Tweet { author: actor, body: body, created: now() }
    content:
        for t in Tweet by created desc limit 50:
            box:
                text "{t.author}: {t.body}"
```

A data facet carries its data **and** its UI together; the wireframe decides
where that UI lands. Placement is still computed over the whole composited graph
— `tweet` uses `now()`, so it pins to the authority; the `member` policy is
enforced there no matter what any client sends.

---

## Why the studs are typed

`socket feed: data` means *only a `data` facet fits here*. Snap a `ui` facet into
it and the build fails:

```
socket "feed" accepts `data` facets, but "Nav" is a `ui` facet — the bricks don't fit
```

Other guardrails, all compile-time:

- A **`ui` facet may not declare an entity** — durable data belongs in a `data`
  facet.
- A **`playground` mounts exactly one wireframe** and holds only `auth`/`theme`.
- Snapping into a **socket that doesn't exist** lists the ones that do.
- A **plain `app` can't be mixed into a layered build** — start from a playground.

These aren't lint rules bolted on top; they fall out of the brick model. If it
compiles, the bricks fit.

---

## It renders as one surface

The output of composition is a single page at `/`: the wireframe frame with every
socket filled. There are no per-facet panels or seams — a box directly inside a
`row` is treated as a **structural column** (transparent, borderless), so the
layers melt into one cohesive surface, the way any real site looks like one page
and not three widgets bolted together.

Swapping what fills a socket (or reordering the frame) restyles the app without
touching the data facet at all. That is the payoff of typed bricks: **each layer
is replaced independently, and they still compose into one app.**

A complete runnable example is in [`examples/layered/`](https://github.com/F33D3R-Inc/fct/tree/main/examples/layered):

```sh
facet dev examples/layered/playground.fct
```

→ Back to **[Home](Home.md)** · see also **[Modules & Imports](Modules.md)**,
**[The Registry](Registry.md)**, and **[Views & UI](Views-and-UI.md)**.
