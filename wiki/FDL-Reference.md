# FDL Reference

FDL (Facet Definition Language) is what you write in `.fct` files. The
compiler turns each definition into a server template plus a manifest entry —
or, for client-rendered kinds, a client render body and **zero server
template**.

## Lexical rules

- UTF-8, case-sensitive.
- **Indentation is significant** and *relative* (4 spaces conventional, not
  enforced). **Tabs are an error.** Wrapped HTML continuation lines inside
  `looks:` may align to any column.
- **Comments are full-line only:** a line starting with `#`
  (so CSS `#fff` and `href="#"` inside HTML survive). `#| … |#` for blocks.

## The 8 primitives

```
<kind> <Name>:
    …blocks…
```

| Kind | Role | Renders on | Extra blocks |
|---|---|---|---|
| `facet` | reactive UI fragment | server | — |
| `feed` | ranked, ordered list | server | `order: <field> [asc]` |
| `stream` | append-only, high-frequency | server | `throttle: <dur>`, `window: <n>` |
| `lifecycle` | multi-step state machine | server | `states: a, b, c` |
| `pipe` | continuous data | server | `throttle: <dur>` |
| `vault` | E2E-encrypted content | **client** | `decrypt:` body (no `looks:`) |
| `media` | binary delivery (video/audio) | **client** | `source:` body (no `looks:`) |
| `signal` | ephemeral peer state | **client** | `ttl: <dur>` |

Client-rendered kinds (`vault`/`media`/`signal`) emit **no server template** —
that's enforced, not convention. For vault, the bytes that render the
plaintext never exist on the server.

An unknown primitive, a block on the wrong kind, or a malformed
`throttle:`/`ttl:`/`window:` value is a **compile error that names the fix** —
nothing is silently ignored.

## Shared blocks

### `what:` — the data contract

```
    what:
        post: Post          # capitalized type = one of your Go structs
        count: int
        rating: float
        name: str
        liked: bool
```

Built-in types: `int`, `float`, `str`, `bool`. Anything else is a **custom
domain type** your app provides (a Go struct, or map keys when rendering with
`map[string]any`). Lists travel inside a custom type and are iterated through
a field path: declare `thread: Thread`, loop `for c in thread.comments:`.

Every identifier used in `looks:` must be declared here; an undeclared or
misspelled field is a compile error. `fct build` also emits a typed
`<Name>Data` Go struct per facet (idiomatic naming: `avatar_url` → `AvatarURL`,
`id` → `ID`).

**Computed fields** are derived values written with `=`; the caller never
supplies them (they are excluded from the generated `<Name>Data` struct) and
they resolve server-side at render time. The type is optional.

```
    what:
        price: int
        qty: int
        total = price * qty             # inferred type
        free: bool = total >= 100       # explicit type, uses an earlier computed
```

A computed field may reference any input field and any computed field declared
*above* it; a forward or self reference is a compile error. Integer `+`, `-`,
`*` stay integers (so `total >= 100` compares cleanly); `/` yields a float.

### `looks:` — the template

Raw HTML with three additions:

1. **Interpolation** — `{expr}`, always HTML-escaped contextually.
2. **Control flow** — block form on its own line, or inline:

   ```
        if liked:
            <span>liked!</span>
        else:
            <span>not yet</span>
        for item in items:
            <li>{item.title}</li>
   ```

   Inline: `<button class="like{ if liked } active{ end }">`.

3. **Composition** — child facets and slots:

   ```
        <Avatar user="{user}" size="48"/>          # self-closing child call
        <Card title="Stats">                        # block form fills the child's
            <Badge label="{count}"/>                # default slot: with this content
        </Card>
   ```

   A child declares the hole with `slot:` on a line inside its `looks:`.
   Cycles, unknown children, and unknown props are compile errors.

   **Named slots.** A facet may declare more than one insertion point with
   `slot name:`; the parent targets each with a `fill name:` block. Content not
   inside any `fill` goes to the default `slot:`. Each `slot`/`slot name:` may
   carry default content, shown when that slot is unfilled.

   ```
        facet Frame:                                 # the child: three slots
            looks:
                <header> slot header: </header>
                <main>   slot:        </main>        # the default slot
                <footer>
                    slot footer:
                        <small>© us</small>          # default if unfilled
                </footer>

        facet Page:                                  # the parent fills them
            looks:
                <Frame>
                    fill header:
                        <h1>Title</h1>
                    <p>main body → default slot</p>
                    fill footer:
                        <small>© Page</small>
                </Frame>
   ```

   Filling a slot the child does not declare (`fill typo:`) is a compile error.

**Expressions** support: identifier paths (`post.author.name`),
method/function calls (`viewer.can_view(post)`), comparisons
(`== != < <= > >=`), boolean (`&& || !`), arithmetic (`+ - * / %`), and
literals (`123`, `"text"`, `true`/`false`). Path segments are Title-cased to
Go names, so backend fields/methods must be exported.

### `style:` — cross-platform appearance

A `style:` block sets the facet's root layout and appearance with **design
tokens** the compiler resolves at build time. The resolved style attaches to the
root element, so the *same* declaration renders on web (DOM), iOS (SwiftUI) and
Android (Compose) — there is no per-platform styling code.

```
    style:
        direction: column      # row | column (implies a flex container)
        gap: 2                 # spacing scale: n → n×4px
        pad: 4                 # uniform; `pad: 2 4` = vertical horizontal
        align: center          # start | center | end | stretch
        justify: between       # start | center | end | between
        grow: true             # expand to fill the parent's main axis
        width: fill            # fill | <px> | <pct>%   (also height)
        bg: surface            # color token (or a literal #hex)
        fg: text
        radius: md             # none | sm | md | lg | pill
        font-size: lg          # sm | base | lg | xl | 2xl
        font-weight: bold      # normal | medium | bold | black
```

Color tokens: `fg`/`text`, `muted`, `border`, `bg`/`surface`, `primary`/`accent`,
`on-primary`, `danger`, `transparent`. Every property and token is validated —
an unknown one is a compile error. This block is **not** arbitrary CSS: web-only
effects (`:hover`, `@media`, animations) are intentionally out of scope and
stay in a separate global stylesheet. Only valid on server-rendered primitives.

### `when <event>:` — reactions

```
    when post.like_toggled:
        replace LikeButton with event.payload
```

Declares which server events this facet reacts to; the subscription is implied
and recorded in the manifest.

### `who:` — authorization

```
    who:
        require: is_member
        redact email unless is_self
        redact internal_flags always
```

Policies are bare names your app implements (`Compiled.Policy`). A facet with `who:`
**refuses** the plain `Render` path — it must be rendered with
`RenderFor(view, …)`, which checks `require` (deny → error) and strips
`redact`-ed fields. Redacting a nonexistent field fails closed. `fct audit`
prints the whole access-control surface.

## facet-id

Every rendered facet root gets `data-facet-id` — the address surgical updates
target.

- Auto-derived from the first custom-typed `what:` field:
  `LikeButton:post:{post.id}`.
- No custom-typed field → singleton: `Entries`.
- Override: `facet-id: "…"` in the facet body.

## Per-primitive runtime behavior

- **feed `order:`** — `c.SortFeed("Timeline", items)` sorts your slice by the
  declared field before render. Bare field = **descending**; `asc` flips.
  Missing field / non-comparable type = error, slice untouched.
- **stream/pipe `throttle:`** — enforced in the hub per facet instance,
  trailing-edge coalescing: first frame immediate, intermediate frames
  replaced, latest always delivered.
- **stream `window: n`** — the web runtime trims the container after
  `append`/`prepend` so the DOM never exceeds *n* children.
- **lifecycle `states:`** — `c.Lifecycle("Order")` returns the machine:
  `Initial`, `Valid`, `Next`, `CanTransition(from, to)` (forward-by-one).
- **signal `ttl:`** — `app.Signal(channel, facetID, payload)` relays
  ephemerally (nothing stored). Elements opt in with
  `data-fa-signal="Typing"`; payload lands as `data-*` attributes +
  `.fa-signal-live`, reverted after `ttl:`.
- **vault `decrypt:`** — register the key client-side with
  `fa.vault.key("DM", hexKey)` (never sent to the server); the runtime
  AES-GCM-decrypts `data-fa-envelope` and renders the `decrypt:` body with
  `{plaintext}` / JSON fields, escaped.
- **media `source:`** — the runtime mounts the player inside
  `[data-fa-media]`, filling `{field}` holes from `data-*` attributes;
  `<hls>`/`<dash>` normalize to `<video controls>`.

Client-rendered bodies (`decrypt:` / `source:`) support the same control flow as
`looks:` — `{if expr}…{else}…{end}` and `{for v in items}…{end}` over the
client values (a JSON plaintext exposes its fields and arrays), with dotted
paths and Go-style truthiness. Interpolated values are always HTML-escaped;
the expression subset is paths, literals, `!`, and comparisons (`== != < <= > >=`).

```
vault DM:
    what:
        envelope: str
    decrypt:
        for m in messages:
            <p class="msg">
                if m.mine:
                    <b>you:</b>
                {m.text}
            </p>
```

## Wiring attributes (what HTML attributes mean to the runtime)

| Attribute | Meaning |
|---|---|
| `data-action="post.like"` | click sends this event to `/events`; all other `data-*` attrs become the payload |
| `data-nav` (on `<a>`) | client-side navigation — page swaps without reload, SSE survives |
| `data-fa-optimistic="active"` | toggle these classes instantly on click; server replace reconciles; auto-revert on timeout |
| `data-fa-signal="Name"` | element receives signal payloads |
| `data-fa-vault="Name"` / `data-fa-envelope` | element holds encrypted content for client decrypt |
| `data-fa-media="Name"` | element is a media mount point |

## CLI quick reference

| Command | Purpose |
|---|---|
| `fct new <dir>` | scaffold a runnable project |
| `fct dev [dir]` | run, rebuilding on `.fct` change |
| `fct build <file.fct>` | compile → template + manifest + typed structs |
| `fct check <file\|dir>` | validate (parse + codegen + composition) |
| `fct fmt <file\|dir>` | format |
| `fct audit <file.fct>` | print the access-control surface |
| `fct lsp` | language server (VS Code extension uses it) |
| `fct init/pack/publish/search/add/registry` | community packages |
