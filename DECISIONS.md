# Architecture Decision Record

Newest first. Each entry: the call, the reasoning, and what would reverse it.

---

## ADR-0009 — Client render bodies gain `{if}`/`{for}` (round 2)

**Decision.** Vault `decrypt:` and media `source:` bodies now support
`{if expr}…{else}…{end}` and `{for v in path}…{end}` over the client values
(decrypted plaintext / media metadata), lifting ADR-0006 #5's "field
interpolation only." The expression subset is intentionally small: dotted paths,
literals (number/"string"/true/false), a leading `!`, and binary comparisons
(`== != < <= > >=`); `&&`/`||`/grouping are not in this round (nest `{if}`).
Truthiness mirrors Go templates (nil/false/0/""/empty list/empty map are falsy)
so a client body behaves like a server `looks:` body. **Interpolated values stay
HTML-escaped** — the vault guarantee (a compromised server / hostile plaintext
cannot inject markup) is unchanged; only literal template text is trusted. The
compiler/manifest already carried the control forms; this is a runtime change in
all three clients (web `fa-runtime.js`, FacetKit Swift, Compose Kotlin), with the
web runtime as the node-tested reference the native ports mirror.

**Reverses if.** Bodies need richer expressions (`&&`/`||`, calls) — then the
client gains a real expression evaluator shared with the server's grammar, rather
than this hand-rolled subset.

---

## ADR-0008 — `style:` is cross-platform design tokens, not scoped CSS

**Decision.** The `style:` block is a small, **token-based** vocabulary
(`direction`/`gap`/`pad`/`align`/`justify`/`grow`/`width`/`height`/`bg`/`fg`/
`radius`/`font-size`/`font-weight`) the compiler resolves at build time into
concrete inline style on the facet's root element. Because the native neutral
tree (`ParseView` → `resolveStyle`) already reads each node's inline style, the
same resolved style lands on web, iOS and Android with no per-platform code.
Color tokens resolve to concrete hex (not CSS vars) so all three renderers show
the identical color. Every property and token is validated — a typo is a compile
error. We explicitly **rejected** scoped/arbitrary CSS (the Vue/Svelte
`<style scoped>` model).

**Why.** FA's one defining invariant is "one server drives web + iOS + Android
through a neutral tree." Arbitrary CSS is web-only; a facet that used it would
silently become web-only or render differently on native — bolting a web feature
onto a cross-platform framework (the runtime-CSS-in-JS mistake one layer down).
Tokens keep appearance on the cross-platform contract. This also matches where
the serious frontends landed — compile-time, atomic, token-based (Meta's StyleX;
ByteDance's Lynx constraining CSS to a native-mappable subset) — rather than
runtime CSS-in-JS. A bonus: in a node-attached neutral model there is no global
selector namespace, so "scoping" is moot — nothing can collide.

**Reverses if.** Real apps prove they need web-only effects (`:hover`, `@media`,
keyframes). Then we add a **separate, explicitly web-scoped** block (e.g.
`style web:`, scoped Vue-style), documented as not crossing to native — the path
of least resistance stays cross-platform, the web escape hatch is opt-in. The
keyword namespace is reserved for this now; build it on demand (YAGNI). Theming
(tokens → CSS vars on web while native keeps hex) would need the manifest-based
style channel instead of inline resolution.

---

## ADR-0007 — Computed `what:` fields; integer arithmetic stays integer

**Decision.** `what:` accepts computed fields written with `=`
(`total = price * qty`, optional explicit type `free: bool = total >= 100`).
They lower to template variables (`{{$total := ...}}`) defined once at the top
of the facet template, in declaration order, and resolve server-side at render
time. They are excluded from the generated `<Name>Data` struct (the caller never
supplies them) and from facet-id derivation. A computed field may reference any
input field and any computed field declared *above* it; forward/self references
are compile errors. Separately, the arithmetic helpers `add`/`sub`/`mul` now
return an integer when both operands are integral (previously always
`float64`); `/` still yields a float.

**Why.** Computed fields were the remaining typed-data gap (deferred under the
stale ADR-0003 "single primitive" freeze, now lifted). Lowering to template
vars keeps them server-resolved with zero runtime/manifest changes and no
client logic — consistent with the architecture. The integer-arithmetic change
is required for them to be useful: a `float64` result compared to an integer
literal (`total >= 100`) is a Go-template type error, and integer-in/integer-out
is what authors expect; display output for whole numbers is unchanged.

**Reverses if.** We add real type inference (making explicit types fully
optional everywhere) or a numeric tower that unifies int/float comparison in the
expression layer, at which point the int-preserving special-case can fold into
it. Computed fields inside block-form slot/fill content remain unresolved (the
aux template lacks the parent's `$vars`); revisit if that pattern is needed.

---

## ADR-0006 — Primitive runtime semantics, round 1: the specific calls

**Decision.** The per-kind behaviors shipped in v0.12.0 pin down five
underspecified corners:

1. **Stream/pipe `throttle:` is trailing-edge coalescing at the emitting
   instance** (per scope+target+facet-instance): first frame immediate, frames
   inside the interval replace each other, the latest flushes when it elapses.
   Intermediates are dropped, the final state is always delivered. It runs
   before the broker, so multi-instance deployments throttle where the events
   originate.
2. **A bare feed `order:` field sorts descending.** A feed is a *ranked* list —
   best/newest first is the overwhelming default (`score`, `created_at`);
   `asc` is explicit. Fails closed on a missing/non-comparable field.
3. **Lifecycle transitions are forward-by-one** through the declared `states:`
   list. Branches (cancel, refund) are app logic on top of `Valid` — the
   declaration stays a simple ordered list instead of growing a transition
   grammar prematurely.
4. **Signal `ttl:` is enforced client-side from the manifest**, not carried on
   the wire. The client already fetches the manifest; adding a TTL field to the
   event would either be unsigned (spoofable) or change the HMAC layout for one
   kind. The relay stores nothing on the server either way.
5. **Vault envelopes are AES-GCM, base64(12-byte IV ‖ ciphertext)**, key
   registered in the browser via `fa.vault.key(name, hexKey)` and never sent.
   Round 1 client bodies support field interpolation only (no `if`/`for`) —
   decrypted values are always HTML-escaped, so plaintext can't inject markup.

**Reverses if.** Apps need leading-edge or rate-N throttling (becomes a
`throttle:` mode), feeds need multi-key ordering (becomes a list), lifecycles
need declared branch transitions (becomes a grammar), or vault needs algorithm
agility (the envelope gains a version byte).

---

## ADR-0005 — Unified block keywords `who/what/looks/when`; subscription folded into `when`

**Decision.** Adopt one set of block keywords across all primitives:
`who` (auth), `what` (data), `looks` (template), `when <event>` (handler). This
renames the earlier `data`→`what`, `render`→`looks`, and collapses `subscribe` +
`update on <event>` into a single `when <event>`. There is **no standalone
`subscribe` block** and no author-specified channel strings; a `when` handler
implies its subscription.

**Why.** The newest design consistently uses `who/what/looks/when` across three
primitives (`vault`/`media`/`stream`) and drops `subscribe` entirely. One coherent
keyword set beats per-primitive variation, and aligning now — while only `facet`
exists and the codebase is tiny — is far cheaper than later. Old names are
rejected with errors that teach the migration.

**The underspecified corner (flagged).** The new examples show no subscription
mechanism at all, so we dropped explicit channel strings (e.g.
`"post.{post.id}.stats"`). The v0 demo routes purely by `data-facet-id` (server
emits to the session and the runtime swaps by id), so nothing is lost yet. If we
later need per-channel SSE subscription/authorization (`channel_auth`), explicit
channels return — likely derived from the `when` event name or declared in `who`.

**Reverses if.** The user wants author-specified subscription channels back, or
the taxonomy shifts again (it has three times — hence ADR-0003 keeps us on one
primitive until the language settles).

---

## ADR-0004 — FDL comments are full-line only (v0)

**Decision.** A `#` begins a comment **only** when it is the first non-whitespace
character on a line. There are no trailing/inline comments. `#| ... |#` spans
multiple full lines.

**Why.** `render:` and `style:` blocks are full of legitimate `#`: `href="#"`,
`fill="#1d9bf0"`, `background:#000`. Treating `#` as an inline comment anywhere
would silently corrupt user HTML and CSS. Full-line-only sidesteps it entirely
and matches every example in the spec.

**Reverses if.** We introduce an explicit inline-comment token (e.g. `;;`) that
can't collide with HTML/CSS. Not planned.

---

## ADR-0003 — Freeze v0 to a single primitive: `facet`

**Decision.** v0 implements only the standard `facet` (with `data`, `render`,
`subscribe`, `update`). `intent`, `stream`, `presence`, `form`, `media`, `edge`,
`vault`, etc. are **designed on paper, implemented never** until `facet` is proven
end to end.

**Why.** Three conflicting taxonomies are currently live in our own docs (the v2
spec list, a later `feed/signal/lifecycle/pipe/vault` list, and the repo's
visual-hierarchy doc). You cannot write a compiler against a grammar that mutates
weekly. Proving one primitive end to end de-risks the entire pipeline; the rest is
repetition once the spine works.

**Reverses if.** The `facet` pipeline (`.fct` → template + manifest → live DOM
swap) is green and we freeze the full primitive set in a spec RFC.

---

## ADR-0002 — Hand-written lexer + recursive-descent parser

**Decision.** No parser generator. Hand-written offside-rule lexer
(INDENT/DEDENT/NEWLINE) feeding a recursive-descent parser.

**Why.** FDL is indentation-significant (Python-style, 4 spaces). Hand-rolling
gives precise positions and error messages — the project's stated bar is "DX that
exceeds React's." Generators fight the offside rule and produce worse errors.

**Reverses if.** The grammar stabilizes and hand-maintenance becomes the
bottleneck. Unlikely for a small language.

---

## ADR-0001 — Compiler hosted in Go, not Rust

**Decision.** The `fct` compiler and CLI are written in Go. (The public spec said
Rust.)

**Why.**
1. Our only backend target today is Go. A Go-hosted compiler can load the user's
   real Go packages with `go/packages`/`go/types` and **actually verify** that
   FDL types and server-function signatures match the backend (`fct.toml [types]`
   and `[functions]`). A Rust host would be reduced to string-parsing Go.
2. Single static binary distribution (`go install`), same as Rust would give.
3. Fast compile/iteration while the language design is still molten.
4. One language across compiler, generated code, and reference app — less context
   switching for the team.

**Reverses if.** We commit to many backend targets (Rust, Node, Swift…) and want
one portable compiler core; at that point a language-agnostic host (Rust) with
per-target plugins may win. Not now — YAGNI.
