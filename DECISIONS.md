# Architecture Decision Record

Newest first. Each entry: the call, the reasoning, and what would reverse it.

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
