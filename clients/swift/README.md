# FacetKit — Facet Architecture for iOS / macOS (SwiftUI)

The native Apple client runtime for [Facet Architecture](../../README.md). It does
for iOS what `fa-runtime.js` does for the browser: render a **server-authored
neutral view tree** to native SwiftUI views, hold one SSE connection, apply live
updates by facet id, and forward taps to the server. **No application logic ships
to the device** — the Go server is the single source of truth for web *and* native.

This is the same idea as React Native (swap the renderer under one tree), except FA
swaps the renderer under one *wire protocol*, and the device runs no app logic.

## How it works

```
Go server ──FA-Native: 1──▶ {title, tree}     initial screen as a neutral ViewNode tree
   │                                            (kind: box|text|button|image|input|link|icon)
   │  SSE /sse  ──▶ {op, facet_id, fragment}   live updates (fragment parsed → ViewNode)
   ▼
FacetClient (ObservableObject)
   ├── renders the tree with FacetView (SwiftUI)
   ├── applies replace/append/prepend/remove by facetId  (surgical, like the web)
   └── POST /events {type, payload, conn}        taps → server → pushed result
```

## Usage

```swift
import FacetKit
import SwiftUI

struct ContentView: View {
    @StateObject private var client = FacetClient(
        baseURL: URL(string: "https://app.example.com")!
    )
    var body: some View {
        FacetScreen(client: client, route: "/")
    }
}
```

That's the whole app. Tapping a `button` node sends its `action` to the server; the
server re-renders the affected facet and pushes a `replace` back, which `FacetClient`
applies to the exact node by `facetId` — the same surgical update model as on web.
Navigation (`client.navigate(to:)` or a `link` tap) loads a new screen without
dropping the SSE connection.

## Layout

`box` nodes map to `VStack`/`HStack` via a class heuristic (rows lay out
horizontally); `text` → `Text`, `button` → `Button`, `image` → `AsyncImage`,
`input` → `TextField`, `icon` → an SF Symbol. A future revision will carry an
explicit flex/style model from the server (see the framework roadmap) so layout is
exact rather than heuristic.

## Build & test

```sh
cd clients/swift
swift build
swift test     # FacetKitTests mirrors the Go fa.ParseView tests
```

(Requires a Swift toolchain / Xcode on macOS.)

## Status

Built: the view-tree model, the HTML→tree parser (port of `fa.ParseView`), the SSE
client, surgical update application, action forwarding, and the SwiftUI renderer.
The server side (`FA-Native` route responses, `RenderTree`) is in `fa` and tested in
Go. Next: an explicit server-driven style/layout model, HMAC event verification
(parity with the web runtime), and the Android/Compose sibling client.
