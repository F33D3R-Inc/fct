# Native Clients (iOS & Android)

The same server that drives your website drives native apps — with **zero
application logic on the device**. Where React swaps `react-dom` for
`react-native`, FA swaps the renderer under one wire protocol: the server
renders each facet to a **platform-neutral view tree** instead of HTML, signs
it, and pushes it over the same SSE connection. The device only draws.

```
server ──RenderTree──► {kind:"button", action:"tip.send", facetId:"TipButton",
                        children:[{kind:"text", text:"🪙 100"}], style:{…}}
   web runtime  → DOM
   FacetKit     → SwiftUI views        (clients/swift)
   FacetKit     → Jetpack Compose      (clients/android)
```

`kind` is abstract (`box`/`text`/`button`/`image`/`input`/`link`/`icon`),
`facetId` still addresses surgical updates, `action` is the event a tap sends
to the same `/events` endpoint. Styles are resolved **server-side**
(`fa/style.go` is the only style table); native clients hold no style logic.

## iOS (SwiftUI)

`clients/swift` — add the FacetKit package, then a whole app is:

```swift
import FacetKit

struct ContentView: View {
    @StateObject var client = FacetClient(baseURL: URL(string: "https://app.example.com")!)
    var body: some View {
        FacetScreen(client: client, route: "/")
    }
}
```

## Android (Jetpack Compose)

`clients/android` — add the `facetkit` module, then:

```kotlin
setContent {
    val client = remember { FacetClient("https://app.example.com") }
    FacetScreen(client, route = "/")
}
```

## What the clients do (and all they do)

1. Load a route as a neutral tree (request header `FA-Native: 1`).
2. Render it with native views, laid out from the server-resolved `Style`
   (`direction`/`gap`/`pad`/`align`, `bg`/`fg`/`fontWeight`/`radius`).
3. Hold the SSE connection; apply surgical updates by `facetId`.
4. **Verify the HMAC on every pushed frame** (the signing key arrives on the
   `_conn` frame; the signed bytes are exactly the tree JSON rendered) — a
   tampered frame is dropped, same as the web runtime.
5. Forward taps as `{type, payload}` POSTs to `/events`.

Your handlers, guards, sessions, and facets are shared with the web app
unchanged — one server, three renderers.

## Primitive semantics on device

Both runtimes fetch `/manifest.json` and enforce the same per-primitive rules
as the web runtime:

- **stream `window:`** — appended/prepended children are capped; the DOM-
  equivalent tree never grows unbounded.
- **signal `ttl:`** — payloads land as `data-*` attributes +
  `fa-signal-live` on matching `data-fa-signal` nodes and revert after the
  TTL. A programmatic hook is available too: `client.onSignal = { id, payload in … }`.
- **vault** — register the key on the device with
  `client.vaultKey("DM", hexKey: …)` (Swift) / `client.vaultKey("DM", hexKey)`
  (Kotlin); it is never sent to the server. Envelopes decrypt locally
  (AES-GCM) and render the `decrypt:` body; any failure leaves the envelope
  untouched.
- **media** — mounted nodes render a real player: AVKit on Apple platforms
  (HLS native). On Android, FacetKit ships no player dependency — plug one in
  once at startup:

  ```kotlin
  FacetKitConfig.mediaRenderer = { node -> MyExoPlayerComposable(node) }
  ```

  (default: a poster-style placeholder).
