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

## Phase 1 — Data at scale (the first real ceiling)

Today queries filter the in-memory working set and rows are stored as JSON
documents. That is correct but caps you at "fits in memory, single server."

- **Query pushdown to SQL** — compile `where/by/limit` to indexed `SELECT`s;
  cursor pagination; don't load whole tables.
- **Real columns + indexes** — promote entity fields to typed columns; index
  relations and filtered/sorted fields.
- **Migrations** — versioned, safe, zero-downtime schema changes; `facet migrate`.
- **Transactions** — multi-statement actions commit atomically.
- **Relations** — reverse relations (a user's posts), joins, cascade deletes.

## Phase 2 — Authorization & security hardening

Auth (who you are) exists; enterprise needs authz (what you may do) and a hardened
edge.

- **Fine-grained permissions / RBAC** and **row-level authorization** (you may edit
  *your* post, not anyone's).
- **Session hardening** — `Secure`/`SameSite`/signed cookies, expiry + refresh,
  **CSRF** protection, **rate limiting**, brute-force lockout.
- **Account lifecycle** — email verification, password reset, **OAuth/SSO (OIDC/SAML)**, **MFA**.
- **Audit log**, secrets management, field-level encryption at rest.

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
