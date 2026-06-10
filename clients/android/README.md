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
guessed from class names. **The style table lives only on the server** — native SSE
connections (`FA-Native: 1`) receive each update as an already-styled neutral tree,
so the client holds no style logic at all.

## Build & test

```sh
cd clients/android
./gradlew :facetkit:test        # unit tests (parser, styles, surgical updates)
./gradlew :facetkit:assemble    # build the AAR
```

(Requires Android Studio / the Android SDK.)

## Status

Built: the view-tree model, the SSE client (receives already-styled trees), surgical
updates, action forwarding, and the Compose renderer. Style is resolved entirely on
the server (`fa.Style`); the client carries no style table. A small HTML→tree parser
remains only as a fallback for fragment-only events.

Roadmap: HMAC event verification (parity with web); richer layout (explicit
width/spacing units).
