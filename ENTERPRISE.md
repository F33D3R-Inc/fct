# Facet — the road to enterprise

Honest map of how Facet became a language you can run a large, regulated,
high-traffic product on. As of **v1.3.0 every phase below is shipped** — from the
v1.0.0 foundation through data-at-scale, authorization, reliability, language
depth, delivery, and the enterprise platform. This document records the whole
climb, in the order that actually unblocked production. For how to *use* any of
it, see the [wiki](wiki/Home.md).

## Built in v1.0.0

- **Compiler + placement calculus** — one declarative graph; the compiler decides
  server vs. client. Soundness enforced (authority can't see/touch client state).
- **Data** — entities persisted in **Postgres** (`FACET_DATABASE_URL`); in-memory
  working set, write-through durability.
- **Language** — entities, server/client state, derives, aggregates (`count`/`sum`),
  policies, actions, jobs (scheduled server work), effectful builtins (`now`/`rand`).
- **Queries** — `for x in C where … by … limit …` (filter / sort / paginate).
- **Auth** — built-in users, `signup`/`login`/`logout`, bcrypt, `actor` + `role`,
  first user is admin; credentials never reach a client.
- **Pages & routing** — multiple `view … at "/path"`, `link` navigation.
- **Projections** — server-rendered web + live updates (SSE) + JSON API, one graph.
- **Packaging** — `facet new`, `facet version`, cross-platform release binaries.

## Phase 1 — Data at scale ✅ (shipped in v1.1.0)

The first ceiling is gone. Entity data is no longer a bag of JSON documents
filtered in memory; it lives in real, typed, indexed tables and reads push down
to SQL, so a table can grow well past what fits in one process.

- **Query pushdown to SQL** ✅ — the JSON API compiles `where`/`by`/`limit` (and
  `field=value` filters) to indexed `SELECT`s with **keyset cursor pagination**
  (`?after=…`, a `next` cursor in the reply); a table is never loaded whole.
- **Real columns + indexes** ✅ — every entity field is a typed column
  (`int`→`BIGINT`, `text`→`TEXT`, `bool`→`BOOLEAN`); the compiler flags every
  relation, filtered, and ordered field and the store builds a database index
  for it.
- **Migrations** ✅ — `facet migrate <app.fct>` reconciles the schema with the
  IR; `--plan` dry-runs it. Changes are **additive** (CREATE TABLE / ADD COLUMN
  / ADD CONSTRAINT / CREATE INDEX) so they are safe to run with no downtime, and
  every applied statement is versioned in `facet_migrations`. The same migration
  runs automatically on startup.
- **Transactions** ✅ — a multi-statement action's writes ride one transaction
  and commit atomically; any failure rolls the whole action back, so the
  database never holds a half-applied action.
- **Relations** ✅ — relation fields are real foreign keys with **ON DELETE
  CASCADE**; removing a parent cascades to its children in both the database and
  the live in-memory working set (and fans out over SSE). Reverse lookups (a
  user's posts) are indexed; cross-relation reads (`User(m.to).name`) are joins
  over the graph.

> Upgrading from v1.0.0 (JSON-document storage)? v1.1.0 uses columnar tables.
> Point it at a fresh database, or migrate your old rows out of the legacy
> `data` column, then `facet migrate` to build the typed schema.

## Phase 2 — Authorization & security hardening ✅ (shipped in v1.2.0)

Auth (who you are) is now joined by authz (what you may do) and a hardened edge.
Every gate is enforced on the authority, threaded through the same IR and
placement model — a policy is just a node, a session is just runtime state.

- **RBAC + row-level authorization** ✅ — policies are roles (`role == "admin"`)
  *and* per-row owner checks: a **parameterized policy** reads the specific row
  being acted on (`policy owns(id): actor == Post(id).author`) and is passed the
  action's arguments at the gate (`requires owns(id)`), so you may edit *your*
  post, not anyone's. The first user is admin; an admin manages roles with the
  built-in `setRole`.
- **Session hardening** ✅ — session cookies are **HMAC-signed** (tamper-proof),
  `HttpOnly` + `SameSite=Lax` + `Secure` (TLS / `FACET_SECURE_COOKIES`), with a
  **sliding 24h expiry** that refreshes on activity. **CSRF** is blocked on the
  browser channel by a per-session token a cross-origin page cannot read;
  **per-IP rate limiting** (`FACET_RATE_LIMIT`) throttles floods; **brute-force
  lockout** freezes an account after repeated bad logins.
- **Account lifecycle** ✅ — **email/account verification** and **password reset**
  (one-time, hashed, expiring tokens); **TOTP MFA** (RFC 6238) with enrollment
  and a second factor at login; **OIDC single sign-on** (authorization-code +
  PKCE), configured from the environment and auto-provisioning users.
- **Audit log** ✅ — every server action (and its allow/deny) is recorded to an
  append-only table and an in-memory ring, readable by an admin at
  `GET /api/_audit`. **Secrets management** ✅ — one `FACET_SECRET` derives every
  key (cookie/CSRF signing, encryption). **Field-level encryption at rest** ✅ —
  a `@secret` column is **AES-256-GCM** encrypted in the database; the working
  set holds plaintext, the disk never does.

> SSO ships as OIDC, the modern standard that fronts Google/Entra/Okta/Auth0/
> Keycloak; a SAML SP would ride the same `/auth/...` callback shape. Set
> `FACET_SECRET` in production — without it the server runs on an ephemeral key
> that does not survive a restart (cookies, MFA secrets, encrypted columns).

## Phase 3 — Reliability & operations ✅ (shipped in v1.3.0)

The server is no longer single-process. SSE fan-out and job scheduling survive a
second instance and a restart mid-job, and the process is observable and
shutdown-safe.

- **Horizontal scale** ✅ — set `FACET_CLUSTER=1` and several stateless servers
  cooperate. Sessions live in the shared store, and **cross-instance live
  updates** ride **Postgres `LISTEN`/`NOTIFY`** — the database you already run is
  the bus, so there is no Redis/NATS to operate. A single-process dev run keeps
  the in-memory path.
- **Durable jobs** ✅ — every scheduled `every Ns` job is a **cron entry** in a
  **persistent queue**; on each interval exactly one instance wins the reservation
  and enqueues a row, so a job fires once across the cluster, not once per
  instance. A failed job is **retried with exponential backoff** and, once it
  exhausts its attempts, **dead-lettered** (kept for inspection) rather than lost.
- **Observability** ✅ — **structured JSON logs** (`slog`, level via
  `FACET_LOG_LEVEL`), **Prometheus metrics** at `GET /metrics`, OTLP log export
  (`FACET_OTLP_LOG`), and **`/healthz` (liveness) + `/readyz` (readiness)** probes
  for an orchestrator.
- **Resilience** ✅ — **graceful shutdown** on SIGTERM/SIGINT (in-flight requests
  drain, workers stop, the database closes), production **timeouts**
  (read-header / idle; the SSE stream is exempt), an optional short-TTL API
  **micro-cache** (`FACET_API_CACHE_TTL`), and logical **backup/restore**
  (`facet backup` / `facet restore`).

## Phase 4 — Language depth & richer UI ✅ (shipped in v1.3.0)

The language grew the types and view primitives a real product needs — every one
a new node kind through the same IR.

- **Types** ✅ — **lists** (`[T]`), **optional/nullable** (`T?`), **`money`** and
  first-class **`date`**, and **enums** (closed text types) that flow through
  fields, state, params, and `select` options.
- **Validation** ✅ — declarative action preconditions: `check <cond> "message"`
  runs before the body and returns a friendly error when it fails.
- **Routing** ✅ — **dynamic params** (`view Post at "/post/:id"`), **layouts**
  (`view … in Layout`, a `layout` with a `slot`), and **route guards**
  (`view … requires <policy>` — the authority refuses to render it, the client
  hides links to it).
- **Components** ✅ — reusable view fragments (`component Name(params):` invoked
  with `use Name(args)`), plus richer primitives: **`select`** (with `option`s or
  enum-defaulted), **`form`**, and **`upload`**.
- **Frontend** ✅ — **SPA navigation** (matched links swap the page with no full
  reload), a **theming** system (`theme:` block → CSS custom properties),
  **optimistic** actions (`action … @optimistic`), and **file/media uploads**
  (`POST /upload`, served from `/uploads/`, `FACET_UPLOAD_DIR`).

## Phase 5 — Delivery, DX & supply chain ✅ (shipped in v1.3.0)

The authoring loop and the release pipeline are first-class.

- **Tooling** ✅ — `facet dev` (a **hot-reloading** dev server that runs with no
  database), `facet console` (an interactive REPL against the app), `facet seed`
  (load fixture rows), and `facet test` (a **behavior-test framework** for Facet
  apps).
- **Editor** ✅ — an **LSP** (`facet lsp`: diagnostics, completion, hovers) and
  **syntax highlighting** for VS Code, Vim, and Neovim (under `editors/`).
- **Deploy** ✅ — a **Dockerfile** and **docker-compose** (app + Postgres), written
  by `facet deploy` (and into every `facet new` project) for a one-command stack.
- **Supply chain** ✅ — the release workflow emits a **CycloneDX SBOM**
  (`scripts/sbom.sh`), **keyless-signs** the checksums/SBOM/provenance with cosign
  (Sigstore, the workflow's OIDC identity — no long-lived key), and attaches a
  **SLSA v1.0 provenance** statement (`scripts/provenance.sh`).

## Phase 6 — Enterprise platform ✅ (shipped in v1.3.0)

The platform layer — multi-tenancy, an admin, billing, compliance, and more
targets — every piece a reserved table / reserved action / runtime service
threaded through the same IR + placement model.

- **Multi-tenancy** ✅ — `FACET_MULTI_TENANT=1` turns on orgs/teams, memberships,
  and invitations, and threads the session's active **`tenant`** and the actor's
  **`tenantRole`** into the eval scope, so an app scopes its own rows with an
  ordinary policy (`policy sees(id): Doc(id).org == tenant`).
- **Auto-admin** ✅ — a generated, admin-only **CRUD dashboard** over every entity
  at **`/admin`** (Django-admin style), pure projection of the IR; on by default,
  `FACET_ADMIN=0` removes it.
- **Billing** ✅ — `FACET_BILLING=1` keeps an authoritative **subscription + usage
  ledger** synced by an **HMAC-signed provider webhook** (`/billing/webhook`); an
  app gates features on `GET /api/_billing`.
- **Compliance** ✅ — **i18n** message catalogs (`FACET_I18N_DIR`, negotiated per
  request, served at `/api/_i18n`), **GDPR** data **export** (`/api/_export`) and
  **erasure** (`/api/_erase`), and declarative **retention** sweeps
  (`FACET_RETENTION`).
- **More targets** ✅ — `facet generate <app.fct> [dir]` emits typed native client
  SDKs — **Swift** (iOS), **Kotlin** (Android), and **TypeScript** (React
  Native / web) — straight from the IR, talking to the same `/api` projection.

## Beyond the roadmap — modules & imports ✅ (shipped in v1.4.0)

The first capability past the original roadmap, and the foundation for a
community ecosystem. A `.fct` file may `import "other.fct"`; the compiler
resolves each module relative to the importing file and **merges every module's
declarations into one graph before placement**. So a large app is many small
files instead of one growing monolith, and a reusable **facet** — data + logic +
UI bundled together — is simply a module another app pulls in. Because placement
is computed over the merged graph, a module is a *vertical slice* that never has
to declare a "layer." Imports de-duplicate, cycles are rejected, and a name
declared twice is a compile error. (A hosted publish/fetch **registry** is the
next, larger step; local module composition is what it will stand on.)

---

Nothing here is an architectural rewrite — each item is another node kind, store
capability, or runtime service threaded through the **same IR + placement model**.
That is the whole point of the compiler-first design: the language grows by
addition, not by forking into "frontend" and "backend" again.
