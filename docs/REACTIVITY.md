# FA Reactivity — Compiled Fine-Grained Client State

> **Status: design + active build.** This is FA's north star: do everything React
> can do (June 2026), with *minimal* front-end JS, as a compiler-native root
> solution — not a runtime bolted on.

## The thesis: compile, don't interpret

A client-side VM that *interprets* FDL is a patch — you ship a runtime and spend
forever shrinking it toward zero. The root solution is the opposite: the `fct`
compiler **bakes** each facet's reactivity into the smallest possible code and
ships almost no framework. This is the Svelte/Solid model — compiled
fine-grained reactivity, **no virtual DOM, no diffing, no reconciliation
runtime** — which is exactly what the industry has already moved to as it leaves
React's VDOM behind.

FA's unfair advantage: Solid and Svelte had to *invent* a compiler and bolt it
onto JavaScript. **FA already owns a typed language and a compiler.** Computed
`what:` fields (`total = price * qty`) are already reactive derived values with a
real dependency graph (`internal/codegen/check.go` → `checkComputed`). We are not
catching up to React — we are building native what its successors bolt on.

## The model

1. **FDL gains a client reactive layer**, all typed, all in FDL, zero hand-written JS:
   - `state` — local reactive values (signals) with defaults.
   - **derived** — already exists as computed `what:` fields; we extend their
     dependency analysis to run client-side.
   - `effect` — run when a dependency changes (later brick).
   - client **actions** — bind a DOM event to a state mutation.

2. **The compiler extracts the dependency graph** and emits a precise per-facet
   update program: *when signal X changes, touch exactly this DOM node.* No VDOM,
   no diff. The "framework" disappears into the compiled output.

3. **Two tiers, the compiler chooses (not the author):**
   - **Tier 1 — tiny generated DOM-surgery** for text/attr/class/list updates
     (95% of UI). The minimal accepted JS; a few lines per facet, Svelte-style.
   - **Tier 2 — WASM** for heavy compute: canvas/WebGL, a rich-text editing
     buffer, physics, crypto, image processing. "Logic and math, not JS." WASM
     for *compute*, Tier-1 code for *DOM* — never fight the WASM↔DOM boundary.

4. **Server-authoritative stays the default** (the security moat is untouched).
   The reactive layer owns only **ephemeral** client state — drag offset, cursor,
   animation progress, unsaved input — as local signals that reconcile to the
   server on commit.

5. **Backend-neutral by construction.** All of this is the *compiler's* output,
   not the server library's. Proof it already works: the `vault`/`media` client
   body is a string in `manifest.json` executed by `fa-runtime.js` with zero Go in
   it — swap the server to Python and it renders unchanged.

## The root-vs-patch test (apply to every decision here)

> Is there exactly **one** place the semantics are defined?

- **One compiler/IR, N dumb backends** → root. `total = price * qty`, a drag
  gesture, an easing curve — defined once, executed identically by Tier-1 JS,
  WASM, SwiftUI, Compose, and every server backend.
- **N transpilers** (FDL→Go, FDL→Python separately) → a patch farm that drifts.

## Build sequence (bricks)

Each brick is independently shippable and tested. We do not build the whole
engine at once.

- **Brick 1 — `state:` in the language (parse + AST). ✅ done.** A facet declares
  typed, defaulted local state. Parser (`parseState`) + AST (`Facet.State`) +
  compile-time checks (`checkState`: initial value required, unique, no `what:`
  collision). Tests in `internal/parser/state_test.go`.
- **Brick 2 — client bindings in the manifest. ✅ done.** The compiler records the
  reactive graph in `manifest.json`: `state` (signals + initial values via
  `genState`) and `bindings` (per `{state}` interpolation, the signals that feed
  it, with stable ids `b0,b1,…` in document order via `genBindings`). State
  signals are now valid roots in `looks:` (`checkFieldRefs`). Tests in
  `internal/codegen/bindings_test.go`.
- **Brick 3 — client actions + mutation. ✅ done.** Named, reusable handlers:
  an `actions:` block defines event-agnostic "controller methods"
  (`name:` → indented `signal = expr` lines, AST `Facet.Actions`/`Action`/`Assign`,
  `parseActions`), and `on:<event>="name"` on an element wires a DOM event to one.
  Checked by `checkActions` (unique names; targets must be `state:` signals, never
  `what:` fields; expr roots in scope; every `on:` references a declared action)
  and recorded in the manifest as `actions` + `handlers` (stable ids `h0,h1,…`).
  Tests in `internal/{parser,codegen}/actions_test.go`, incl. a load-bearing check
  that `on:click="…"` survives `html/template` emission.
  **Decision: inline mutations are out — actions are always named** (one IR, the
  "controller method" model; an MVC-friendly separation of trigger from logic).
- **Brick 4 — the Tier-1 updater. ✅ done.** The reactive loop now *runs*, fully
  client-side, zero round-trip (`examples/counter.fct` is the wedge). Pieces:
  - **Marker injection** (`emitNodes`): a live binding is a *pure-signal*
    text interpolation (every root is a signal) — it is wrapped
    `<span data-fa-bind="bN">…</span>` with the signal's **initial value baked in**
    via `substituteSignals` (correct first paint, no flash, no client round-trip).
    `inTag` tracking keeps attribute-context interps out (text only for now).
    Binding ids are assigned in the *same* walk that records them in the manifest,
    so template markers and manifest ids cannot drift.
  - **Handler wiring** (`wireHandlerAttrs`): `on:<event>="x"` → inert
    `data-fa-on-<event>="x"`.
  - **Runtime** (`runtime/fa-runtime.js`): the client expression evaluator is
    extended to the full FDL grammar (arithmetic/boolean/parens, mirroring
    `expr.go`) — still a tiny tokenizer + precedence-climbing parser, **no
    eval/Function (CSP-safe)**. A new reactive module keeps a per-instance signal
    store, delegates one listener per event type, runs an action's assignments on
    click, and writes each bound node directly (`applyAction`/`bindingText` are the
    DOM-free, unit-tested core).
  - **Tests**: `internal/codegen/actions_test.go` (`TestCounterWedgeCompiles`),
    `runtime/reactive_test.js`, bridged under `go test` via
    `runtime/runtime_test.go` so CI runs the JS suites too.
  - **Note on the thesis**: the *graph* is compiled (manifest bindings); only the
    leaf expressions are evaluated by a fixed, bounded evaluator — not a per-app
    framework. The IR is unchanged if we later compile leaves to JS/WASM, so this
    stays "one IR, swappable executors," not a patch.
  - **Deferred to later bricks**: attribute/class bindings (to fill the heart on a
    like button), and mixed signal+`what:` expressions (need server values exposed
    client-side — Brick 5).
- **Brick 5 — derived on the client. ✅ done.** A computed `what:` field whose
  every root is a signal (or an earlier such field) is *client-derived*: the
  compiler records it (`clientDerived`/`genDerived` → manifest `derived`), bakes
  its initial value into the first paint (`emitComputed` runs `substituteSignals`),
  and a binding `{derived}` is live (`reactiveRoots`). The runtime recomputes
  derived values over the signal store in order (`computeScope`) before flushing.
  A computed field that touches a plain `what:` prop stays server-only.
  `examples/poll.fct`; tests in `internal/codegen/derived_test.go`.
- **Brick 6 — reactive lists, keyed reconciliation. ✅ done.** `for v in <signal>`
  over a list signal lifts to an empty `<fa-for>` host plus a client item template
  (`extractLists` → manifest `lists`); the evaluator gained array literals and
  list-concat `+` (so `items = items + [x]` appends). The runtime renders items
  with `fill` and reconciles the host's children **by key** (`item.id` else index):
  reuse-unchanged, move-into-order, create, remove (`reconcileList`/`listItems`).
  `examples/todo.fct`; tests in `internal/codegen/lists_test.go`.
- **Brick 7 — effects. ✅ done.** `effects:` runs `on <signals>: <action>` — when a
  dependency signal changes, the action runs. The imperative complement to a
  derived value (history, mirroring). Effects run **once per event cycle and never
  re-trigger one another** (`runEffects` diffs pre/post signals), so they cannot
  loop. AST `Facet.Effects`, `parseEffects`, `checkActions` (deps are signals,
  action exists), manifest `effects`. `examples/tracker.fct`.
  - **Tier-2 WASM is the named next frontier, not yet built.** The seam: a future
    `compute`/`effect` can target a WASM module compiled from FDL for canvas/WebGL,
    a rich-text buffer, physics, crypto. The manifest IR (signals/actions/effects)
    is the contract; swapping the leaf executor from the JS evaluator to compiled
    WASM changes no IR — "one IR, swappable executors."
- **Brick 8 — attribute / class / show bindings. ✅ done.** A reactive signal inside
  an attribute value now patches that attribute. Codegen detects a pure attribute
  binding (`class="{x}"`) at the tail of a text chunk, strips the raw `attr="`,
  emits a controlled attribute, and marks the element `data-fa-bind-attr="<ids>"`
  (attribute-context marker injection, the piece deferred from Brick 4). Two node
  kinds in the manifest: `attr` (setAttribute — class, href, aria-*, style) and
  `boolattr` (toggle by presence — `disabled`/`hidden`/`checked`/…), so
  `hidden="{!visible}"` is **show/hide** and `disabled="false"` (still-disabled) can
  never be emitted. First paint is baked when every root is a state signal, else
  emitted neutral and corrected by the runtime's hydrate pass (`applyAttrBindings`).
  `examples/like.fct`.
- **Brick 9 — forms / two-way binding. ✅ done.** `bind:value="signal"` (and
  `bind:checked` for checkboxes) ties a form control to a state signal both ways:
  the runtime reads the control on `input`/`change` into the signal (preserving a
  numeric signal's type) and writes the signal back to the control on flush
  (skipping a focused field so the caret never jumps). Lowered to inert
  `data-fa-bind-value`/`-checked` markers (`wireBindAttrs`), recorded as manifest
  `inputs`, validated by `checkBindValues` (target must be a mutable `state:`
  signal). `examples/greeter.fct`.
- **Brick 10 — routing. ✅ done.** `route` is a built-in reactive signal seeded from
  `location.pathname` and updated on every client navigation (`updateRoute` over the
  existing `data-nav` swap). A client router then falls out of Brick 8 —
  `<section hidden="{route != "/about"}">` — with no server route table. Matching
  `a[data-nav]` links get `.fa-active` + `aria-current`. The compiler reserves
  `route` as a built-in reactive root (`builtinSignals`); route-dependent bindings
  are non-bakeable (unknown at compile time) so they hydrate at boot.
  `examples/site.fct`.
- **Brick 11 — async / queries. ✅ done.** `query: name from "url"` exposes an async
  fetch to the reactive layer as a structured `{loading, error, data}` value. The
  runtime seeds `{loading:true}` on mount (`signalsFor`), fetches once per instance
  (`runQueries`), then writes the result and flushes — so loading/error/data are
  just Brick-8 show bindings and a Brick-4 text binding over `name.data.…`.
  Server-authoritative by transport (a normal same-origin endpoint). AST `Query`,
  `parseQueries`, `checkQueries`, manifest `queries`. `examples/forecast.fct`.
- **Later (separate layer, not v1):** reactive *structural* `{if}`/`{for}` over
  signals beyond the show-binding shape; offline / local-first / CRDT conflict
  resolution.

## Hard parts (engineering, named honestly)

- The dependency-graph compiler with **correct invalidation** is the core. The
  skeleton exists in `checkComputed`.
- **Keyed list reconciliation** is where every fine-grained framework bleeds.
- **Offline + server-authoritative** are in genuine tension; that's a deliberate
  later layer, not bolted into the reactive core.
