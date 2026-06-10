# Publishing

The module path is **`github.com/F33D3R-Inc/fct`**, so it installs straight from
GitHub — no domain, no hosting, no extra service.

## 1. Push the code

```sh
git push origin main
```

## 2. Tag a release (SemVer)

```sh
git tag v0.1.0 && git push --tags
```

`@latest` resolves to the newest tag (or a pseudo-version off the default branch
if there are no tags yet). Pre-1.0, minor versions may break; after `v1.0.0` the
`fa` API and FDL follow semantic versioning (see GOVERNANCE.md). Release notes
live in CHANGELOG.md.

## 3. That's it — anyone can now install

```sh
go install github.com/F33D3R-Inc/fct/cmd/fct@latest   # the `fct` command
```

and in their app: `import "github.com/F33D3R-Inc/fct/fa"` — `go` fetches it from
GitHub on first build. The same machinery that powers `k8s.io`/`gorm.io`, minus
the vanity domain.

> Want a branded import path like `fct.dev` later? Own the domain and serve a
> one-line `go-import` meta tag pointing back to this repo, then change the module
> path. Purely cosmetic — not required to ship.

## 4. The facet registry (`fct add`)

`fct add <name>` fetches `${FA_REGISTRY}/<name>.fct`; `fct add <url>` and
`fct add <path>` also work. To run a registry, serve `.fct`/`.tgz` package files
over HTTPS (or `fct registry <dir>` for a self-hosted file-backed server).
Validation (`fct check`) runs on install, so malformed packages are rejected
before anything is written. Point apps at a registry with `FA_REGISTRY`.

## 5. Editor extension

`editor/vscode` — `cd editor/vscode && npm install && npx vsce package` produces a
`.vsix`; publish to the Marketplace or share the file. Highlighting needs no LSP;
diagnostics use the installed `fct` binary (`fct lsp`).
