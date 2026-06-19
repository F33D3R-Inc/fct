# Configuration

Facet is configured entirely by environment variables (`FACET_*`). A local
`.env` file is folded in for every command, and **real environment variables
always override it**. Run `facet config` to see the resolved values and warnings,
and `facet config --gen-secret` to mint a secret.

## Core

| Variable | Default | Purpose |
|---|---|---|
| `FACET_DATABASE_URL` | — | Postgres connection (`postgres://…`). Required for `facet run`/`migrate`; dev tools work without it. |
| `FACET_SECRET` | ephemeral | Master secret — derives cookie/CSRF signing, token hashing, and `@secret` encryption. **Set it in production** (≥32 bytes); without it keys do not survive a restart. |
| `FACET_SECURE_COOKIES` | `0` | `1` behind TLS so session cookies are HTTPS-only. |

## Security & auth

| Variable | Default | Purpose |
|---|---|---|
| `FACET_RATE_LIMIT` | (built-in) | per-IP request/second throttle. |
| `FACET_OIDC_ISSUER` | — | OIDC issuer URL; presence enables SSO. |
| `FACET_OIDC_CLIENT_ID` | — | OIDC client id. |
| `FACET_OIDC_CLIENT_SECRET` | — | OIDC client secret. |
| `FACET_OIDC_REDIRECT` | — | OIDC callback URL (`https://host/auth/oidc/callback`). |

See [Authorization & Security](Authorization-and-Security.md).

## Operations

| Variable | Default | Purpose |
|---|---|---|
| `FACET_CLUSTER` | `0` | `1` to run multiple instances cooperating over Postgres `LISTEN`/`NOTIFY` + a durable job queue. |
| `FACET_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FACET_OTLP_LOG` | — | export structured logs over OTLP. |
| `FACET_API_CACHE_TTL` | off | short-TTL micro-cache in front of read endpoints. |
| `FACET_UPLOAD_DIR` | (built-in) | directory for uploaded files (served from `/uploads/`). |

See [Operations](Operations.md).

## Enterprise (Phase 6)

| Variable | Default | Purpose |
|---|---|---|
| `FACET_MULTI_TENANT` | `0` | `1` enables orgs/teams/invitations and `tenant`/`tenantRole` scope. |
| `FACET_ADMIN` | on | set `0` to remove the `/admin` dashboard. |
| `FACET_BILLING` | `0` | `1` enables the subscription/usage ledger + webhook. |
| `FACET_BILLING_WEBHOOK_SECRET` | derived | HMAC key for `/billing/webhook` (falls back to a `FACET_SECRET`-derived key). |
| `FACET_I18N_DIR` | — | directory of `<locale>.json` message catalogs. |
| `FACET_DEFAULT_LOCALE` | `en` | default locale when none is negotiated. |
| `FACET_RETENTION` | — | retention rules `Entity:field:days,...` (daily sweep). |

See [Enterprise](Enterprise.md).

## Flag values

Boolean flags accept `1`, `true`, `on`, or `yes` (case-insensitive) as "on";
anything else is off. `FACET_ADMIN` is the exception — it is **on unless set to
exactly `0`**.

## Example production `.env`

```sh
FACET_DATABASE_URL=postgres://facet:facet@db:5432/facet?sslmode=require
FACET_SECRET=<facet config --gen-secret>
FACET_SECURE_COOKIES=1
FACET_CLUSTER=1
FACET_LOG_LEVEL=info

# optional
FACET_MULTI_TENANT=1
FACET_RETENTION=AuditNote:at:30
FACET_OIDC_ISSUER=https://accounts.google.com
FACET_OIDC_CLIENT_ID=...
FACET_OIDC_CLIENT_SECRET=...
FACET_OIDC_REDIRECT=https://your-host/auth/oidc/callback
```

→ Back to **[Home](Home.md)**.
