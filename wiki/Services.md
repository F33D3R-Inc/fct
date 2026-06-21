# Services — calling external brains

A Facet app is one declarative graph, but a real platform is many services. A
`service` lets an action **call out** to an external service (a "brain") over
HTTP, with a typed contract the compiler checks — without breaking placement
soundness.

```
service Moderation at "http://moderation:8090":
    review(id: int, body: text)

action post(body: text):
    requires member
    add Post { author: actor, body: body, created: now() }
    call Moderation.review(Post.last, body)
```

## Why it's safe

- **A `call` is an effect.** Like `now()` or a database write, calling a service
  makes the action **server-placed**. A client can never reach a brain directly —
  it routes through the authority. (Try to make such an action client-only and it
  won't compile.)
- **The contract is checked at compile time.** `service Name at "url"` declares
  typed operations; every `call` is validated against them — unknown service,
  unknown operation, and wrong argument count are all compile errors. That's a
  schema registry, enforced by the language instead of CI.
- **Egress is bounded.** The only outbound calls are to the URLs declared in
  `service` blocks. There is no raw HTTP escape hatch.

## How it runs

`call Service.op(a, b)` posts JSON to `<url>/op` with the operation's parameter
names as keys:

```
POST http://moderation:8090/review
{"id": 42, "body": "…"}
```

A plain `call` is **fire-and-forget**: a side effect that never blocks the
action's response. If the service is down, the action still succeeds; the failure
is logged, not surfaced.

## Request → response — binding the answer back

Declare a typed **return** on an operation and bind its result with `let`:

```
service AethyrRank at "http://aethyrrank:8080":
    rank(viewer: text, posts: [int]) -> [int]   # a list return
service Wallet at "http://ain-soph:8089":
    balance(user: text) -> int                  # a scalar return

state balance: int = 0

action refresh():
    let b = call Wallet.balance(actor)          # wait for the brain's answer
    balance = b                                 # …and surface it as authoritative state
```

The bound name (`b`) is a local the rest of the action body can use — assign it
into a **server** state cell (it reaches every client as a delta), store it in an
entity field, or feed it to a `check`. Because the result is the authority's, it
must land in authoritative (server) state, never `@client`.

The brain replies with JSON: either a bare value (`42`, `[3,1,2]`) or a
`{"result": …}` envelope — both are decoded and coerced to the declared return
type. A transport error or non-2xx **aborts the action** (surfaced via
`failed(<action>)`), so a down brain fails the action instead of lying. The call
is synchronous, so keep bound brains fast — it's the authority's egress, on the
mesh.

A list operation parameter (`posts: [int]`) is allowed on services (unlike action
params), so the keystone shapes — rank a batch of ids, price a cart — are
expressible.

## Grammar

```
service <Name> at "<http(s)-url>":
    <op>(<param>: <Type>, …)            # fire-and-forget
    <op>(<param>: <Type>, …) -> <Type>  # request→response (bind with `let`)
    …
```

- `<Name>` is referenced as `call <Name>.<op>(args)` inside an action body.
- `let <name> = call <Name>.<op>(args)` binds an op's typed return.
- The URL must be `http://` or `https://`.
- Operations are typed signatures — the parameter names become the JSON keys
  sent; a param may be a list (`[T]`); the optional `-> Type` (or `-> [Type]`) is
  the bound return.

A runnable demo is in `examples/service.fct`.

## Webhooks — the inbound twin

A `service` calls *out*; a `webhook` lets the mesh call *in*. An external system
(a payment processor, a transcode worker, any brain) POSTs to a declared path, the
runtime **verifies an HMAC over the raw body**, then runs the named action with the
JSON body decoded into its parameters by name.

```
entity Payment:
    id: int
    ref: text
    cents: int

action confirm(ref: text, cents: int):
    check cents > 0 "amount must be positive"
    add Payment { ref: ref, cents: cents }

webhook "/hooks/pay" -> confirm secret PAY_SECRET
```

- The signature is a hex SHA-256 HMAC over the exact request body, sent in the
  `X-Facet-Signature` header. A missing or mismatched signature is rejected with
  **403** before the action runs — so a webhook is authenticated, not open.
- `secret <ENV>` names the env var holding the HMAC key. Omit it and the key is
  derived from the master secret, so a deployment always has a usable key.
- The action runs with **system authority** (like a job), so it passes policies
  the way a trusted internal caller would. Validate the payload with `check` —
  a failed check returns its message with **422**, and nothing is written.
- Paths must be unique and may not shadow a route the runtime owns (`/api`,
  `/admin`, `/auth/…`, the ops probes, the built-in billing webhook); the
  compiler rejects a collision.

### Grammar

```
webhook "<path>" -> <action> [secret <ENV_VAR>]
```

A runnable demo is in `examples/webhook.fct`.

→ Back to **[Home](Home.md)** · see also **[Actions & Logic](Actions-and-Logic.md)**
and **[Layered Facets](Layered-Facets.md)**.
