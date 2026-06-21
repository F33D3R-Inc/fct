# Custom identity & server-only values

Facet ships built-in auth (signup/login/sessions/RBAC — see
[Authorization & Security](Authorization-and-Security.md)). But a platform often
owns identity itself: a device-signature handshake, an external identity brain, a
cryptographic UUID that authorizes everything but must never be shown. Two
primitives make that expressible without leaving the language — and without
leaking.

## `establish` — adopt a session identity

An action can set the session's identity from values it computed:

```
establish actor <expr>            # set the renderable session identity
establish actor <expr> role <expr>
```

Setting who you are is the authority's job, so an action that uses `establish`
is **server-placed**. `actor` becomes the rendered identity (the handle); a
reactive `{actor}` updates when it changes. This is the hook for a custom login —
typically a *verify-action* that calls an identity brain
([request→response](Services.md)) and adopts the result:

```
service Elohim at "http://elohim:8093":
    verify(handle: text, sig: text) -> text

state pid: text @private          # the opaque UUID — see below

action login(handle: text, sig: text):
    let uuid = call Elohim.verify(handle, sig)   # the brain verifies the device sig
    pid = uuid                                   # keep the key private
    establish actor handle                       # the handle is the rendered identity
```

A bad signature → the brain answers non-2xx → the call aborts the action (it never
adopts an unverified identity). There is no built-in `auth` required; identity is
sourced entirely from the app + brain.

## `@private` — server-only, never rendered

A `@private` state cell is **authoritative** (server-placed) **and** server-only:

- it is **never shipped to a client** — not in the state bootstrap, not in deltas;
- it is a **compile error to render** it (interpolating it in text/label/badge/…),
  so a secret cannot leak through the UI;
- it **can** key policies, gate logic, and feed service calls.

```
state pid: text @private
policy member: pid != ""                  # authorize on the UUID
policy owns(id): pid == Doc(id).owner

# text "{pid}"     ← compile error: @private cannot be rendered
# establish actor pid   ← compile error: actor is renderable; keep the key private
```

So the pattern for a PIAL-style identity is: **the UUID lives in `@private` and
authorizes; the handle is `actor` and renders.** The compiler guarantees the key
is uncompilable to render and never crosses to the client.

A runnable demo: `examples/identity.fct` (+ `examples/services/mock_brain.py`).

→ Back to **[Home](Home.md)** · see also **[Services](Services.md)** and
**[Authorization & Security](Authorization-and-Security.md)**.
