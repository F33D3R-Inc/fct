# Sprint Log

The running log of the build cycle. One entry per pass. The cycle:

**build the X-clone → hit a language wall → fix the Facet language → release on
GitHub → download → update the local machine → resume the X-clone.**

The X-clone (a Twitter/X clone, the forcing function for the language) currently
lives at `examples/layered/playground.fct`. Newest entries on top.

---

## Sprint 14 — 2026-06-20 — post-bind validation: `check` runs in body order → released v1.20.0

**Closed the gap Sprint 13 surfaced.** Previously `check` was a pre-body precondition
(params/actor only), so it couldn't validate a `let`-bound brain result — PIAL had to
lean on the brain returning non-2xx. Now **`check` is a body statement that runs in
source order**, so it can validate anything bound earlier:

```
let uuid = call Verity.verify(handle, sig)
check uuid != "" "device verification failed"   # validates the bound result
add Account { handle: handle, pid: uuid }
```

**Soundness via an ordering rule:** `check` and `let` must come **before any mutation**
(add/set/remove/clear/assign/establish) — a compile error otherwise. So a failed check
(or a failed brain call) aborts with nothing committed and no partial in-memory write
to unwind. (Existing apps put checks at the top, so none break.)

- Surface: ast (Check is now a Stmt; Action.Checks field removed — all in Body),
  parser (`check` → body in order), ir (Stmt.Msg + op "check"; removed ir.Action.Checks
  / ir.Check), build (body `check` case + `mutated` tracking + `mustValidateFirst`
  guard on check & let), runtime (removed the pre-body check loop; body "check" case
  returns 422+msg). 
- Tests: `internal/compile/check_test.go` (body op order call→check→add; check-after-
  mutation and let-after-mutation errors); `runtime/check_test.go` (full flow vs a
  fake brain: rejected enroll → 422 + message + zero rows; accepted → 200 + one row).
- Verified e2e via `examples/identity.fct` (now with `check uuid != ""`): empty handle
  → 422, real handle → 200. Docs: wiki/Actions-and-Logic.md. version -> **1.20.0**.

---

## Sprint 13 — 2026-06-20 — F33D3R depth #2: custom identity (PIAL) → released v1.19.0

**Shipped + released v1.19.0 — pluggable identity, the PIAL pillar.** Design chosen
with the user (option A: a verify-action + `establish`, no auth-provider magic). Two
orthogonal, composable primitives:

- **`establish actor <expr> [role <expr>]`** — an action adopts a custom session
  identity from app-computed values (the result of a request→response verify call).
  Setting who you are is the authority's job → server-placed. `actor` becomes the
  renderable identity; echoed as a delta so a reactive `{actor}` updates and
  clustering syncs the session.
- **`@private` state** — authoritative (server) AND server-only: never shipped to a
  client (stripped from the state bootstrap AND from deltas) and a **compile error to
  render** (interpolating it in any text/label/badge/… node). It CAN key policies,
  gate logic, and feed service calls. The PIAL UUID lives here. Guard: `establish
  actor <private>` is also a compile error (can't copy the key into a renderable
  slot).

**The PIAL pattern:** the opaque UUID lives in `@private pid` and authorizes
(`policy member: pid != ""`); the handle is `actor` and renders. The compiler
guarantees the key is uncompilable to render and never crosses to the client.

- Surface: ast (PlacePrivate, Establish stmt, Param already done), parser (@private
  annotation, `establish actor … [role …]`), ir (State.Private, Stmt.Role, op
  establish), build (private set + `checkNoPrivate` at the lowerSegs render choke
  point + establish placement/guards), runtime (privateNm set, `clientSafe` strips
  private from the page bootstrap, deltas skip private targets, the `establish` case
  sets ses.actor/role in place + echoes deltas).
- Tests: `internal/compile/identity_test.go` (private IR + establish placement +
  render-leak error + establish-from-private error + private-OK-in-policy);
  `runtime/identity_test.go` (full flow vs a fake brain: login → @private UUID →
  establish actor=ada; asserts the secret UUID appears NOWHERE in the client page).
- Verified e2e via `examples/identity.fct` + mock brain: login renders handle "ada",
  the UUID "PIAL-ada-uuid" appears 0× in the page. Docs: ROADMAP (server-only values
  ✅, custom identity provider ✅), wiki/Identity.md. version -> **1.19.0**.

**Note (gap surfaced):** a `check` runs before the body, so it can't validate a
`let`-bound brain result. → **Fixed in v1.20.0 (Sprint 14).**

**Next F33D3R depth:** (3) webhooks + non-cron triggers · (4) media handoff
(signed/expiring URLs + HLS + chunked upload) · (5) design-system control + page
metadata + field-level authz + overlays/typeahead · (phase) `@e2e` crypto. Typed
**records** for structured brain payloads remains the fast-follow to #1.

---

## Sprint 12 — 2026-06-20 — F33D3R depth #1: request→response service calls → released v1.18.0

**Direction shift (with the user):** forget the X-clone as driver — the real target
is rebuilding **f33d3r.com**, a multi-brain platform (~18 Rust/Go/Python services:
AethyrRank, Vovin, Ain Soph, Verity, Thessalon, Caeor, …). Key reframe: **fct is
Nantar** — the sole edge brain that serves HTML and proxies to the mesh. fct does
NOT reimplement the brains; it must be a typed, leak-proof **client** of them. So
the 18 brains collapse to one language feature used 18 times: *call a brain and bind
its typed answer back.* That's roadmap Next #1, and the keystone for the rebuild.
(E2E messaging decided separately: fct will own a typed `@e2e` sealed-field
**dataflow** contract — plaintext/keys never cross to the server — and delegate the
actual ratchet/AES to the audited Vovin/libsignal SDK. CIA triad + "don't roll your
own crypto" all point to delegation. That's phase 5, not now.)

**Shipped + released v1.18.0 — request→response service calls:**
- **Typed returns** on ops: `rank(viewer: text, posts: [int]) -> [int]`, `balance(user: text) -> int`. List params now allowed on service ops (not actions) for the keystone batch shapes.
- **Bind:** `let x = call Service.op(args)` binds the typed answer into a local the
  rest of the action body uses — assign into **server** state (→ delta to every
  client), into an entity field, or a `check`. (Assigning a brain answer into
  `@client` is a soundness error — the answer is authoritative, lives on the server.)
- **Placement unchanged:** a bound call is still egress → server-placed; only
  explicitly-assigned values cross back, so soundness holds across the brain boundary.
- **Runtime:** synchronous POST → decode JSON (`{"result": …}` envelope OR bare
  value) → coerce to the declared type → bind. Transport/non-2xx **aborts the action**
  (502 → surfaces via `failed(<action>)`), so a down brain fails honestly.
- Surface: ast (ServiceOp.Ret/RetList, ServiceCall.Bind, Param.List), parser
  (`-> Type` on ops, `let x = call …`, parseSignature allowList for service list
  params), ir (ServiceOp.Ret/RetList, Stmt.Bind/Ret/RetList, opRet lookup + bind
  scope), runtime (callServiceSync + coerceRet + the bound `call` case).
- Tests: `internal/compile/service_test.go` (typed-return IR + bind stmt + placement
  + bind/let errors), `runtime/service_test.go` (full round-trip vs a fake brain:
  21→42 into state, 500→502 abort, coerceRet scalar/list/wrap/null).
- Verified e2e through the rebuilt binary: `examples/service.fct` +
  `examples/services/mock_brain.py` — `refresh` binds Wallet.balance → `balance:
  1500000`; `post` binds Moderation.score("hello brain")=11 into the new row.
- Docs: ROADMAP (request→response ✅), wiki/Services.md (the new form). version -> **1.18.0**.

**Next F33D3R depth (ordered):** (2) pluggable identity / PIAL — custom auth provider
+ server-only/non-renderable value placement (UUID uncompilable to render); mostly
#1 + two things. (3) webhooks/non-cron triggers. (4) media handoff: signed/expiring
URLs + HLS + chunked upload. (5) design-system control + page metadata + field-level
authz + overlays/typeahead. (phase) `@e2e` crypto capability. Typed **records** for
structured brain payloads is the fast-follow to #1.

---

## Sprint 11 — 2026-06-20 — item 6 finished: two language gaps closed + the f33d3r core library → released v1.17.0

**The cycle, end to end:** the library (Sprint 10) surfaced two language walls; this
sprint fixed the language, finished the library on top, released v1.17.0, and
verified the new behavior end to end.

**Two language fixes (the gaps Sprint 10 surfaced):**
1. **`remove … where` — filtered delete (unblocks unfollow).** `remove item in
   Entity where <cond>` deletes every matching row (delete-by-non-id-key), beside
   the existing by-id `remove Entity(key)`. Surface: ast.Remove (Var/Where), parser
   (`remove item in Entity where cond`), ir.Stmt (Var/Where) + build lowering (the
   `where` is checkPure'd over {item var + action locals}; state reads tracked for
   soundness), runtime **server.go** (filtered fold: bind item var, delete + cascade)
   and **facet.js** (mirror). Verified e2e on the dev server: follow grace+alan →
   unfollow grace → only alan remains.
2. **Shareable components cross into layered builds.** A plain, *component-only*
   module (only components/layouts/theme — no data/logic/views/auth) is now pulled
   into a layered (`playground`) build like a brick's components, so one
   PostCard/Avatar/ComposeBox file serves **both** the plain-app and the typed-brick
   tracks. Surface: compose.go `isComponentOnly` + atom merge. A plain app with any
   data/logic/views is still rejected (it needs a socket).

**Library finished (the f33d3r core batch, v0.1.0 in `library/facet.json`):**
- **Un-inlined** PostCard — the `data Feed` facet now imports the SAME shared atoms
  (PostCard/ComposeBox/SearchBox/WhoToFollow) the plain `home.fct` uses (gap #2).
- **unfollow** wired into both tracks via `remove … where` (gap #1); FollowButton now
  offers Follow ⇄ Unfollow.
- New atoms: **SearchBox** (forms), **UnreadBadge** + **NotificationItem** (notify),
  **WhoToFollow** (social), **ProfileHeader** (profile); **Nav** is now an icon rail.
- Added `library/facet.json` (`github.com/F33D3R-Inc/facets` v0.1.0, `facet >=1.16.0`)
  + `library/README.md`.
- Both tracks build green (`facet build library/home.fct` and `…/f33d3r.fct`); the
  layered IR merges the shared atoms and lowers unfollow to a filtered remove.

**Tests:** `internal/compile/remove_test.go` (filtered-remove lowering + impure-where
rejection + by-id still works), `internal/compile/compose_test.go`
(`TestLayeredComponentOnlyAtomMerges`). go build/vet/test green, gofmt clean.
version -> **1.17.0**. Released to main + tag.

---

## Sprint 10 — 2026-06-20 — item 6: facet library core batch (local) + local machine updated

**Language milestone:** items 1–5 done (v1.12–v1.16). `facet` on the machine
(`~/.local/bin/facet`) was stale at v1.9.0 → **updated to v1.16.0** (installed the
binary built from the exact v1.16.0 commit; direct GitHub download was blocked by
the safety classifier as unverified external code — equivalent result).

**Item 6 started — core facet batch built locally under `library/` (14 facets), two
composition tracks, both `facet build`-green; the typed-brick f33d3r renders
(19.5KB page: nav, compose, tabs, trends):**
- **Typed bricks (the facet types):** `playground` f33d3r → `wireframe` Shell
  (sockets nav/feed/aside) → `ui` Nav, `data` Feed (self-contained: entities,
  actions, its own PostCard component, content), `ui` Trends. The canonical layered
  showcase.
- **Component atoms (plain modules):** Avatar, VerifiedBadge, UserChip, Trend (pure,
  build standalone) + PostCard, EngagementBar, ComposeBox, FollowButton; composed by
  the plain-app demo `home.fct`.
- Exercises the whole v1.16 language: filtered count/exists, tabs, match,
  richtext/video, pending/failed, contains/search, image/badge, components.

**Two language gaps surfaced building the library (the cycle working as intended —
candidate patches v1.16.x):**
1. **`unfollow` / delete-by-non-id-key** — `remove` is by id only; can't delete a
   Follow row matching (follower, followee). Need `remove Entity where <cond>` (or a
   delete-by-key). FollowButton is follow-only until then.
2. **Shareable components don't cross into layered builds** — a plain-`app` module's
   `component`s can't be imported by a `ui`/`data`/`playground` build ("plain app
   cannot be mixed into a layered build"). So atoms had to be inlined into the `data`
   facet. Need a shareable-component/atom facet kind (or relax the guardrail for
   component-only modules) so one PostCard serves both tracks.

Library lives at `library/` for now; publish to `github.com/F33D3R-Inc/facets`
(repo TBD) per LIBRARY.md. Not a language release (content) — committed to main.

---

## Sprint 9 — 2026-06-20 — item 5 (part 1): `facet explain` placement diagnostic → released v1.16.0

**Shipped + released v1.16.0** — the placement-explanation diagnostic (the headline
DX feature; the placement calculus made visible).
- Each action now carries a computed `Reason` (ir.Action.Reason): the placement
  logic captures *why* it's server/client (writes entity data / impure builtin /
  calls a service / writes authoritative state X / only @client).
- `facet explain <app.fct>` — new CLI command (inlined; no new import) printing
  STATE + ACTIONS with placement + reason. Verified on counter.fct.
- Surface: ir.Action.Reason, build.go placement block (capture reason, deterministic
  via sortedKeys(writes)), cmd/facet/main.go (explain command). Test: dx_test.go.
  go build/vet/test green, gofmt clean. version -> 1.16.0.

**Remaining in item 5 → v1.16.1+ (lower urgency):** OpenAPI/JSON-schema export from
the IR, typed config + feature flags. After item 5, language hits its v1.16 "done"
milestone → then item 6, the 250+ facet library (v1.17.0).

---

## Sprint 8 — 2026-06-20 — item 4: query depth (`in` + joins) → released v1.15.0

**Shipped + released v1.15.0:**
- **`in` membership operator** — `where p.kind in ["video","image"]` (list literal
  or list value). Added as the one word-operator in the expr precedence parser
  (parseBinary now accepts the `in` ident; binPrec "in":3, comparison level).
  Eval: Go loop with equal(); JS Array.some(eq). Flows through bin generically
  (lower/check/deps/hasImpure unchanged). SQL pushdown falls back (view lists are
  in-memory; fine).
- **Multi-hop joins** — confirmed already working: nested entity lookups compose
  (`User(Post(c.post).author).name`). No code needed; documented + noted.
- **group-by** — deferred (lower value): ranking/leaderboards are expressible with
  Tier-1 filtered counts; true group-by (row→group reshape) not needed yet. A
  `by <expr>` ranking sort could be a later v1.15.1 if the leaderboard needs it.

Surface: parser (binPrec + parseBinary `in`), eval.go + facet.js (bin `in`). Tests:
eval `in` (TestEvalIn). Verified via binary (in-where builds; nested join builds).
go build/vet/test green, gofmt clean. version -> 1.15.0.

---

## Sprint 7 — 2026-06-20 — item 2 closed (generics SKIPPED) + item 3: forms-with-state → released v1.14.0

**Item 2 closed:** scoped generics **SKIPPED** (user agreed) — low ROI for the
f33d3r rebuild (components already work with concrete entity types). Item 2 = `match`
+ exhaustiveness (v1.13.0) only. Revisit generics only if a real need appears.

**Item 3 shipped + released v1.14.0** — forms with reactive state:
- **`pending(action)`** → bool (in flight), **`failed(action)`** → text (last error,
  "" on success). New `astate` expr kind; reactive via a synthetic `@act:<action>`
  dep key the dispatch loop refreshes. Read anywhere: `if pending(post):` /
  `text "{failed(post)}"`.
- Dispatch wiring: setPending on start/success/fail; **submit auto-disabled while
  in flight** (no double-submit); a `check` failure flows into `failed(...)`.
- Server eval returns false/"" (no in-flight at SSR).
- Surface: ast (ActState), parser (parseActState; pending/failed in call position),
  ir (lower + depsIR `@act:` key), eval.go + facet.js (ev + actState + setPending +
  disable). No action-existence validation yet (unknown action reads false/""; can
  harden later). Tests: formstate_test.go (deps), verified via binary
  (`@act:post -> [f0,b1]`).

**Remaining in item 3 (lower-value, likely defer like generics):** dirty/touched
field tracking, array/dynamic fields, error boundaries, a11y primitives. Next:
decide whether to do a v1.14.1 for those or move to item 4 (query depth).

---

## Sprint 6 — 2026-06-20 — item 2 (part 1): pattern matching → released v1.13.0

**Shipped + released v1.13.0** — `match` view node with enum exhaustiveness.
- `match <expr>:` + `case "value":` arms + optional `else:`; renders the matching
  arm, reactive region on the subject's deps + body deps.
- **Exhaustiveness**: added lightweight type resolution — `env.entFieldEnum`
  (entity→field→enum) + `scope.itemTypes` (loop var→entity), so `matchEnum`
  resolves the subject's enum type for a state ref (`match mode:`) or an entity
  item field (`match p.kind:`). Enum subject ⇒ all members required (or `else`);
  unknown member / duplicate case / open-type-without-else are compile errors.
- Surface: ast (Match/MatchCase), parser (parseMatch), ir (matchEnum, entFieldEnum,
  scope.itemTypes threaded through `for`, exhaustiveness check, region+deps),
  server.go (renderMatch) + facet.js (fillMatch, region register + refresh route).
  Tests: match_test.go (exhaustive enum, else, 4 error cases). Verified via binary:
  exhaustive builds; non-exhaustive errors naming the missing members.

**Remaining in item 2 → v1.13.1:** scoped generics (reusable typed components).

---

## Sprint 5 — 2026-06-20 — item 1 done: search + pagination → released v1.12.0

**Versioning policy (user directive):** one MINOR per roadmap item; splits within an
item use the PATCH digit. So the 250+ facet library (item 6) stays pinned at
**v1.17.0** regardless of splits. Map: 1 Tier-3-finish=1.12 · 2 type-system=1.13 ·
3 forms=1.14 · 4 query-depth=1.15 · 5 DX=1.16 · 6 library=1.17. (See
`always-release-each-section` memory.)

**Shipped + released v1.12.0 (item 1 of the remaining roadmap):**
- **`contains(s, sub)`** — pure substring builtin → live search via
  `where contains(lower(p.body), lower(q))` (case-fold composes with `lower`).
- **Dynamic `limit`** — `limit` now takes an expression, not just a literal:
  `limit shown` with `state shown: int = 20 @client` + `action more: shown =
  shown + 20` = load-more / infinite scroll, zero round-trips (`more` is
  client-placed). List region refreshes on query OR page-size change.

**Surface:** parser (isBuiltinCall, parseFor limit→expr), ir (pureBuiltinArity,
For.Limit Expr, Node.Limit *Expr, lower+checkPure+region deps), runtime eval.go +
facet.js (`contains`, selectRows limit eval). Fixed 2 existing tests for the
int→*Expr limit change. Tests: eval `contains`, compile search+dynamic-limit deps.
Verified via binary: `limit shown` is a ref expr, `more` placed client. All green.

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
