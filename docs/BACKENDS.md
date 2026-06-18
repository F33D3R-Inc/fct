# Multi-language backends

FA's pitch is **language-agnostic via the compiler** (ROADMAP #4, ENTERPRISE
P2 #13). The compiler front end — lex (offside rule) → parse → AST → semantic
checks → a flat render-node stream — is entirely target-independent. Only the
final lowering is language-specific, and it lives behind one interface:
`codegen.Backend`.

This reverses the narrow part of **ADR-0001** ("our only backend target is Go"):
the compiler core no longer assumes Go. See DECISIONS.md ADR-0003.

```
              ┌──────────────────────── target-neutral ────────────────────────┐
  .fct  ─▶  lexer ─▶ parser ─▶ ast ─▶ checks ─▶ flat render stream ─▶ manifest.json
              └─────────────────────────────────────────────────────┬──────────┘
                                                                     │
                                          codegen.Backend (per target)
                                          ├─ Expr       FDL expr → target expr
                                          ├─ FieldName  FDL name → target ident
                                          └─ Types      facets   → typed data decls
```

`fct.toml` selects the target:

```toml
[compiler]
target = "go"      # go (default) | node | python | rust
```

## Two tracks

Breaking the "Go lock" is two distinct bodies of work. Keep them separate.

### 1. Compiler track — **done**

The compiler emits, for every target, from one FDL source:

| Artifact | Go | Node | Python | Rust |
|---|---|---|---|---|
| Typed per-facet data | `types.go` struct | `types.ts` interface | `types.py` `@dataclass` | `types.rs` struct |
| Expression lowering | html/template pipeline | JS infix | Python infix (`and`/`or`/`not`) | Rust infix |
| Identifier casing | `UserID` | `userId` | `user_id` | `user_id` |
| Render description | `*.tmpl.html` (native) | `manifest.json` (IR) | `manifest.json` (IR) | `manifest.json` (IR) |
| Routing/auth/reactive metadata | `manifest.json` | `manifest.json` | `manifest.json` | `manifest.json` |

Implemented in `internal/codegen/`:

- `backend.go` — the `Backend` interface + registry + `BackendFor`/`BackendNames`,
  and `goBackend` (delegates to the unchanged `goExpr`/`GoName`/`GoStructs`, so
  the Go path is byte-for-byte what it was).
- `exprast.go` — one neutral expression parser (`parseExpr` → `exNode` tree) the
  non-Go backends render via `renderInfix`. The Go path keeps its own single-pass
  `goExpr` and is untouched.
- `backend_node.go`, `backend_python.go`, `backend_rust.go` — the three new
  targets. Each is ~80 lines: register, `Expr`, `FieldName`, `Types`, `TypesFile`.

Tested in `backend_test.go` (registry, expression lowering golden cases across all
three, error propagation, typed-data emission). `cmd/fct` reads `[compiler] target`
and routes `runBuild` through the selected backend.

### 2. Runtime track — **core shipped** (`runtimes/`)

A non-Go target needs a **server runtime** equal to `fa/` — the part that turns a
manifest + handlers into a live app. The core live-render loop is now implemented
for all three targets under `runtimes/{node,python,rust}` (dependency-free in each
language): manifest + render-IR load, render-IR interpreter + neutral expression
evaluator, HMAC-SHA256 signing matching `fa/event.go`, an in-memory SSE hub, the
`GET /` · `GET /sse` · `POST /events` endpoints, and `when:`-driven re-render.
The native (`FA-Native`) neutral-tree path also ships in all three: each runtime
mirrors Go's `RenderTree = ParseView(Render(...))` (render HTML → parse to a
ViewNode tree → serialize), so the existing iOS/Android clients (`clients/swift`,
`clients/android`) work against Node/Python/Rust too — the emitted tree JSON is
byte-identical to `fa.ParseView`. Remaining `fa/` surface (sessions, forms, `who:`
authz, broker fan-out, rate limiting) is additive against the same contract. Per
language the surface is:

- **Render-IR interpreter** — render a facet from the neutral IR (below) instead of
  Go `html/template`. ~a few hundred lines; the browser runtime (`runtime/fa-runtime.js`)
  already does the client half against the same manifest, so the contract is proven.
- **SSE hub** — per-connection event stream, scope/target routing (`fa/hub.go`).
- **Fragment signing** — HMAC over each pushed fragment, key + rotation window
  (`fa/security.go`, `FA_SIGNING_KEY[_PREVIOUS]`). Must match the wire format the
  client verifies, byte-for-byte.
- **Sessions, routing, forms, authz (`who:`), broker** — the rest of `fa/`.

The client runtime, the manifest schema, and the wire/signing format are **shared
across all targets** — that is what caps the blast radius. A new server runtime
must conform to them, not invent its own.

## The neutral render IR

The render program is **data, not code** — it ships in `manifest.json`, one entry
per facet (`facetEntry` in `internal/codegen/codegen.go`). The fields a runtime
interprets to render and react:

- `name`, `kind`, `facet_id` — identity + the id pattern for scoped delivery.
- `view` / `client` — the neutral render body for client-rendered/reactive bodies:
  the flat `Text` / `{expr}` / `{if}…{else}…{end}` / `{for v in xs}…{end}` /
  `{cmp Child|field=expr}` stream, as an FDL-shaped string (`clientBodyText`). This
  is the same description the browser runtime already interprets — a non-Go server
  renders the *server-rendered* kinds from the same shape.
- `template` — the Go `html/template` source. **Go target only**; absent for other
  targets (they render from the stream, not a Go template).
- `state`, `derived`, `bindings`, `lists`, `regions`, `actions`, `effects`,
  `queries`, `handlers` — the fine-grained reactive graph (docs/REACTIVITY.md):
  signal inits, derived expressions, text/attr bindings (signal set → DOM node),
  reactive list/`if` regions, client actions and effects. Already neutral.
- `when` — server event handlers: the events that trigger a re-render and the
  mutations (`replace`/`append`/…) applied. The handler *bodies* are app code in
  the target language; the manifest carries the wiring.
- `who` — authorization surface (`require` policies, redactions) for `fct audit`.

Because expressions inside these entries are still FDL (`count > 0`), a runtime
either (a) evaluates FDL directly with a tiny interpreter, or (b) the compiler
pre-lowers them per target via `Backend.Expr`. Choosing between (a) and (b) — and
freezing the IR's exact JSON shape as a versioned contract — is the first task of
the runtime track.

## Adding a target

1. Add `backend_<lang>.go`: a `Backend` implementation, `init()`-registered.
2. Add golden cases to `backend_test.go`.
3. (Runtime track) implement the server runtime against the shared manifest +
   wire/signing format.

`Backend.Expr` reuses `parseExpr`; supply an `infixStyle` (root object, identifier
casing, logical-operator keywords, boolean literals, call syntax) and `Types` for
the language's data declarations. No front-end changes are ever required.
