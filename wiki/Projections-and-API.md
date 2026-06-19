# Projections & the API

One graph is served three ways, with no extra code. They are **projections** of
the same IR — you do not write an API, a websocket layer, or a renderer.

- **Web** — server-rendered first paint, then a fine-grained client runtime.
- **Live** — entity changes fan out over **SSE**; every open tab converges.
- **API** — a JSON contract: schema, entity reads, action invocations.

## Web rendering

`GET /` (and every other `view … at "/path"`) returns server-rendered HTML, then
hydrates with the client runtime (`/facet.js`). Navigation between views is SPA
(no full reload). The page is the `view` tree from the
[Views & UI](Views-and-UI.md) reference.

## Live updates (SSE)

The client subscribes to `/event`; when any entity changes (from any user, job,
admin edit, or retention sweep), the authority pushes a delta and every open tab
recomputes its derives, lists, and interpolations. You write nothing to get
this — it falls out of the reactive model.

## The JSON API

| Method & path | Purpose |
|---|---|
| `GET /api` | the **application contract**: entities, fields, types, and invokable actions |
| `GET /api/<Entity>` | the durable rows of an entity (with query params, below) |
| `POST /api/<action>` | invoke a server action: body `{"args":[...]}` |

```sh
curl localhost:7373/api                 # the contract
curl localhost:7373/api/Post            # rows
curl -X POST localhost:7373/api/send \
  -H 'content-type: application/json' \
  -d '{"args":[1,"hi"]}'                # invoke send(to, body)
```

Reserved runtime tables (users, tenancy, billing ledgers) are **never** exposed
as ambient entity data — an admin reads them through `/admin` and the dedicated
endpoints below.

### Querying entities

`GET /api/<Entity>` compiles to indexed SQL with **keyset cursor pagination** — a
large table is never loaded whole.

| Param | Meaning |
|---|---|
| `?by=field` | sort field |
| `?desc=1` | descending |
| `?limit=20` | page size |
| `?field=value` | equality filter on a field |
| `?after=<cursor>` | the next page (cursor comes back as `next` in the reply) |

```sh
curl 'localhost:7373/api/Post?by=created&desc=1&limit=20'
curl 'localhost:7373/api/Post?author=ada&limit=10'
```

### Dedicated endpoints

| Path | Purpose | Page |
|---|---|---|
| `GET /api/_audit` | admin audit feed | [Security](Authorization-and-Security.md#audit-log) |
| `GET /api/_export` | GDPR data export | [Enterprise](Enterprise.md#gdpr-export--erasure) |
| `POST /api/_erase` | GDPR erasure | [Enterprise](Enterprise.md#gdpr-export--erasure) |
| `GET /api/_i18n` | negotiated message catalog | [Enterprise](Enterprise.md#i18n) |
| `GET /api/_billing` | caller's billing standing | [Enterprise](Enterprise.md#billing) |
| `POST /billing/webhook` | signed provider webhook | [Enterprise](Enterprise.md#billing) |
| `POST /upload`, `GET /uploads/…` | file upload + serving | [Views & UI](Views-and-UI.md#form--upload) |
| `GET /admin` | generated admin dashboard | [Enterprise](Enterprise.md#auto-admin) |
| `GET /auth/oidc/login`, `/auth/oidc/callback` | OIDC SSO | [Security](Authorization-and-Security.md#oidc-single-sign-on) |
| `GET /healthz`, `/readyz`, `/metrics` | ops probes + Prometheus | [Operations](Operations.md) |

## Hardened the same way everywhere

Every projection is enforced identically: server-side RBAC + row-level policies,
HMAC-signed sessions, CSRF on the browser channel, rate limiting, brute-force
lockout, the audit feed, and `@secret` encryption. Set `FACET_SECRET` to key it
all. See **[Authorization & Security](Authorization-and-Security.md)**.

## Native clients

`facet generate` emits typed Swift / Kotlin / TypeScript SDKs that talk to this
same `/api` projection — the schema can never drift from the server because both
come from one IR. See [Enterprise → Mobile clients](Enterprise.md#mobile-clients).

→ Next: **[Enterprise](Enterprise.md)**.
