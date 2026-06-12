# Troubleshooting & FAQ

Common errors, what they actually mean, and the questions everyone asks.

## Compile-time errors (these are features)

FA never silently ignores input — every one of these names the fix in the
message. The usual suspects:

**"unknown field X in facet Y"** — `looks:` uses an identifier not declared in
`what:`. Add it to `what:` or fix the typo. This is the data contract doing
its job.

**"tabs are not allowed"** — FDL indentation is spaces only. Configure your
editor (the VS Code extension does this for `.fct` files).

**dedent / indentation errors** — nesting is by *relative* indentation; a
dedent must land exactly on an enclosing level. Re-align the block.

**"unknown child facet" / "unknown prop"** — a `<Child .../>` tag names a
facet that isn't compiled (is it in `facets/`? spelled right? did you compile
with `std.CompileDir` so the stdlib is available?), or passes a prop the child
doesn't declare.

**composition cycle** — facet A includes B includes A. Break the loop.

**`{{` rejected** — you can't write raw Go-template syntax in `looks:`;
that's the template-injection guard.

**malformed `throttle:` / `ttl:` / `window:`** — durations are Go syntax
(`250ms`, `3s`); window must be a positive integer.

## Runtime symptoms

**The page loads but nothing updates live.**
Check the browser console and the `/sse` request in the network tab.
- Behind a proxy: is buffering disabled? (`proxy_buffering off` — see
  [Deployment](Deployment.md)). A buffered SSE stream looks exactly like
  "events never arrive."
- Multiple instances without a broker / without stickiness: your `/events`
  POST landed on a different instance than your SSE connection.

**Updates work in one browser but other users don't see them.**
That's by design — handler returns go only to the acting connection. Use
`Broadcast` / `EmitTo` / `EmitChannel` for wider delivery
([Realtime Patterns](Realtime-Patterns.md)).

**"signature verification failed" / fragments dropped after a deploy.**
The signing key changed (ephemeral random key per process is the default).
Set `FA_SIGNING_KEY` to the same value on every instance — see
[Deployment](Deployment.md). Open pages from before a key change need a
reload.

**Channel subscriptions silently do nothing.**
Channels are deny-by-default. Register `app.ChannelAuth(...)`.

**A `who:`-protected facet errors with plain `Render`.**
Also by design — use `RenderFor(view, …)`. The facet declared it needs an
authorization view; the framework refuses to render it without one.

**403 on an event.** A `Guard` rejected it (or CSRF: the request lacked the
per-connection id / failed the Origin check — usually a sign you're calling
`/events` by hand instead of via `data-action`).

**429 on `/events`.** Per-IP rate limit. If you're behind a proxy and all
traffic appears to come from one IP, make sure `X-Forwarded-For` is set by
your proxy.

**My form inputs don't arrive in an event payload.**
`data-action` sends the element's `data-*` attributes only — it doesn't
serialize forms. Full forms POST to a route (`<form method="post"
action="/…">`) and are read with `fa.NewForm(r)`; see
[Sessions, Auth & Forms](Sessions-Auth-and-Forms.md).

## FAQ

**Where do I write JavaScript?** You don't. The runtime is fixed plumbing. If
you think you need JS, you probably want: an event handler (interaction), a
signal (ephemeral UI state), `data-fa-optimistic` (instant feedback), or a
`media`/`vault` primitive (player / client-side decrypt).

**How do I fetch data from my API?** There is no API. Routes and handlers
query your data source directly and return rendered HTML
([Working with Databases](Working-with-Databases.md)).

**How do I do a loading state?** Render the stdlib `Skeleton`/`SkeletonCard`
first, then push the real facet when ready (`replace`).

**Can I use Tailwind/SCSS?** Any CSS works — pass it via
`fa.ShellOptions{CSS: …}` or serve a stylesheet; facets are plain HTML.
Scoped per-facet styles (`style:` block) are on the roadmap.

**How big can an app get?** The std catalog covers X.com-scale surfaces;
per-op costs are ~17–18 µs (render/dispatch). For multi-instance scale-out see
[Deployment](Deployment.md); for honest open items see
[ENTERPRISE.md](../ENTERPRISE.md).

**Why Go for my backend — can I use Node/Python?** Today the compiler targets
Go only; other backend targets are roadmap item #6 in the README.

**Where's the hot reload?** `fct dev` rebuilds on every `.fct` save.

## Still stuck?

- `fct check facets/` — validates everything and names problems precisely.
- `fct parse file.fct` / `fct lex file.fct` — inspect what the compiler sees.
- `fct audit file.fct` — the access-control surface of a facet.
- `/debug/metrics` — live counters (events in/out, connections, forbidden,
  rate-limited) for diagnosing delivery issues.
