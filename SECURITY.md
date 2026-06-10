# Facet Architecture — Security Audit & Hardening Plan

Red-team audit of the current framework (module `fct.dev`). Every finding below
was verified against the code, not assumed. The goal: a **built-in security
layer** designed for FA's architecture — the way Django ships CSRF/auth/escaping
out of the box — so apps are safe by default, plus an **FA-native debugger** to
make the server-authoritative flow inspectable.

## Status (live)

| Finding | State |
|---|---|
| **C1** SSE leak | ✅ **fixed** — handler output goes only to the acting connection; `EmitTo`/`EmitChannel`/`Broadcast` for explicit scopes; `channel_auth` **deny-by-default**; conn↔identity anti-spoof. Proven by unit tests + a live two-client no-leak run. |
| **C2** authz | ✅ **enforced (events + render) + auditable** — `App.Guard(event, fn)` gates every event (403 before the handler runs). The `who:` block (`require`/`redact`) is enforced at render: a protected facet **refuses** the plain `Render` path and must go through **`RenderFor(view, …)`**, which checks `require` policies (`ErrForbidden` on deny) and strips `redact` fields from a copy of the data (caller's data never mutated). Declared requirements are reported by **`fct audit`**. All unit-tested. |
| **H1** CSRF | ✅ **fixed** — `/events` requires the per-connection id (readable only over the same-origin SSE stream) **and** a same-origin Origin check. |
| **H2** rate limits / DoS | ✅ **fixed** — per-IP token-bucket on `/events` (429) + per-IP SSE connection cap. |
| **H3** child recursion DoS | ✅ **fixed** — compile-time cycle / unknown-child / unknown-prop detection. |
| **M1** secure headers | ✅ **fixed** — CSP (`script-src 'self'`, no inline/eval), `frame-ancestors 'none'`, `object-src 'none'`, `nosniff`, `Referrer-Policy`. |
| **M2** raw-HTML boundary | ⏸ **policy set** — there is deliberately no unescaped-HTML escape hatch yet (so none can be misused). When added it MUST be an explicit directive that `fct audit` lists. Not needed until a facet must emit trusted rich HTML. |
| **M3** HMAC scope | ✅ **documented** — see M3 below; signing is defense-in-depth (off-path injection on plaintext transports), not per-user auth. The real controls are C1/C2. |
| **L1** hand-built fragments | ⏸ guidance — keep the typed `Render` path; don't string-concat user data into a `Fragment`. |

## Threat model

FA's shape: the server renders HTML from data, signs it, and pushes fragments
over SSE; the browser swaps them in by `data-facet-id`; all user actions hit one
`POST /events`. Attackers we care about:
- a **malicious end-user** (crafted payloads, hostile profile data, forged requests);
- a **malicious/compromised facet** pulled from a future registry ("infected facet");
- a **network attacker** (off-path injection, MITM);
- a **resource attacker** (DoS).

---

## What is already safe (don't break these)

- **XSS auto-escaping** — generated templates use Go `html/template`, which
  escapes data per context (HTML/attribute/JS/URL). Hostile profile data like
  `<script>` renders inert by default.
- **Template-injection is closed** — the `{…}` parser consumes every `{`, so a
  facet author cannot smuggle raw `{{…}}` Go-template directives through `looks:`
  (verified: `{{.Secret}}` is rejected at compile time). *Caveat: you also can't
  write a literal `{` yet — fix carefully, see M2.*
- **facet-ids / props** are escaped in attribute context; the client uses
  `CSS.escape` before `querySelector`.
- **No client application logic** — the runtime is fixed plumbing; there's no
  app code in the browser to exploit or exfiltrate.
- **Tamper-evidence** — pushed events are HMAC-signed and verified client-side
  (see M3 for the honest limits).

---

## Findings (verified, severity-ranked)

### CRITICAL

**C1 — SSE fan-out has no per-recipient scoping.** `fa.Hub` exposes only
`Broadcast` (hub.go:45); every event goes to **every** connected client. The
moment an app pushes anything user-specific (a DM, a private notification, an
authorized fragment), it **leaks to all logged-in users**. This is the single
biggest gap and blocks any real multi-user app.
*Fix:* per-session / per-user / per-channel emit + **subscription authorization**
(`channel_auth`): a client may only subscribe to channels it's allowed to, and the
server emits to addressed recipients, never the whole hub.

**C2 — No authorization on `/events`.** Any client can invoke any registered
handler with any payload (app.go, no auth check). This is IDOR + privilege
escalation by construction (e.g. `{"type":"post.delete","payload":{"postId":"<anyone's>"}}`).
Auth is currently "advisory" — the exact thing FA's spec claims to make
structural, but hasn't.
*Fix:* the `who:` block compiled into server-side guards (`require` / `redact`),
an auth context on every handler, and mandatory payload validation.

### HIGH

**H1 — No CSRF protection.** `POST /events` has no token / origin / SameSite
check (verified). A hostile page can fire events as a logged-in user.
*Fix:* a framework-issued CSRF token embedded by the Playground, sent by the
runtime on every `/events` POST, verified server-side; default SameSite cookies.

**H2 — No rate limiting or connection caps.** Nothing caps `/events` throughput
or concurrent `/sse` connections. Trivial DoS.
*Fix:* per-IP/session token-bucket on `/events`, a max-SSE-connections ceiling,
and keep the existing slow-client drop as backpressure.

**H3 — Child-facet recursion is unguarded.** No cycle detection (verified). A
facet that includes itself — directly or through a cycle — renders forever →
stack-overflow DoS. Now reachable because composition just landed.
*Fix:* compile-time cycle detection in the facet dependency graph + a runtime
render-depth cap.

### MEDIUM

**M1 — No secure-by-default headers.** No CSP, `X-Frame-Options`,
`X-Content-Type-Options`, or HSTS (verified). The runtime applies fragments via
`outerHTML`/`insertAdjacentHTML` (runtime:63-65), so a strong **CSP is the
backstop** if anything ever slips the escaper; absence of `X-Frame-Options`
allows clickjacking.
*Fix:* a secure-headers default on `app.Mount`, with a CSP (nonce) strategy that
fits server-pushed fragments.

**M2 — No audited "raw HTML" boundary.** Today you *cannot* emit unescaped HTML
(safe but limiting). Real apps need it (rendered markdown, oEmbed). When added it
must be an **explicit, greppable, manifest-listed** directive (FA's equivalent of
React's `dangerouslySetInnerHTML` / Django's `|safe`) — never an implicit
`template.HTML` footgun. Design it as structural so `fct audit` can list every
raw sink.

**M3 — HMAC signing has a narrow real threat model.** One key is generated per
process and embedded in every page, so it is a shared verification secret, not
per-user authentication. Under TLS its marginal value is small (it defends mainly
against off-path injection on plaintext transports).
*Fix:* keep as defense-in-depth, **document the real scope honestly**, and don't
let it create a false sense of authorization (C1/C2 are the real controls).

### LOW

**L1 — `Fragment` is a raw string at the swap site.** Safe today because all
fragments come from `html/template`. Becomes dangerous the instant a handler
hand-builds a fragment by string concatenation with user data.
*Fix:* make the typed/escaped render path the only ergonomic one; lint
hand-built fragments.

---

## What React & Django ship — and FA's answer

| Concern | React | Django (batteries) | **FA should provide** |
|---|---|---|---|
| XSS escaping | JSX auto-escape; `dangerouslySetInnerHTML` opt-out | template autoescape; `|safe` | ✅ have (html/template); add audited `raw` (M2) |
| CSRF | app's job | **middleware, on by default** | **build it in** (H1) |
| AuthZ | app's job | auth + permissions framework | **`who:` structural guards** (C2) |
| Data scoping | n/a (client) | querysets per-request | **per-channel SSE + `channel_auth`** (C1) |
| Clickjacking | n/a | X-Frame-Options middleware | secure headers default (M1) |
| Secure headers | n/a | SecurityMiddleware (HSTS, nosniff) | secure headers default (M1) |
| Secrets/signing | n/a | `SECRET_KEY` signing | per-app key (have; scope honestly, M3) |
| Audit surface | n/a | n/a | **`fct audit` security manifest** (spec'd) |

The takeaway: Django's edge is **secure-by-default middleware**. FA's pitch is
stronger — security as a *compile-time structural primitive* (`who:`/`redact`/
`channel_auth` are checked by the compiler and listed in a manifest) — but **none
of that is built yet.** Until it is, FA is *less* safe than Django for multi-user
apps because of C1/C2.

---

## The FA debugger (built for this architecture, not ported)

React DevTools inspects a client component tree. FA's state and rendering live on
the **server**, so the debugger is mostly server-side, surfaced to a dev panel
(itself a facet — dogfood). Dev-only; never enabled in production.

- **Facet tree** — the live `data-facet-id` hierarchy on the page.
- **Event timeline** — `action → handler → mutations → which facet-ids swapped`,
  with timing. The "why did this update?" trace.
- **Wire inspector** — SSE events in/out, signed/verified/dropped, payloads.
- **Security panel** — rejected fragments (bad HMAC), CSRF failures, authz
  denials, and **broadcast-scope warnings** (e.g. "event sent to N clients —
  intended?").
- **Inline guardrails** (compile + dev): recursion/cycle risk, facet-id
  collisions, a sensitive facet with no `who:`, any `raw` sink, user data flowing
  into an unescaped context.

Delivery: a `fa.Dev(mux)` middleware that taps the hub, the event router, and the
compiler, exposing `/.fa/devtools`. The same taps feed the guardrail warnings.

---

## Prioritized hardening roadmap

Security now interleaves with the DX roadmap (README). Recommended order:

1. **C1 — per-channel SSE + `channel_auth`.** Nothing multi-user is safe without it.
2. **C2 + H1 — `who:` structural authz + CSRF.** The headline FA security feature
   and the table-stakes web control, together.
3. **H3 — cycle detection.** Cheap; close the composition DoS now that it's reachable.
4. **M1 — secure headers + CSP default.**
5. **H2 — rate limits / connection caps.**
6. **M2 — the audited `raw` boundary + `fct audit` security manifest.**
7. **FA debugger** — build alongside, since the same hub/router taps power both.

Each is independently shippable and verifiable. C1 and C2 are the gates: until
they exist, FA must be documented as **single-tenant / public-content only.**
