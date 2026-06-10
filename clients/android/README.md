# FacetKit for Android (Jetpack Compose)

The native Android client runtime for [Facet Architecture](../../README.md) — the
Kotlin/Compose sibling of the [iOS client](../swift). It renders a
**server-authored neutral view tree** to native Compose, holds one SSE connection,
applies live updates by facet id, and forwards taps to the server. **No application
logic runs on the device** — the Go server is the single source of truth for web,
iOS, and Android alike.

## Usage

```kotlin
class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            val client = remember { FacetClient("https://app.example.com") }
            FacetScreen(client, route = "/")
        }
    }
}
```

That's the whole app. Tapping a `button` node sends its `action` to the server; the
server re-renders the affected facet and pushes a `replace`, which `FacetClient`
applies to the exact node by `facetId` — the same surgical update model as web and
iOS. `client.navigate(route)` (or a `link` tap) loads a new screen without dropping
the SSE connection.

## How it maps

| neutral kind | Compose |
|---|---|
| `box` | `Row` / `Column` (per server `style.direction`) |
| `text` | `Text` (weight/size/color from `style`) |
| `button` | clickable `Row` → sends `action` |
| `image` | Coil `AsyncImage` |
| `input` | `OutlinedTextField` |
| `link` | tappable `Text` → `navigate` |
| `icon` | placeholder |

Layout comes from the server-resolved `style` (direction/gap/pad/align/paint), not
guessed from class names. Live SSE fragments are parsed on-device and re-resolved
through `StyleResolver` (a kept-in-sync mirror of `fa/style.go`) so updates look
identical to the initial server-rendered screen.

## Build & test

```sh
cd clients/android
./gradlew :facetkit:test        # unit tests (parser, styles, surgical updates)
./gradlew :facetkit:assemble    # build the AAR
```

(Requires Android Studio / the Android SDK.)

## Status

Built: the view-tree model, the HTML→tree parser (port of `fa.ParseView`), the
on-device style resolver (mirror of `fa.Style`), the SSE client, surgical updates,
action forwarding, and the Compose renderer. The server side (`FA-Native` responses,
`RenderTree`, `Style`) is in `fa` and tested in Go.

Roadmap: push already-styled neutral trees over SSE so the style table lives only on
the server (removing the client mirror); HMAC event verification (parity with web);
richer layout (explicit width/spacing units).
