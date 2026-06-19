# Facet — the road to enterprise

Honest map of what's built and what's left to make Facet a language you can run a
large, regulated, high-traffic product on. v1.0.0 is a real, usable foundation —
you can build and ship a small-to-mid social site with it today. This document is
the rest of the climb, in the order that actually unblocks production.

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

## Phase 3 — Reliability & operations (run it for real)

The current server is single-process: in-memory SSE fan-out and in-memory job
tickers don't survive a second instance or a restart mid-job.

- **Horizontal scale** — stateless servers; shared session store and **cross-instance
  pub/sub** for live updates (Redis/NATS).
- **Durable jobs** — persistent queue with retries, backoff, dead-letter, cron.
- **Observability** — structured logs, **Prometheus metrics**, **OpenTelemetry
  tracing**, health/readiness endpoints.
- **Resilience** — graceful shutdown, timeouts, retries, caching, backups/DR.

## Phase 4 — Language depth & richer UI

- **Types** — lists, optional/nullable, decimal/money, first-class dates,
  enums; a small standard library (string/date/math).
- **Validation** — declarative action input constraints with friendly errors.
- **Routing** — dynamic params (`/post/:id`), nested layouts, route guards.
- **Components** — reusable view fragments; richer primitives (forms, selects,
  dates, modals, keyed/virtualized lists).
- **Frontend** — SPA navigation (no full reload), styling/theming system,
  accessibility, optimistic updates, file/media uploads.

## Phase 5 — Delivery, DX & supply chain

- **Tooling** — dev server with hot reload, `facet console`, seed data, a testing
  framework for Facet apps.
- **Editor** — LSP (completion, go-to-def, inline errors), syntax highlighting.
- **Deploy** — Dockerfile/image, one-command deploy, config/secret management.
- **Supply chain** — SBOM, keyless-signed releases, SLSA provenance (the release
  pipeline had this; re-add it).

## Phase 6 — Enterprise platform

- **Multi-tenancy** — tenant isolation, teams/orgs, invitations.
- **Auto-admin** — a generated admin dashboard (Django-admin style).
- **Billing** — subscriptions/usage integration.
- **Compliance** — i18n, GDPR data export/erasure, retention policies.
- **More targets** — native mobile (iOS/Android) reading the same IR.

---

Nothing here is an architectural rewrite — each item is another node kind, store
capability, or runtime service threaded through the **same IR + placement model**.
That is the whole point of the compiler-first design: the language grows by
addition, not by forking into "frontend" and "backend" again.
