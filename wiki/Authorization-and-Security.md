# Authorization & Security

Facet ships authentication (who you are), authorization (what you may do), and a
hardened edge — all enforced on the **authority**, threaded through the same IR
and placement model. A policy is just a node; a session is just runtime state.

## `auth` — built-in users

Add the bare line `auth` to your app and you get:

```
app Social:
    auth
```

- **Actions:** `signup(username, password)`, `login(username, password)`,
  `logout`, `setRole(username, role)` (admin only), plus password reset and
  account verification.
- **Scope values** available everywhere expressions run: `actor` (username or
  `"guest"`), `role` (`admin` | `member` | `guest`), `verified` (bool).
- **First user is admin.** The first account to sign up is promoted; an admin
  manages everyone else's role with `setRole`.
- Passwords are **bcrypt**-hashed; credentials never reach a client.

## Policies — RBAC + row-level authorization

A `policy` is a named predicate enforced on the authority (and shipped so the UI
can hide what you can't do).

```
policy admin:                       # role-based (a plain permission)
    role == "admin"

policy mine(id: int):               # row-level (parameterized)
    actor == Post(id).author
```

- A **zero-parameter** policy is a permission check.
- A **parameterized** policy is *row-level*: it reads the specific row being
  acted on, and the gate binds its parameters from the action's arguments.

Attach policies to actions with `requires`, or guard whole pages with
`view … requires <policy>` (a [route guard](Views-and-UI.md#routing)).

```
action erase(id: int):
    requires mine(id)               # you may erase YOUR post, not anyone's
    remove Post(id)

action purge:
    requires admin                  # only an admin may wipe everything
    clear Post
```

Any `requires` forces the action onto the server — a permission check is only
meaningful where it can be trusted.

## Sessions

Session cookies are **HMAC-signed** (tamper-proof), `HttpOnly` + `SameSite=Lax`
+ `Secure` (behind TLS / `FACET_SECURE_COOKIES=1`), with a **sliding 24-hour
expiry** that refreshes on activity. Sessions live in the shared store, so they
survive a restart and work across a cluster.

## The hardened edge

| Protection | How |
|---|---|
| **CSRF** | a per-session token a cross-origin page cannot read, required on the browser mutation channel |
| **Rate limiting** | per-IP throttle, `FACET_RATE_LIMIT` req/s |
| **Brute-force lockout** | an account freezes after repeated bad logins |
| **Account verification** | email/account verification with one-time, hashed, expiring tokens |
| **Password reset** | one-time, hashed, expiring tokens |
| **TOTP MFA** | RFC 6238 enrollment + a second factor at login |

## OIDC single sign-on

Authorization-code flow with **PKCE**, configured from the environment, that
auto-provisions users. Set the issuer/client and the callback routes light up:

```
FACET_OIDC_ISSUER=https://accounts.google.com
FACET_OIDC_CLIENT_ID=...
FACET_OIDC_CLIENT_SECRET=...
FACET_OIDC_REDIRECT=https://your-host/auth/oidc/callback
```

Routes: `GET /auth/oidc/login` and `GET /auth/oidc/callback`. OIDC fronts
Google / Entra / Okta / Auth0 / Keycloak. See [Configuration](Configuration.md).

## Audit log

Every server action — and its allow/deny — is recorded to an append-only table
and an in-memory ring, readable by an admin at **`GET /api/_audit`**.

## `@secret` field encryption

A field marked `@secret` is **AES-256-GCM encrypted at rest**: the working set
holds plaintext, the database never does.

```
entity Note:
    id: int
    body: text
    secret: text @secret      # ciphertext on disk
```

## Secrets management

One environment variable, **`FACET_SECRET`**, derives every key — cookie/CSRF
signing, token hashing, and `@secret` encryption.

```sh
export FACET_SECRET=$(facet config --gen-secret)
```

> **Set `FACET_SECRET` in production.** Without it the server runs on an
> ephemeral key that does not survive a restart — cookies, MFA secrets, and
> encrypted columns would all break across restarts. `facet config` warns you
> when it (or `FACET_SECURE_COOKIES`) is unset.

→ Next: **[Views & UI](Views-and-UI.md)**.
