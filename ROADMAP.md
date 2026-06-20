# Facet Roadmap

What Facet is, what it can express today, and what it still needs to express
**an entire modern application with no escape hatches**.

## The thesis

Facet is a **compiler-first application language** — built specifically for making
apps and sites, not a general-purpose language. One typed app graph, no
frontend/backend split in the source, placement (server · client · edge) computed
by the compiler. The language is organized around **five declaration families**,
and the **placement calculus** decides how each materializes:

1. **data** — entities, storage, relationships, queries, migrations
2. **state** — local, shared, derived, persisted, replicated
3. **actions** — mutations, commands, workflows
4. **views** — pages, components, forms, routes, the UI projection
5. **effects & policies** — auth, permissions, secrets, IO, jobs, integrations

Everything below is sorted into those families plus the cross-cutting platform.
The rule that keeps Facet coherent (and stops it becoming "Rust + React + Prisma +
auth + jobs smashed together"): **a feature is a new declaration kind or runtime
service through the same IR + placement model — never a new architecture.**

## How to read this

Status is honest, not aspirational:

- ✅ **shipped** — in the language today (v1.0–v1.9), verified in code/docs
- 🟡 **partial** — exists but narrow; a real app hits its edges
- ⬜ **missing** — not expressible yet
- 🚫 **out of scope** — deliberately excluded (rationale given); this is a feature

Rings: **Now** (shipped) · **Next** (v2 — what real apps hit first) · **Later**
(v3+) · **Out of scope**.

---

## Where Facet actually is (the surprising part)

Facet is far along on the **platform** axis and thin on the **language-core**
axis. It already ships most of what a brainstorm calls "the boring dealbreakers":

> auth · RBAC + row-level policies · sessions/CSRF/MFA/OIDC · entities + relations +
> FK cascade · migrations · transactions · indexed query pushdown + keyset
> pagination · live updates (SSE) · durable cron jobs w/ retry/dead-letter · file
> uploads · optimistic mutations · audit log · multi-tenancy · auto-admin · billing
> ledger · i18n/GDPR/retention · `@secret` encryption · mobile SDK generation ·
> remote facet registry.

The gaps that actually block "express any app" cluster in five places:
**(A) type-system depth**, **(B) a real effects/capability system**,
**(C) UI/forms richness**, **(D) data-query depth**, **(E) offline/local-first**.
The roadmap below is organized to attack those.

---

## Building X: the gap (the X-clone vs x.com)

The X-clone (`examples/layered/`) **is** the proving ground. The cycle:
**build the clone → x.com does something the clone can't → that's a missing
language feature → fix the lang → release → download & update the local machine →
continue the clone.** The same two data shapes (a social graph + correlated
per-viewer queries) build X, Facebook, and YouTube — so closing this gap is general.

Side-by-side, today's clone is a functional feed of bordered boxes; x.com has
avatars, For-you/Following tabs, an engagement bar with icon counts, media, search,
notifications. Translated into **language** features (visual polish is secondary):

**Tier 1 — the social-data spine (✅ DONE — Sprint 2, 2026-06-20)**
1. ✅ **Many-to-many & self-referential relations** — self-ref (`parent: Tweet?`) works (reply threads); M2M via a join entity, now countable.
2. ✅ **`exists` correlated subqueries** — "tweets by people I **follow**" (Following feed) and "**have I** liked this?" per viewer. (`in` sugar still pending.)
3. ✅ **Per-row / filtered aggregates** — `count(x in E where …)` for like/reply/repost counts. (Filtered `sum` deferred.)

**Tier 2 — dynamic behavior**
4. **Feeds** (For you / Following) — falls out of #2.
5. **Notifications** — an action that **fans out rows to *other* users** + a per-user unread **badge**.
6. **Ranking** — sort a feed by a computed engagement score.

**Tier 3 — UI primitives so it looks 2026 (in progress)**
7. ✅ `icon`, `badge`, `tabs`/segmented control (Sprint 3) · `avatar` = `image` ✅ · ⬜ `richtext`/markdown, `video`, infinite-scroll, search input + a real styling story.

**Tier 4 — platform breadth (FB / YouTube)**
8. **Video** primitive + multi-image + upload→attach.
9. **Full-text search** (Explore).
10. **DMs / chat** (realtime per-pair threads — SSE + auth already exist).

### Phase after primitives — the Facet Library (250+)

The old project shipped a **library of 250+ reusable facets** (UI + data + logic
bricks) that f33d3r.com was assembled from. FA today has the *mechanism* — the
registry (`import "github.com/…"`), layered facets, `component`/`layout` — but
**none of the library content**. So "done" is two ordered workstreams: **(1) the
language primitives** (this roadmap), then **(2) (re)build the facet library** on
top of them, published via the registry, and import it to rebuild f33d3r.com. Do
not build library content before the primitives can express it.

**Through-line:** Tier 1 is the keystone. Many-to-many relations + correlated
subqueries + per-row aggregates make a social graph expressible at all; Tiers 2–4
sit on top. **Tier 1 ✅ done (Sprint 2). Next: Tier 3 UI primitives** (tabs/icon/
avatar/badge) — Tier 2 (notifications, feeds) is already expressible on Tier 1.

---

## 1 · Language core

| Capability | Status | Notes |
|---|---|---|
| Primitive types `int/text/bool/money/date` | ✅ | typed columns + same semantics server/client |
| Enums (closed text types) | ✅ | flow through fields/state/params/`select` |
| Optionals `T?`, lists `[T]` | ✅ | |
| Relations (entity-typed fields) | ✅ | FK + cascade |
| Records / structs (beyond `entity`) | ⬜ | no value objects / inline record types |
| Tagged unions / sum types | ⬜ | **Next** — the missing piece for state machines & typed errors |
| Pattern matching / exhaustiveness | ⬜ | **Next** — pairs with sum types |
| Generics / parametric polymorphism | 🟡→⬜ | **Next, scoped**: generic *components* & *collections* only |
| Type inference | 🟡 | placement is inferred; value types are mostly annotated |
| Branded IDs / newtypes | ⬜ | Later |
| First-class functions / closures | 🚫 | a general-purpose-language feature; fights the declaration-first model |
| `async`/await | 🚫 | **subsumed by placement** — the compiler decides round-trips; there is no async to write |
| Macros / metaprogramming | 🚫 | the IR + facets are the extension mechanism |

**Call:** add *just enough* core — **sum types + pattern matching + scoped
generics** — to express variants, state machines, and typed errors. Stop there.
Full first-class-function/closure/async machinery is explicitly **not** the goal;
it would turn Facet into a general-purpose language and break the "compiler decides
placement" contract. (We considered going general-purpose on 2026-06-20 and
deliberately decided against it — the goal is to build a site, not a language.)

## 2 · Placement & authority

| Capability | Status | Notes |
|---|---|---|
| server / client placement + inference | ✅ | the spine |
| authoritative vs ephemeral (`@client`) | ✅ | |
| impurity ⇒ server, `requires` ⇒ server | ✅ | |
| soundness: server action can't read/write `@client` | ✅ | symmetric, compile-enforced |
| secret / server-only values | 🟡 | `@secret` (encryption) + credentials never shipped; no general `@secret`-typed *values* with leak diagnostics |
| edge placement | ⬜ | **Later** — third placement target |
| replicated / offline-capable / latency-sensitive annotations | ⬜ | **Later** — feeds local-first (§15) |
| materialization policies (cacheable/prefetch/subscribable/resumable) | 🟡 | live subscribe ✅, API micro-cache ✅; not declarable per-value |
| **placement *explanation* diagnostic** ("why does this run on the server?") | ⬜ | **Next** — high-leverage DX; the compiler knows, it just can't tell you |

## 3 · State

| Capability | Status | Notes |
|---|---|---|
| server / client / session state | ✅ | |
| derived / computed (reactive, inlined) | ✅ | `derive`, recomputed on input change |
| aggregates `count`/`sum` | ✅ | |
| defaults | ✅ | |
| reducers / state machines / typed actions-on-state | ⬜ | **Next** — rides sum types + pattern matching |
| undo/redo, transactional/batched UI updates | ⬜ | Later |
| persistence classes (localStorage/IndexedDB/TTL) | ⬜ | Later — today it's server-persisted or `@client`-ephemeral only |
| replicated / conflict-resolved state | ⬜ | Later (§15) |

## 4 · Data & domain modeling

| Capability | Status | Notes |
|---|---|---|
| entities, relations (1-1, 1-many) | ✅ | FK + ON DELETE CASCADE |
| typed columns + auto indexes | ✅ | compiler indexes filtered/ordered/relation fields |
| migrations (additive, versioned, auto-on-start) | ✅ | |
| transactions (action = one tx) | ✅ | |
| `check` validation (action-level) | ✅ | |
| many-to-many | 🟡 | expressible via a join entity; not first-class |
| **declarative field constraints** (required/unique/range/regex) | ⬜ | **Next** — today only via imperative `check` |
| cross-field / relational invariants | ⬜ | **Next** |
| soft delete / archival / audit fields / versioning | ⬜ | **Next** — common dealbreakers |
| value objects | ⬜ | rides §1 records |

## 5 · Query & data access

| Capability | Status | Notes |
|---|---|---|
| filter / sort / limit (`for…where…by…limit`) | ✅ | one model for views + API |
| SQL pushdown + keyset cursor pagination | ✅ | large tables never loaded whole |
| relation traversal (`User(m.to).name`) | ✅ | |
| `count` / `sum` (whole-collection) | ✅ | |
| **filtered aggregates** `count(x in E where …)` | ✅ | per-row counts (likes/replies); filtered `sum` deferred |
| **`exists(x in E where …)`** (correlated membership) | ✅ | per-viewer "have I liked"; powers the Following feed inside `for … where exists(…)` |
| `in` membership operator | ⬜ | Next (sugar; `exists` covers the feed case) |
| **group by / richer aggregates** | ⬜ | **Next** |
| multi-entity joins beyond single-hop lookup | ⬜ | **Next** |
| full-text search | ⬜ | Later |
| vector / semantic search | ⬜ | Later (§AI) |
| policy-aware query planning (row/field filtering in the planner) | 🟡 | policies enforced at the gate; not pushed into the query |

## 6 · Actions, effects & policies

| Capability | Status | Notes |
|---|---|---|
| mutation statements (assign/add/set/remove/clear) | ✅ | |
| `requires` policies, `check` preconditions | ✅ | |
| `@optimistic` | ✅ | |
| jobs: cron + on-start, durable queue, retry/backoff/dead-letter | ✅ | |
| service calls (external "brains" over HTTP, typed contract) | ✅ | fire-and-forget **and** request→response |
| **request→response service calls** (bind a result back) | ✅ | **shipped v1.18.0** — `op(...) -> T`; `let x = call S.op(...)` binds the typed answer (scalar or list) into the action; list params allowed; failure aborts via `failed(...)` |
| **first-class typed effects / capability system** | ⬜ | **Next, keystone** — see below |
| idempotency keys / replay protection | ⬜ | Next |
| compensation / saga / multi-step workflows | ⬜ | Later |
| human-approval / long-running state machines | ⬜ | Later |

### The keystone: a real effects system

Today effects are ad-hoc: impurity (`now`/`rand`) forces server, DB writes force
server, services are a bespoke node. To express *any* app with **no anonymous side
effects**, effects must become **first-class typed capabilities**: an action
declares the effects it performs (`db.write`, `http`, `clock`, `random`, `email`,
`push`, `queue`, `secrets`, `browser`), the type carries them, placement reads
them, and tests mock them. This is the single feature that (a) generalizes
services, jobs, and impurity into one model, (b) makes placement provably sound at
scale, and (c) unlocks email/SMS/push/queue integrations without one-off nodes. It
is the most important thing on this roadmap after request→response calls.

## 7 · Views & UI

| Capability | Status | Notes |
|---|---|---|
| nodes: box/row/text/image/button/input/select/form/upload/link/if/for/use/slot | ✅ | |
| interpolation (text + button labels) | ✅ | |
| components (`use`) + layouts (`slot`) | ✅ | |
| SSR first paint + fine-grained client patching | ✅ | |
| SPA navigation, theming (`theme:` → CSS vars) | ✅ | |
| keyed list reconciliation | 🟡 | lists re-render; verify keying for big/animated lists |
| **forms: dirty/touched/pending, array/dynamic fields, autosave** | ⬜ | **Next** — `form` exists, form *state* doesn't |
| async/cross-field/server validation wired to fields | 🟡 | `check` returns a message; no field-level mapping |
| tables w/ selection + bulk actions; modal/drawer/toast patterns | ⬜ | **Next** — the "internal tools" dealbreakers |
| accessibility primitives (focus, ARIA, keyboard, error announce) | ⬜ | **Next** — currently minimal |
| typed generic components | ⬜ | rides §1 generics |
| streaming SSR / partial hydration / islands | ⬜ | Later |
| page metadata (title/OG/canonical), sitemap/RSS | 🟡 | `<title>` set; no declarative meta/SEO surface |

## 8 · Routing & pages

| Capability | Status | Notes |
|---|---|---|
| path routes, dynamic params (`/post/:id`), layouts, route guards, `link` SPA nav | ✅ | |
| screens (guarded multi-surface playgrounds) | ✅ | login vs app routing with no redirect code |
| nested routes / route stacks | 🟡 | layouts cover some; no true nesting |
| load/prefetch/transition/leave hooks, loading & error boundaries | ⬜ | **Next** (error boundaries) / Later (the rest) |
| query/hash params, locale-aware routes | 🟡 | locale negotiated server-side; not in routes |

## 9 · Auth, authz & security

| Capability | Status | Notes |
|---|---|---|
| auth builtin (signup/login/logout/setRole, reset, verify) | ✅ | first user = admin |
| RBAC + row-level + parameterized policies | ✅ | enforced on authority, shipped to hide UI |
| sessions (HMAC, HttpOnly/SameSite/Secure, sliding expiry) | ✅ | |
| CSRF, rate limit, brute-force lockout | ✅ | |
| TOTP MFA, OIDC SSO (PKCE) | ✅ | |
| audit log, `@secret` AES-256-GCM at rest, one `FACET_SECRET` | ✅ | |
| field-level visibility (per-field authz) | 🟡 | row-level ✅; field-level via separate entities only |
| passkeys / WebAuthn, signed URLs, CSP/clickjacking hooks | ⬜ | Later |

**This family is Facet's strongest. Mostly maintenance + field-level + passkeys.**

## 10 · Networking, APIs & realtime

| Capability | Status | Notes |
|---|---|---|
| auto JSON API (`/api`, `/api/<Entity>`, `POST /api/<action>`) | ✅ | zero app code |
| live updates over SSE (entity changes fan out) | ✅ | every tab converges |
| typed client gen (Swift/Kotlin/TS from IR) | ✅ | can't drift from server |
| **OpenAPI / JSON-schema export** | ⬜ | **Next** — cheap, high external value |
| websocket channels (presence/typing/cursors) | ⬜ | Later — SSE covers most today |
| GraphQL / gRPC endpoints | 🚫 | the IR-derived JSON API is the contract; not adding parallel API styles |
| inbound webhooks as typed endpoints | 🟡 | billing webhook exists; not a general primitive |

## 11 · Files, jobs, background

| Capability | Status | Notes |
|---|---|---|
| uploads + serving (`/upload`, `/uploads/`) | ✅ | |
| durable cron/queue jobs, retry/backoff/dead-letter | ✅ | once-across-cluster |
| blob adapters (S3/GCS), signed URLs, image transforms | ⬜ | Later — local disk only today |
| triggers (DB-event / webhook / action-completion / file-event) | 🟡 | cron + on-start only |

## 12 · Cross-cutting platform

| Capability | Status |
|---|---|
| multi-tenancy (orgs/teams/invites, `tenant`/`tenantRole`) | ✅ |
| auto-admin CRUD at `/admin` | ✅ |
| billing ledger + signed webhook | ✅ |
| i18n catalogs, GDPR export/erase, retention sweeps | ✅ |
| observability: structured logs, Prometheus, healthz/readyz, OTLP | ✅ |
| clustering (Postgres LISTEN/NOTIFY) | ✅ |
| backup/restore, graceful shutdown, timeouts, micro-cache | ✅ |
| supply chain: SBOM, cosign, SLSA | ✅ |
| remote facet registry (GitHub, lockfile, pinned) | ✅ |
| **typed config schema + feature flags in-language** | ⬜ (**Next**) |
| **typed error hierarchy** (validation/auth/notfound/conflict/…) | ⬜ (**Next**, rides §1 sum types) |
| **placement/dataflow graph inspector + leak diagnostics** | 🟡 `facet build` prints IR; no graph viz / "why" (**Next**) |
| testing: `facet test` behavior tests (in-memory, fake auth/clock) | ✅; UI-render/property/migration tests ⬜ (Later) |

## 13 · Offline / local-first

⬜ Entirely **Later** (a major version of its own). Needs: offline state classes,
queued mutations, replicated/conflict-resolved data, sync hooks (LWW /
server-authoritative / merge / CRDT). The placement model is the right foundation
(it already classifies authority) — but this is deliberately deferred until the
core + effects system land.

## 14 · Multi-target

| Target | Status |
|---|---|
| web app | ✅ |
| API-only service | ✅ |
| job runner / worker | ✅ |
| mobile (SDK gen) | 🟡 typed client SDKs; not a full from-graph mobile UI |
| desktop shell | ⬜ Later |
| edge deployment | ⬜ Later (rides §2 edge placement) |

## 15 · AI-era (plan now, build later)

⬜ All **Later**, but the architecture already gestures at it: **services = external
brains.** Once request→response calls (§6) + the effects system land, AI features
are just typed effects/services: vector fields, embedding/inference jobs,
prompt/tool/agent actions as effects, moderation hook points, retrieval pipelines
as queries/jobs, streaming-output UI primitives.

---

## The rings, in order

### Next (v2) — the smallest set that makes Facet "express any app"
1. ✅ **Request→response service calls** — bind a brain's result back into an action (shipped v1.18.0). This is the keystone for the F33D3R rebuild: fct is the edge brain (Nantar), a typed client of the mesh (AethyrRank/Ain Soph/Verity/Astraon/…). Next: typed records for structured payloads (today: scalars + lists).
2. **The effects/capability system** — the keystone; generalizes services/jobs/impurity; unlocks email/push/queue.
3. **Sum types + pattern matching + scoped generics** — variants, state machines, typed errors, generic components.
4. **Typed error hierarchy** (rides #3).
5. **Declarative data constraints** (unique/required/range) + **soft-delete/audit-fields**.
6. **Forms with state** (dirty/touched/pending, array fields) + **error boundaries** + **a11y primitives**.
7. **Tables + modal/drawer/toast** UI patterns (internal-tools dealbreakers).
8. **DX: placement-explanation diagnostic** + **graph/IR inspector**.
9. **Query depth: group-by + multi-hop joins.**
10. **OpenAPI/JSON-schema export** + **typed config & feature flags.**

### Later (v3+)
Offline/local-first (§13) · edge placement & deploy · websocket realtime
(presence/typing) · blob adapters/signed URLs/image transforms · full-text & vector
search · streaming SSR/islands · desktop shell · saga/workflow primitives ·
passkeys/WebAuthn · persistence classes (localStorage/TTL) · undo/redo · property &
UI-render tests · AI-era features (§15).

### Out of scope (a feature, not a gap)
- **`async`/await** — placement subsumes it; there is no round-trip to hand-write.
- **First-class functions / closures / macros** — would make Facet general-purpose and break declaration-first placement. The IR + facets are the extension mechanism.
- **GraphQL / gRPC parallel APIs** — the IR-derived JSON API is the single contract.
- **Raw escape hatches** (inline SQL, arbitrary FFI) — allowed only as explicit, typed, placement-annotated capabilities, never as an unchecked back door. "No anonymous effects" is the whole point.

---

## The decision (made 2026-06-20): stay an application language

We considered making FA **general-purpose** and deliberately decided **against** it.
The goal is to **build a real site (f33d3r.com)**, not to build a language for its
own sake — and the application-language FA already covers the large majority of what
a site needs. Going general-purpose (full functions/closures/generics/traits/
inference, a both-sides function runtime, eventually self-hosting) is a multi-year
detour that would push the site years out for capabilities it doesn't need.

So the line is drawn deliberately: add **sum types, pattern matching, and scoped
generics** — and *not* first-class functions, closures, async, or macros. Facet
stays a **declaration-first application language** where the compiler owns
placement. Everything in "Next" is sized to that line. Revisit only if the goal
itself changes from "build my site" to "build a language" — and only with a real
reason.
