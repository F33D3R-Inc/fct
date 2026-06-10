# Contributing to Facet Architecture

Thanks for helping build the server-authoritative web. The bar: every change keeps
the suite green and the gofmt/vet clean.

## Dev loop

```sh
go build ./...        # compiler, runtime lib, stdlib, CLI
go test ./...         # all packages
go vet ./... && gofmt -l .   # must be empty
go test ./fa -bench . # perf, if you touched hot paths
```

Run the demo end to end: `go run ./examples/demo` → http://localhost:7373.

## Where things live

- `internal/{lexer,ast,parser,codegen}` — the compiler. Hand-written, no deps.
- `fa/` — the server runtime library apps import.
- `runtime/fa-runtime.js` — the fixed client runtime.
- `std/` — the standard library of facets.
- `cmd/fct/` — the CLI (`new/dev/build/check/fmt/add/audit/lsp/parse/lex`).
- `editor/vscode/` — syntax + LSP client.

## Rules

- **No external Go dependencies in the framework** (`go.mod` stays require-free).
  Optional integrations (a Redis `Broker`, etc.) are adapters apps supply.
- **Security is structural.** New event paths go through `Guard`/scoped delivery;
  new render paths respect `who:`. Run `fct audit` on facets that touch auth.
- **The client never gets application logic.** It stays pure plumbing.
- **Add a test.** New language features need parser + codegen + an `fa`/`fatest`
  end-to-end test. New stdlib facets are covered by `std`'s compile-all test.
- A language/grammar change needs an entry in `DECISIONS.md` (an ADR).

## Standard-library facets

Use current FDL (`what`/`looks`/`when`/`who`, composition, slots). Style via
`fa-*` classes in `std/style.css`. Every facet must compile (the `std` test
enforces it). Prefer composing existing atoms.

## Commit / PR

- Conventional, imperative commit subjects.
- PRs: what changed, why, and how you verified (tests/bench/demo).
- By contributing you agree your work is licensed under the repo's MIT license.
