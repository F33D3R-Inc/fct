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

The call is **fire-and-forget**: a side effect that never blocks the action's
response. If the service is down, the action still succeeds; the failure is
logged, not surfaced. (Request→response calls that bind a result into the graph
are the next step.)

## Grammar

```
service <Name> at "<http(s)-url>":
    <op>(<param>: <Type>, …)
    …
```

- `<Name>` is referenced as `call <Name>.<op>(args)` inside an action body.
- The URL must be `http://` or `https://`.
- Operations are typed signatures, like an action's — the parameter names become
  the JSON keys sent.

A runnable demo is in `examples/service.fct`.

→ Back to **[Home](Home.md)** · see also **[Actions & Logic](Actions-and-Logic.md)**
and **[Layered Facets](Layered-Facets.md)**.
