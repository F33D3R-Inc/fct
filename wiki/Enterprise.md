# Enterprise

The platform layer (ENTERPRISE.md Phase 6): multi-tenancy, an auto-generated
admin, a billing ledger, compliance services, and native client generation. Each
is a reserved table / reserved action / runtime service through the **same IR +
placement model** — turned on with an environment flag, off by default.

- [Multi-tenancy](#multi-tenancy)
- [Auto-admin](#auto-admin)
- [Billing](#billing)
- [Compliance: i18n](#i18n)
- [Compliance: GDPR](#gdpr-export--erasure)
- [Compliance: retention](#retention)
- [Mobile clients](#mobile-clients)

---

## Multi-tenancy

Turn it on with `FACET_MULTI_TENANT=1`. The runtime then manages orgs/teams,
memberships, and invitations, and threads two identity values into every
evaluation scope — exactly like `actor`/`role`:

- **`tenant`** — the id of the session's active tenant (`0` = none).
- **`tenantRole`** — the actor's role within it (`owner`/`admin`/`member`, or
  `""`).

Because they are ordinary scope values, **you express tenant isolation in your
own policies**, enforced on the same gate as everything else:

```
policy sees(id: int):
    Doc(id).org == tenant
```

Tenancy manages membership; your app scopes its own rows by `tenant` — exactly
the data the compiler already knows how to filter and index.

**Reserved actions** (invokable like any action, e.g. over `POST /api/<action>`):

| Action | Purpose |
|---|---|
| `createTenant(name)` | create an org and become its owner |
| `switchTenant(id)` | set the session's active tenant |
| `inviteMember(email, role)` | invite (owner/admin only); returns a token |
| `acceptInvite(token)` | join via an invitation |
| `setMemberRole(username, role)` | change a member's role (owner/admin) |
| `removeMember(username)` | remove a member (owner/admin; not the owner) |
| `leaveTenant` | leave the active tenant |

## Auto-admin

A generated, admin-only **CRUD dashboard** over every entity, Django-admin
style, at **`/admin`**. It is pure projection of the IR — no app code declares
it. It lists each entity, browses rows, and creates/edits/deletes through
ordinary store writes that fan out to live clients. Every page and mutation is
gated on an admin session and carries the per-session CSRF token.

On by default; set `FACET_ADMIN=0` to remove it (the routes 404). The
`facet run` banner prints the admin URL when it's enabled.

## Billing

Turn it on with `FACET_BILLING=1`. Facet does not embed a payment SDK — that
would couple the static binary to one vendor. Instead it keeps an **authoritative
local ledger** (subscriptions + a usage meter) and syncs reality from your
provider (Stripe, Paddle, …) via a **signed webhook**.

**Reserved actions:** `subscribe(plan)`, `cancelSubscription`,
`recordUsage(metric, quantity)`.

**Read standing:** `GET /api/_billing` reports the caller's subscription and
metered usage — gate premium features on it.

**Webhook:** your provider POSTs state changes to `POST /billing/webhook`, signed
with an HMAC over the raw body in `X-Facet-Signature` (key:
`FACET_BILLING_WEBHOOK_SECRET`, or derived from `FACET_SECRET`). An unsigned or
mis-signed webhook is rejected — that HMAC is the trust boundary.

```json
{"subscriber":"ada","tenant":0,"customer":"cus_123",
 "plan":"pro","status":"active","periodEnd":1750000000}
```

Under multi-tenancy a subscription is keyed by tenant; otherwise by user.

## i18n

Message catalogs negotiated per request. Drop `<locale>.json` files in
`FACET_I18N_DIR` (set `FACET_DEFAULT_LOCALE`, default `en`). The locale is chosen
from `?lang=` or `Accept-Language`, stamped on the response (`Content-Language`),
and a client fetches its catalog from **`GET /api/_i18n`** to localize — so one
app serves many languages from the same source the server renders against. Zero
catalogs = the app serves its literal strings.

## GDPR export & erasure

- **`GET /api/_export`** — returns every row, across every entity, that names the
  caller — their right of access, as machine-readable JSON. Secret/credential
  fields are redacted. An admin may export anyone with `?user=`.
- **`POST /api/_erase`** — anonymizes the caller: their account is scrubbed
  (username → opaque tombstone, credentials/contact cleared) and every personal
  text field that references them, in every entity, is nulled. **Rows are
  preserved** (counts and relations stay intact); only identifying values are
  removed — the right to erasure without breaking the graph. An admin may erase
  anyone; the subject's live sessions are dropped.

## Retention

Declare max ages per entity in `FACET_RETENTION` (`Entity:field:days,...`) and a
daily sweep deletes rows past their limit, fanning the deletions to live clients.
Each rule is validated against the schema, so a typo is logged and skipped rather
than dropping nothing — or everything.

```sh
FACET_RETENTION=Post:created:90,AuditNote:at:30
```

## Mobile clients

`facet generate <app.fct> [dir]` emits typed native client SDKs straight from the
compiled IR — a model per entity and a call per server action, talking to the
`/api` projection the web server already serves:

```sh
facet generate app.fct mobile
```

| File | Target |
|---|---|
| `Facet.swift` | iOS / macOS (URLSession, async/await) |
| `Facet.kt` | Android (`java.net.http`, kotlinx.serialization) |
| `facetClient.ts` | React Native / web (`fetch`) |
| `README.md` | usage |

The schema can never drift from the server because both are generated from one
IR. Regenerate whenever the app changes. The reserved credential table and
client-only actions are omitted, exactly as in `GET /api`.

→ Next: **[Operations](Operations.md)**.
