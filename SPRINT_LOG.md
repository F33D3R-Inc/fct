# Sprint Log

The running log of the build cycle. One entry per pass. The cycle:

**build the X-clone → hit a language wall → fix the Facet language → release on
GitHub → download → update the local machine → resume the X-clone.**

The X-clone (a Twitter/X clone, the forcing function for the language) currently
lives at `examples/layered/playground.fct`. Newest entries on top.

---

## Sprint 4 — 2026-06-20 — Tier 3 (part 2): post-content primitives `richtext` + `video` → released v1.11.0

**Shipped + RELEASED (the new cadence: every tested section ships immediately —
`always-release-each-section`):**
- **`video "{url}"`** — media player with controls; interpolated like `image`.
- **`richtext "{expr}"`** — renders a safe Markdown subset (headings, `- ` lists,
  ```` ``` ```` fenced code, inline code/`**bold**`/`*italic*`). Input is fully
  HTML-escaped first (XSS-safe), then a fixed tag set is emitted. The renderer is
  duplicated **byte-identically** in Go (`runtime/richtext.go`) and JS
  (`assets/facet.js` `markdownHtml`) so SSR first paint == client hydration.

**Surface:** ast (Video/Richtext), parser, ir, server.go (render + CSS), facet.js
(render + the mirrored md functions). Tests: `runtime/richtext_test.go` (8 cases
incl. XSS escaping + code-fence escaping), `internal/compile/content_test.go`.
go build/vet/test green, gofmt clean. **Released v1.10.0 then v1.11.0** (main +
tags pushed; Actions builds binaries).

**v1.10.0 was the catch-up release** bundling the previously-local Sprint 2+3 work
(after the user flagged "I don't see anything for 8 hours" — local-only work is
invisible to investors/devs). Cadence now locked.

**Next (Tier 3 remainder + beyond):** infinite-scroll list + search input, then
sum types + pattern matching, scoped generics, forms-with-state, query depth —
toward language "done", then the 250+ facet library phase.

---

## Sprint 3 — 2026-06-20 — Tier 3 (part 1): view primitives `icon` · `badge` · `tabs`

**Context shift:** the target is finishing the LANGUAGE so the real f33d3r.com can be
rebuilt (not the X-clone). New screenshots showed f33d3r.com is a rich creator-social
platform; the gap to it is concentrated in **UI primitives**. Also recorded: the old
project had a **250+ facet library** we still lack — that's a phase *after* the
primitives, built on them via the registry. See `facet-library-250` memory + ROADMAP.

**Shipped (code complete; build/test pending Bash availability):**
- **`icon "name"`** — a named glyph the page CSS/icon-font fills (`--fa-icon-bg` per
  `[data-fa-icon=…]`); for the icon nav.
- **`badge "{expr}"`** — a small interpolated pill for counts/status (unread,
  verified).
- **`tabs bind cell:` / `tab "Label" -> "value":`** — a segmented control whose
  selection is a `@client` state cell (validated client-only); switching is local
  (no round-trip). The tabbed feed (Following/Trending/New/NSFW). It is a reactive
  region: refreshes on the bound cell AND on entities read in a tab body.

**Surface touched:** ast (Icon/Badge/Tabs/Tab + node()), parser (3 line cases +
`parseTabs`), ir (Node.Value, lowering w/ region+deps), server.go (render +
`activeTab` + CSS), facet.js (render + `fillTabs` + region registration + refresh
routing + tab-click handler). Tests: `internal/compile/primitives_test.go`
(node presence, tab count, region deps on bind+entity, client-state + unknown-bind
errors).

**Note:** `avatar` not needed — `image` already renders rounded avatars. Next in
Tier 3: `richtext`/markdown posts and `video`.

---

## Sprint 2 — 2026-06-20 — Tier 1: filtered aggregates + exists (the social-data spine)

**Shipped (code complete, tested):** the query primitives a social graph needs.
- **Filtered aggregates** — `count(x in Entity where <cond>)`: a count scoped to a
  predicate with an item variable (per-tweet like/reply/repost counts).
- **`exists(x in Entity where <cond>)`** — membership / "have I liked this?" per
  viewer; and, used inside a `for … where exists(…)`, it expresses the **Following
  feed** ("tweets by people I follow").
- **Self-referential relations** confirmed working (`parent: Tweet?` → reply
  threads); many-to-many already expressible via a join entity, now *countable*.
- **Live list refresh fix** — a list region now also subscribes to entities read in
  its body, so a new like updates a per-row `count(...)` live.

**Surface touched:** ast (`Agg.Var/Where`, `exists` op), parser (`x in Coll where`
form + `exists`), ir (`Expr.Var/Where`, lower/check/freeNames/depsIR/hasImpure/
cloneExpr), runtime eval.go + assets/facet.js (filtered fold, item-var bind/restore),
SQL pushdown left untouched (view lists filter in memory; API params error-and-fall-
back on agg). Tests: `internal/compile/agg_test.go` (compile + IR shape + error
cases + live-dep), `runtime/eval_test.go` (filtered count/exists, no var leak).

**Verified:** gofmt clean · `go vet` clean · `go build` ok · `go test ./...` green ·
rebuilt `./facet` (1.9.0) and `facet build` on new syntax → correct IR.

**What this unlocks in X (now expressible in the lang):** the full engagement bar
(reply/repost/like counts + has-liked/has-reposted/bookmarked via `exists`), the
For-you vs Following feeds, reply threads, and per-user notification unread counts.
Remaining for the clone are mostly UI primitives (tabs, icons, avatars, badges,
infinite scroll) and platform breadth (search, video, DMs) — Tiers 3–4.

**Note:** deferred within this section — filtered `sum` and an `in` membership
operator (sugar; `exists` covers the feed case). Next: pick Tier 3 UI primitives
(tabs/badge/icon) or continue Tier 2 (notifications already expressible).

---

## Sprint 1 — 2026-06-20 — direction: considered general-purpose, decided NO

**What happened:** shelved the X-clone as the driver to work the language directly,
and floated making FA a **general-purpose language**. Talked it through and
**decided against it** — the goal is to build a real site (f33d3r.com), not a
language; the application-language FA already covers most of what a site needs, and
going general-purpose is a multi-year detour. FA **stays a compiler-first
application language.** No "bolting on" GP features.

**Outcome:** wrote `ROADMAP.md` (mapped the full app-language feature checklist
against shipped reality; map kept, GP pivot reverted). Memory:
`fa-stays-application-language` records the decision so we don't re-litigate.

**Build order (app-language, unchanged):** request→response service calls →
effects/capability system → scoped type-system additions (sum types, pattern
matching, scoped generics) → app-platform polish (constraints, form-state, tables,
DX diagnostics).

---

## Sprint 0 — 2026-06-20 — orientation & "why isn't the X-clone running?"

**Status:** the X-clone runs. The blocker was tooling, not the language.

**What we found**
- The "X-clone" is `examples/layered/playground.fct` — `playground X` with an
  auth/login screen and a member-only three-column feed (nav · feed · trends),
  Twitter dark theme (`#15202b` / `#1d9bf0`), built out of layered facets.
- It was *not* dockerized as assumed: `docker-compose.yml` mounts
  `examples/chirp.fct`, **not** the X-clone — and Docker isn't available on this
  machine at all.
- The real reason it "wasn't running": the committed `./facet` binary was the
  **v1.0-era** build (only knew `build|run`; rejected `playground`), while source
  is at **v1.9.0** — the local tool was 8 releases stale. This is the
  "update local machine" step of the cycle, skipped.

**What we did**
- Rebuilt the tool from source (`go build -o facet ./cmd/facet`) → `facet 1.9.0`.
- Ran the X-clone in-memory: `facet dev examples/layered/playground.fct`.
- Verified end-to-end via curl:
  - `GET /` as guest → 302 redirect to `/login` (member screen-guard working).
  - `GET /login` → renders the X sign-in surface ("𝕏 · F33D3R", "Sign in to X").
  - signup `ada` (first user = admin) → `{"reload":true}`.
  - `tweet "hello from the x-clone"` → posted; home feed renders
    "ada · hello from the x-clone"; `GET /api/Tweet` returns the row.
  - CSRF is enforced on `/event` via the `X-Facet-CSRF` header (browser's
    facet.js sends it automatically).

**Notes**
- To run the X-clone locally with no DB: `./facet dev examples/layered/playground.fct`.
- Open item carried over: LICENSE still missing from the repo (flagged earlier).

**Next wall (already identified):** request→response service calls. v1.9.0 shipped
`service`/`call` as **fire-and-forget** only ("request→response is next" — see the
v1.9.0 release notes and `wiki/Services.md`). The X-clone will need a `call` that
**binds a result back into the action** (e.g. a moderation/AI verdict) to do
anything conditional on an external brain. That is the next language fix.
