# Facet Registry

> **Status: SHIPPED in v1.5.0.** This file was the original build spec; the
> feature is now implemented and documented as user-facing docs. The spec is
> superseded — see the guide below.

Remote facet imports — `import "github.com/owner/repo"` — are live. GitHub is the
registry (no central server): a facet is a public/private repo of `.fct` files
with a `facet.json` manifest and `vX.Y.Z` release tags. The toolchain fetches a
repo as an immutable, commit-pinned tarball over HTTPS (no `git` binary), caches
it content-addressed by commit, pins the exact commit + tarball integrity hash in
a committed `facet.lock`, and feeds the cached files into the existing local
merge → dedup → cycle-check → placement pipeline. Reproducible, offline after
first fetch, and placement-sound across the import boundary.

**Read the shipped docs:**

- **[wiki/Registry.md](wiki/Registry.md)** — consuming and publishing facets, the
  full command reference, versioning/resolution, cache & lockfile, and the
  security/trust model.
- **[wiki/Modules.md](wiki/Modules.md)** — local vs. remote module composition.
- **[wiki/CLI-Reference.md](wiki/CLI-Reference.md)** — `facet add`/`get`/`update`/
  `why`/`publish`/`vendor`.

**Where it lives in the code:**

- `internal/registry/` — the resolver: `registry.go` (`Resolver`, `New`,
  `IsRemote`, `Resolve`, `Save`, version selection, cache), `github.go` (tag
  listing, ref→SHA, tarball download, safe extraction), `lock.go` (`facet.lock`),
  `manifest.go` (`facet.json`), `semver.go` (selection + ranges), and
  `registry_test.go` (offline `httptest` unit/integration tests).
- `internal/compile/compile.go` — `File` builds a `*registry.Resolver` and
  `loadModule` resolves every import through it; everything downstream is
  unchanged.
- `cmd/facet/main.go` — the `add`/`get`/`update`/`why`/`publish`/`vendor`
  subcommands.
- `internal/parser/parser.go` — rejects an inline `@version` in an import string.
