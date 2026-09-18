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
| `proc Name(params) -> RetType:` | A general-purpose, always-server-side function — real local variables, loops, and branches | A plain function/method (Python `def`, Swift `func`) that only ever runs on the server |
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
  f-string (`f"{expr}"`) or Swift's `"\(expr)"`. **`{expr}` only interpolates
  in a fixed, small set of positions**: node text, headings, labels (button/
  link/tab/option labels), placeholders, `class`, `meta`, and a link/image/
  icon/video destination — everywhere a view renders something, and nowhere
  else. Concretely, `{expr}` does **not** interpolate in: a component
  argument, `style`, an option/tab/`case` value, a `check` message, a `requires`
  argument, a `theme` token, a service's base URL, a webhook path, a view's own
  route, a `state` default, or **any text literal inside any other expression**
  — an action argument, a `set`/`let` value, a `where` operand, or (as of this
  fix) a `proc` body's own `let`/`set`/`return`/`do` argument. Writing `{expr}`
  in any of those positions is a **compile error**, not silent text — the
  compiler names the position, quotes the string, and hands back the `+`
  concatenation to write instead (`"{slot}:{pid}:{score}"` becomes
  `slot + ":" + pid + ":" + score`, since `+` already stringifies an int
  against a text operand — see the `proc` section below). This is deliberately
  narrow, not a blanket ban on `{` in a literal position: braces around a name
  nothing in scope defines (a JSON sample, a CSS block, prose about FDL syntax
  itself) still compile as plain text everywhere, because there was no value
  for them to silently drop.
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

## `proc`: real local variables and control flow, always on the server

```
proc classify(x: int, limit: int) -> text:  # a peer declaration to entity/
                                             # action/view/service — a plain
    if x > limit:                           # function with a real body. This
        return "high"                       # `if` is proc control flow, NOT
    else:                                   # the view-rendering `if` node
        return "low"                        # from the node table above

proc sumTo(n: int) -> int:
    let mut total = 0        # `let mut` = a genuinely reassignable local
    let mut i = 0
    loop i < n:               # while-style: check the condition, then run
        total = total + i     # the body while it holds. There's no separate
        i = i + 1              # for-loop keyword — counting is just a
    return total                # `let mut` counter bumped inside the loop

action run(x: int, limit: int, n: int):
    let label = do classify(x, limit)   # `do` calls a proc. Bound, it's the
    let total = do sumTo(n)             # same shape as `let v = call
    ...                                  # Service.op(...)` — but `do` is for
                                         # code in this same app; `call` is
                                         # for an external service
```

A `proc` is a new peer declaration alongside `entity`/`action`/`view`/`service`
— the place for real computation, declared `proc Name(params) -> RetType:`
(drop `-> RetType` for a proc that returns nothing). Unlike `action`, whose
placement (browser vs. server) the compiler infers from what it touches, a
`proc` is unconditionally server-executed — there's no browser version of it
and no placement question to reason about. In exchange, its body can do
things an `action` body can't:

- `let name = expr` declares an immutable local; `let mut name = expr`
  declares one you can reassign later with a plain `name = expr` — a real
  local variable, like a Python or Swift local (contrast with an `action`'s
  `let`, below).
- `if cond: ... else: ...` is proc control flow with a statement body —
  distinct from the `if cond: ...` view node in the node table above, which
  only decides what to render and carries no statements of its own.
- `loop cond: ...` runs its body while `cond` holds, checked before each
  pass (a while loop). A proc has no separate for-loop form.
- `break` exits the nearest enclosing `loop` immediately; `continue` skips
  straight to its next condition check — the usual meanings.
- `return expr` (bare `return` for a proc with no return type) exits the
  whole proc immediately, from anywhere, including from inside a nested `if`
  inside a `loop`.

A `proc` body is pure: it can see only its own parameters and its own
`let`/`let mut` locals. It cannot read or write `state`, `entity` rows, or
anything else from the surrounding app — `requires`, `check`, `establish`,
`add`, `set`, `remove`, and `clear` are all invalid inside one. Data has to
come in as a parameter; results have to go out via `return`, to be used by
whichever `action` called it.

**A proc body's own string literals never interpolate** — `{expr}` only
renders inside a view (see "The two arrows and the two kinds of
interpolation" above), and a proc computes a plain value with no rendering
involved at all. Build a text value by concatenating with `+` instead, which
already stringifies an int (or any value) against a text operand:

```
proc tag(slot: int, pid: int, score: int) -> text:
    return slot + ":" + pid + ":" + score   # "3:42:99", not the four
                                             # literal braces of
                                             # "{slot}:{pid}:{score}"
```

Writing `"{slot}:{pid}:{score}"` here is a compile error, not a silently
wrong runtime value — the compiler names the offending literal and hands
back the exact `+` form to write instead.

`do ProcName(args)` calls a proc as a statement — fire-and-forget, its result
discarded — the same way `call Service.op(args)` invokes an external service.
Bound, `let x = do ProcName(args)` keeps the result, same shape as
`let x = call Service.op(args)`: `do` is for a proc defined in this same app,
`call` is for an external service. (One current sharp edge: `let mut` can't
bind a `do` call directly — bind it plainly first, then copy it into a
`let mut` if you need to mutate it afterward.)

Because a `proc` always runs on the server, any `action` that calls one
(`do ProcName(args)` or `let x = do ProcName(args)`) is pinned to the server
too — there's no browser-side way to run proc code, so the compiler has no
placement choice left to make for that action.

**A proc parameter may be list-typed** (`p: [T]`), exactly like a proc's own
return type — the shape two cooperating procs need, one producing a list the
next consumes as its own parameter, not just as an internal local:

```
proc buildRange(n: int) -> [int]:
    let mut xs = []
    let mut i = 0
    loop i < n:
        xs = append(xs, i)
        i = i + 1
    return xs

proc sumList(xs: [int]) -> int:      # a list-typed PARAMETER
    let mut total = 0
    let mut i = 0
    loop i < len(xs):
        total = total + xs[i]
        i = i + 1
    return total

action run(n: int):
    let xs = do buildRange(n)        # xs: a real list value, bound from a proc
    let total = do sumList(xs)       # …passed straight into another proc's [int] param
    ...
```

`len(xs)`/`xs[i]` on a list-typed parameter type-check exactly like they
already do on a `let mut xs = [1,2,3]` local. It is bound by copy, like every
other proc-local value: the callee may copy it into a `let mut` local and
mutate that copy freely without the caller's original array ever changing.
Only an `action`/`component`/`policy` parameter stays scalar-only — a proc
parameter is the one place besides a proc's own return type where `[T]` is
allowed.

**`xs[i]` out of range is a runtime error, not a clamp.** Unlike
`charAt`/`slice`/`take` (which clamp a bad index to something in range), an
array/byte-buffer index read past the array's length ends the request as a
clean error, not a silently-wrong answer and not a Go panic reaching the
HTTP layer — bounds are data-dependent, so this is the one failure a proc's
own compile-time checks can never fully rule out.

**`&&` and `||` short-circuit**, exactly like every mainstream language
(Go, JS, Python, Rust, Java, C, …): `a && b` evaluates `a` first, and only
evaluates `b` at all if `a` is true; `a || b` evaluates `a` first, and only
evaluates `b` if `a` is false. This is what makes the standard bounds-guard
idiom safe: `if i < len(xs) && xs[i] == target: ...` never evaluates
`xs[i]` when `i` is already out of range, because the left side being false
already decides the whole `&&`. The mirror form, `i >= len(xs) || xs[i] ==
target`, is equally safe: once the left side is true, the right side (the
unguarded index read) never runs. This holds for both `proc` bodies and
`action`/view/policy/derive expressions — the two interpreters
(`runtime/eval.go`'s `eval()`/`evalInFrame()`, and their client mirror
`assets/facet.js`) agree on this exactly as they do on every other operator.
Because placement (`action` server/client inference) is decided statically
at compile time rather than per call, an impure builtin (`now()`/`rand()`)
that only appears on the right of a short-circuited `&&`/`||` still forces
that action to server placement — it is a possible effect of calling the
action at all, even on calls where it doesn't end up running.

### `let` means something different in an `action` than in a `proc`

Same keyword, two different jobs — the thing most likely to trip up a cold
read:

- **Inside an `action`**, `let` only ever binds the result of a
  request→response call: `let verdict = call Verity.check(id)` or
  `let r = do sum3(x, y, z)`. There is no general local variable in an
  action — `let` here just means "name the thing that came back." Everything
  else an action does to change something goes through `set`/`add`/`remove`/
  `clear` against real entities, never through a local variable.
- **Inside a `proc`**, `let`/`let mut` is a real local variable: `let mut
  total = a` starts one at `a`, and a later `total = total + b` changes it in
  place. There's no `call` or `set`/`add`/`remove`/`clear` inside a proc at
  all — parameters plus locals are the entire vocabulary.

### `float`: a second numeric type, proc-only

`3.14`, `0.5`, `-2.0` are `float` literals — a real scalar type, a Go
`float64` under the hood, alongside `int | text | bool | money | date`. It is
usable as a proc parameter, a `let`/`let mut` local, a return type, and an
array/map element value (never a map *key* — see below) — and nowhere else
yet: not an entity field, not a `state` cell, not a record field, not a
component or action parameter. Every one of those gives a clear compile
error naming the reason (no database column, no client-side representation
in `assets/facet.js`, no wire encoding) rather than silently accepting it.

```
proc weightedAverage() -> int:
    let xs = [1.5, 2.5, 3.5]        # a float array literal
    let mut total = 0.0
    let mut i = 0
    loop i < len(xs):
        total = total + xs[i]        # float + float
        i = i + 1
    return round(total / 3.0)        # -> int, crossing back to an action
```

**No automatic int/float promotion.** `1 + 2.5` is a compile error, not a
silently-computed `3.5` — every arithmetic operator (`+ - * /`) and every
comparison (`== != < <= > >=`) requires both operands to already be the same
numeric type. This is the choice consistent with the rest of the language:
the *only* implicit widening anywhere is "anything converts to text"
(interpolation) — int and money and date already share one representation
rather than being "promoted" into each other — so int and float, which are
two genuinely different machine representations (`int` vs `float64`), get no
special-cased crossing either. `toFloat(n)` and `toInt(x)` convert
explicitly wherever a program needs to cross the boundary — `toInt` truncates
toward zero. `%` (modulo) is int-only outright: a fractional modulus is a
compile error, not a runtime one.

`floor(x)` and `round(x)` always return an **int** — the natural reading of
"I rounded, so now it's a whole number" — while an int input passes through
unchanged (their pre-float identity behavior, preserved exactly).
`round` uses round-half-away-from-zero (`round(2.5) == 3`, `round(-2.5) ==
-3`, matching Go's own `math.Round` — not round-half-to-even). `abs(x)`
instead *preserves* whichever flavor it's handed (`abs(-2.5)` is a float,
`abs(-2)` is an int), since taking an absolute value never changes whether a
number has a fraction.

A float cannot be used with the bitwise operators (`& | ^ << >> ~`, already
int-only) or as a map key (alongside bool/money/date, which are refused for
the same "no good equality/hashing story" reason — `0.1 + 0.2 != 0.3` is
float's own version of that problem).

Because a proc is unconditionally server-executed but the *action* that
calls one is not necessarily — and `assets/facet.js` has no float
representation — an action may not bind a float-returning proc's result
(`let x = do FloatProc()` is refused); a fire-and-forget `do FloatProc()`
(the result discarded) is fine, and float values flow freely between procs
calling each other.

### I/O capabilities: `uses`, and real file/HTTP effects

A proc's header may declare the I/O capabilities its body is allowed to use,
trailing the return type (or the parameter list, for a proc with none) the
same way `requires <policy>` trails a `mount`/`view` header:

```
proc readConfig(path: text) -> text uses io.file:
    return readFile(path)

proc notifyAndLog(url: text, path: text, msg: text) uses io.net, io.file:
    httpPost(url, msg)     # a bare call — the effect is the point, result discarded
    writeFile(path, msg)
```

Two capabilities exist today: **`io.file`** gates `readFile(path: text) ->
text` and `writeFile(path: text, content: text) -> bool`; **`io.net`** gates
`httpGet(url: text) -> text` and `httpPost(url: text, body: text) -> text`.
All four are ordinary builtins usable anywhere an expression is (bound with
`let`, returned, passed as an argument) or, fire-and-forget, as a bare
statement on their own line when only the side effect matters — the same
shape `do ProcName(args)` already has for a proc call, but for a builtin.

A proc that calls one of these four without declaring the matching capability
is a **compile error** naming both the missing capability and the builtin
that needed it — this is checked statically, the same "catch it before it
runs" stance every other proc restriction in this section takes, not a
runtime permission check. Declaring `io.net` does not grant `io.file` or vice
versa; each is checked independently against exactly what the proc's own body
calls (not what any proc it `do`-calls uses). Like bitwise operators and
`float`, all four builtins are proc-only — calling one from an action, view,
policy, or derive is refused outright, because only a proc is unconditionally
server-executed with no client mirror that would have to agree with a real
file or network effect.

**Sandboxing.** `readFile`/`writeFile` resolve `path` against a single
configured root directory (`FACET_DATA_DIR`, defaulting to `./facet-data`
beside the running server) — the same "one configured root, no escaping it"
convention `FACET_UPLOAD_DIR` already uses for uploaded files. An absolute
path is refused outright; a relative path is cleaned and resolved, and if the
result would land outside that root (a `../../` climb, however many levels)
it is refused too — a proc can never read or write outside its sandbox by
construction. `writeFile` creates any missing parent directories inside the
sandbox and overwrites an existing file at `path`; it returns `true` on
success.

**HTTP.** `httpGet`/`httpPost` use a real `net/http` client with a 5-second
timeout — matching this codebase's existing convention for the authority's
own outbound calls to a declared `service` brain (`call Service.op(...)`),
since calling out from a proc is the same shape of egress to an
author-supplied URL. A non-2xx response is a clean runtime error naming the
status code, not the error page's body masquerading as a real answer.

**Errors.** Every way one of these four can fail for reasons outside the
program's control — file not found, permission denied, a path escaping the
sandbox, an unreachable host, a timeout, a non-2xx response — surfaces as a
clean runtime error that aborts the proc (and whatever action called it, via
`do`'s existing failure path), never a crash: the same "clean error, not
silent corruption" contract an out-of-bounds array read or an out-of-range
byte write already has.

### Structured concurrency: `spawn`, `join`, and channels

`spawn ProcName(args)` runs a proc call concurrently — a real Go goroutine
under the hood (this milestone's reference/bootstrap implementation; no green
threads, no scheduler of the language's own yet) — and immediately hands back
a task handle, always bound: `let h = spawn ProcName(args)`. `join h` blocks
until that goroutine finishes; bound, `let r = join h` also keeps its result.

```
proc fetchOne(url: text) -> text uses io.net:
    return httpGet(url)

proc fetchBoth(u1: text, u2: text) -> text uses io.net:
    let h1 = spawn fetchOne(u1)   # both requests start now, concurrently
    let h2 = spawn fetchOne(u2)
    let r1 = join h1               # blocks until fetchOne(u1) finishes
    let r2 = join h2               # already finished or blocks the rest of the way
    return r1 + r2
```

**Structured, not fire-and-forget.** A spawned task can never outlive the
proc call that started it. `spawn` has no bare/unbound form at all — a
discarded handle could never be joined, which is exactly the leaked
goroutine structured concurrency forbids — and a handle, once produced by
`spawn`, is not an ordinary value: it cannot be used in an expression,
returned, or passed as an argument, only consumed by exactly one `join`. The
compiler proves every handle is joined **before the statement block it was
spawned in ends** — the proc's own top level, or a `loop`/`if` body — which
is also why no `if`, `loop`, `break`, `continue`, or `return` may appear in
that same block while a handle is still unjoined: any of those could exit
the block on some path before reaching a `join` written later in it. Spawn
everything, join everything, then branch or return:

```
proc runIt(a: int) -> int:
    let h = spawn slow(a)
    if a > 0:      # ← compile error: `h` is still unjoined here
        return 1
    let r = join h
    return r
```

**Concurrency safety.** A spawned call gets its own independent frame (its
own parameters and `let` locals) — nothing is ever shared with the spawning
call's frame, so two spawned tasks mutating their own `let mut` locals can
never corrupt each other. A proc's existing rules already make this safe by
construction: a proc can't touch entities or `state` at all, so nothing a
spawned task does can race the durable store or another request. The two
capability builtins (`readFile`/`writeFile`/`httpGet`/`httpPost`) are
likewise safe to run concurrently — they only ever touch the sandbox's root
path (fixed at startup) and a shared `net/http` client (already safe for
concurrent use). A panic inside a spawned task — any bug that slips past
every static check — is recovered and surfaced through `join` as an ordinary
error instead of crashing the server, the same "clean error" contract every
other proc failure already has.

**Channels.** `channel()` creates a small (single-slot buffered), text-only
channel — deliberately minimal, not a full CSP feature set — represented as
a plain `int` handle, so it is usable anywhere an `int` already is: a proc
parameter, a `let` local, an argument to another proc. `send(ch, value)`
blocks until `value` is delivered; `recv(ch)` blocks until a value arrives.
Channels are how two independently spawned tasks talk to each other, since
they otherwise share no state at all:

```
proc producer(ch: int, msg: text) -> bool:
    return send(ch, msg)

proc consumer(ch: int) -> text:
    return recv(ch)

proc roundTrip(msg: text) -> text:
    let ch = channel()
    let h1 = spawn producer(ch, msg)
    let h2 = spawn consumer(ch)
    join h1
    return join h2
```

`spawn`/`join`/`channel`/`send`/`recv` are all proc-only, the same "no client
mirror to disagree with it" reasoning bitwise operators, `float`, and the I/O
builtins already follow.

## Built-in functions

`facet lang` lists these live; as of this page: `abs, ago, channel, charAt,
commas, compact, contains, day, floor, httpGet, httpPost, len, lower, max,
min, money, month, now, rand, readFile, recv, round, send, slice, split,
take, toFloat, toInt, trim, upper, writeFile, year`. Most of this list is
deliberately small and pure — there is no `replace`/`slugify`, so anything
that needs one (like turning a title into a URL slug) is written by hand
rather than derived, and `check` enforces whatever invariant that leaves. If
you're looking for a string-manipulation function and it's not in that list,
it doesn't exist yet — that's a real gap, not something you're missing.
`readFile`/`writeFile`/`httpGet`/`httpPost` are the exception to "pure": real,
capability-gated I/O effects, proc-only — see the `uses` subsection above.

**String decomposition.** `split(s, sep) -> [text]` splits `s` on every
occurrence of `sep`, matching Go's own `strings.Split` exactly — an empty
`sep` splits after every character, consecutive occurrences of `sep` produce
empty-string elements, and a `sep` never found in `s` returns a
single-element result holding `s` unchanged. `slice(s, start, end) -> text`
is the general (not prefix-only) counterpart to `take`: it returns the
substring from `start` up to (not including) `end`. `charAt(s, i) -> text`
returns the single character at `i` as a length-1 string (there is no
separate character/rune type). All three are rune-indexed, not byte-indexed
— consistent with `take`/`len`, which already count/slice by rune rather than
by byte — so a multi-byte character is never cut in half. Like `take`,
`slice`/`charAt` never raise a runtime error for an out-of-range `start`/
`end`/`i`: both bounds of `slice` are clamped into `[0, len(s)]`
independently, and a `start` left past `end` after clamping (or a `charAt`
index outside the string) yields `""` rather than an error. Together with
`len` and a `loop`, these are enough to write a real line-oriented scan —
`split(src, "\n")` followed by a per-line `charAt`/`len` pass is exactly what
a hand-written lexer needs.

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
