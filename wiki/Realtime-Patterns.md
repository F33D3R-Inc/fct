# Realtime Patterns

Every live feature in FA is the same primitive: the server renders a fragment,
signs it, and pushes it to **some set of connections**, where the runtime swaps
it in by `data-facet-id`. The design question is always *which set*.

## The four delivery scopes

| Scope | Call | Reaches | Use for |
|---|---|---|---|
| Actor | *(return events from the handler)* | the clicking user's connection | the response to their own action |
| User | `ctx.EmitTo(identity, events…)` | all of one user's connections (every tab/device) | notifications, DMs, badge counts |
| Channel | `ctx.EmitChannel(channel, events…)` | subscribers of a topic | live chat, comments on a post, dashboards |
| Everyone | `ctx.Broadcast(events…)` | every connection | public content only |

The default is the safest: events **returned** from a handler go only to the
actor. Nothing fans out unless you say so. Outside a handler (a form POST, a
cron job, a queue consumer) the same methods live on `app.Hub()`.

```go
app.On("dm.send", func(ctx fa.Ctx) ([]fa.Event, error) {
    msg := saveDM(ctx.Identity, ctx.Payload["to"], ctx.Payload["text"])
    frag := string(c.MustRender("ChatMessage", map[string]any{"Msg": msg}))

    // recipient: every tab they have open
    ctx.EmitTo(ctx.Payload["to"], fa.Event{Op: "append", FacetID: "Thread:" + msg.ThreadID, Fragment: frag})

    // sender: their own echo (the return value)
    return []fa.Event{{Op: "append", FacetID: "Thread:" + msg.ThreadID, Fragment: frag}}, nil
})
```

`EmitTo` identities come from `app.Identify` — wire sessions first
([Sessions, Auth & Forms](Sessions-Auth-and-Forms.md)).

## Channels are deny-by-default

A client asking to subscribe to a channel gets **nothing** until you authorize
it:

```go
app.ChannelAuth(func(identity, channel string) bool {
    room, ok := strings.CutPrefix(channel, "room:")
    return ok && db.IsMember(identity, room)
})
```

No `ChannelAuth` registered = no channel subscriptions at all. This is what
makes `EmitChannel` safe for private group content.

## Event ops

An `fa.Event` is `{Op, FacetID, Fragment}`:

- `replace` — swap the node with `data-facet-id == FacetID` (empty fragment
  removes content).
- `append` / `prepend` — insert inside that node (lists, chat, feeds). If the
  facet is a `stream` with `window: n`, the runtime trims the other end so the
  DOM stays bounded.

Per-instance facet-ids (`PostCard:post:42`) are what make updates surgical: a
like updates one card, not the page.

## Don't poll, don't debounce — declare it

High-frequency sources (tickers, presence, telemetry) shouldn't hand-roll rate
limiting. Declare it on the primitive:

```
stream Ticker:
    throttle: 250ms
    window: 100
    what:
        quote: Quote
    looks:
        <div class="tick">{quote.symbol} {quote.price}</div>
```

The hub enforces `throttle:` (trailing-edge coalescing — the latest frame
always lands), and the runtime enforces `window:` (DOM capped at 100 rows).
Your handler just emits on every change.

## Ephemeral state: signals

For typing indicators, presence dots, cursors — state that should **never be
stored** — use a `signal`:

```
signal Typing:
    ttl: 3s
    what:
        user: str
```

```go
// server: relay (stores nothing, signs the relay)
app.On("compose.keystroke", func(ctx fa.Ctx) ([]fa.Event, error) {
    _ = ctx.Signal("room:"+ctx.Payload["room"], "Typing", map[string]string{"user": ctx.Identity})
    return nil, nil
})
```

```html
<!-- page: pure CSS reaction, zero app JS -->
<span data-fa-signal="Typing" class="typing-dot"></span>
<style>.typing-dot.fa-signal-live { opacity: 1; }</style>
```

The runtime sets the payload as `data-*` attributes, adds `.fa-signal-live`,
and reverts both after `ttl:`.

## Optimistic UI (perceived zero latency)

```
<button data-action="post.like" data-post-id="{post.id}" data-fa-optimistic="active">
```

The class toggles **instantly** on click; the server's authoritative `replace`
reconciles. No reply within the TTL → the guess auto-reverts. Use for toggles
where the server almost always agrees (like, follow, bookmark).

## Multi-instance

All of the above works unchanged across multiple instances once a Broker is
configured — the hub publishes signed events through Redis and each instance
delivers to its local connections. See
[Deployment](Deployment.md#multiple-instances).
