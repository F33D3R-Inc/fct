# fct in plain English

You should be able to open any `.fct` file and read it without asking anyone.
This page is that translation layer — not a spec, not a restatement of the
in-source comments (which explain *why*; this explains *what*).

For the authoritative, always-current list of exactly what a given `facet`
binary supports — it reads its own compiler tables, so it can't drift out of
sync with this page the way a hand-written doc can — run:

```
facet lang
```

That command is the ground truth for *names*. This page is the ground truth
for *what they mean*, with the closest Python/Swift/React analogy where one
genuinely helps.

## The shape of a file

```
import "../../facets/layout/card.fct"   # pull in a reusable piece, like a Python
                                         # or JS import

app Journal:                            # everything indented under this line
                                         # belongs to "Journal" — indentation is
                                         # significant, like Python (no braces)
    entity Post:                        # a database table / model, like a
                                         # Django model or a SQL CREATE TABLE
        id: int
        title: text
        published: bool

    state draftTitle: text = "" @client # a piece of UI state, like React's
                                         # useState or a SwiftUI @State var.
                                         # `@client` = lives in the browser tab;
                                         # leave it off and it lives on the server

    action publish(id: int):            # a server-side function the UI can call —
        requires member                 # like a Django view + form handler fused
        check id > 0 "bad id"           # into one declaration. `requires`/`check`
        set p in Post where p.id == id: # are guard clauses that run before the
            p.published = true          # body — a failed one aborts with the
                                         # message given, nothing partial happens

    component PostCard(title: text):    # a reusable UI piece with typed
        box class "card":               # parameters — a React function
            text "{title}"              # component, or a SwiftUI View struct

    view Home at "/" in Chrome:         # a page, bound to a URL, rendered
        use PostCard("Hello")           # inside a shared layout — the router
                                         # and the template are one declaration
```

## Structural keywords (declare things)

| Keyword | What it is | Closest analogue |
|---|---|---|
| `app Name:` | The namespace everything in this file belongs to | A Python module / a Django app |
| `entity Name:` | A data model — fields become database columns | A Django model / SQL table |
| `state name: type = default @client\|@server` | A named, typed, mutable value | React `useState`, Swift `@State` — `@client` = browser-side, default is server-side |
| `component Name(args):` | A reusable, parameterized piece of UI | A React function component, a SwiftUI `View` |
| `view Name at "/path" in Layout:` | A page: a route + what renders at it | A Django URL pattern + its template, fused |
| `layout Name:` | A shared shell other views render inside, via `slot` | A Django base template / a React layout wrapper |
| `action name(args):` | A server-side function the UI can trigger | A Django form-handling view / an RPC endpoint |
| `theme:` / `theme dark:` | Named design tokens (colors, spacing) for light/dark | CSS custom properties, declared once |
| `css:` | An escape hatch: literal CSS, for anything the layout primitives can't express | A plain `.css` file |
| `import "path"` | Pull in facets (components/entities/pages) from elsewhere | `import` in any language |
| `auth` | Turns on the built-in session model — gives you `actor`, `login`, `logout` | Django's auth middleware, batteries included |

## View nodes (draw things) — the vocabulary `facet lang` calls "node kinds"

These are the building blocks inside a `component` or `view` body — the
language's entire alternative to raw HTML tags:

| Node | Renders as / does | Analogue |
|---|---|---|
| `box` | A generic container (`<div>`) | An HTML `<div>` / SwiftUI's plain container |
| `row` | A container laid out horizontally (flex-row) | A `<div style="display:flex">` |
| `text "..."` | A text node (`<span>`) | JSX text content |
| `heading` | A heading (`<h1>`…) | `<h1>`/`<h2>` |
| `link "label" -> "/path"` | An anchor tag | `<a href>` |
| `button "label" -> action` | A button wired directly to a server `action` | `<button onClick={...}>`, minus the wiring code |
| `image` | An `<img>` | `<img>` |
| `if cond: ... ` | A conditional block, Python-style (colon + indent) | `if`/`{cond && <X/>}` in JSX |
| `match expr: ...` | Pattern matching, checked exhaustive at compile time | Swift's `switch`, Rust's `match` |
| `for x in list: ...` | A loop over rows/values | `.map()` in JSX, a Django `{% for %}` |
| `use Component(args)` | Instantiate another component | `<Component prop={x} />` in JSX |
| `slot` | Where a layout's child view is inserted | `{children}` in React, `{% block content %}` in Django |
| `form: ...` | A form wired to `state` cells and an `action` | An HTML `<form>` with all the event wiring implicit |
| `tabs bind cell: tab "label" -> "value": ...` | Tabbed panels, driven by one state cell | A controlled tab component |
| `input bind cell` | A two-way-bound text field | `<input value={x} onChange={...}>` — but bidirectional by one line |
| `textarea` / `checkbox` / `toggle` / `radio` / `password` / `newpassword` / `select` / `typeahead` / `upload` | The rest of the two-way-bound controls — each binds to a `state` cell of a specific type | The rest of HTML's form elements, each pre-wired |
| `overlay` | A modal/popover surface | A portal-rendered modal in React |
| `richtext` / `video` | Rich content embeds | Their HTML equivalents |
| `badge` / `icon` | Small decorative primitives | Design-system atoms |

Run `facet lang` any time to see this list exactly as the compiler you're
running actually implements it — new rows get added there, not just here.

## The two arrows and the two kinds of interpolation

- `"{expr}"` inside a string — string interpolation, the same idea as a Python
  f-string (`f"{expr}"`) or Swift's `"\(expr)"`.
- `label -> "/path"` — this control/link navigates to a route.
- `label -> actionName(args)` — this control calls a server action instead of
  navigating. Same arrow, two meanings, disambiguated by what's on the right.
- `bind cellName` — two-way binds a control to a `state` cell: the control
  both reads the cell's current value and writes back to it on change. There
  is no separate "read the value" / "write the value on change" pair to wire
  up — `bind` is both directions in one word.

## Guard clauses inside an `action`

```
action createPost(title: text, slug: text, body: text):
    requires member                                    # a named, zero-argument
                                                          # authorization policy —
                                                          # like a Django
                                                          # @login_required, but
                                                          # declared per-action
    check title != "" "a title is required"            # a precondition + the
    check !contains(slug, " ") "no spaces in a slug"   # error message shown if
                                                          # it fails — like an
                                                          # assert with a
                                                          # user-facing message
    add Post { title: title, slug: slug, body: body }  # the actual write
```

`requires`/`check` run top-to-bottom before the body; the first failure aborts
the whole action with its message, and nothing partial is written.

## Built-in functions

`facet lang` lists these live; as of this page: `abs, ago, commas, compact,
contains, day, floor, len, lower, max, min, money, month, now, rand, round,
take, trim, upper, year`. This is deliberately small and pure — there is no
`replace`/`split`/`slugify`, so anything that needs one (like turning a title
into a URL slug) is written by hand rather than derived, and `check` enforces
whatever invariant that leaves. If you're looking for a string-manipulation
function and it's not in that list, it doesn't exist yet — that's a real gap,
not something you're missing.

## The `@` modifiers

`facet lang` lists these live: `@client, @e2e, @optimistic, @private,
@required, @secret, @server, @softdelete, @unique`. The two you'll see most:
`@client` on a `state` (lives in the browser, resets on reload) vs. its
absence (lives on the server, persists), and `@secret` on an entity field
(never sent to the browser at all, not even in a "hidden" sense — it's simply
absent from what the client receives).

## What this page is not

It is not a language reference for every rule (exhaustiveness checking on
`match`, the exact authorization model, how placement/hydration works) — those
live in the compiler's own comments, which are long on purpose because they
record *why* a rule exists, often after a real bug. Start here to stop being
confused by the vocabulary; go there when you need to know why a rule is the
way it is.
