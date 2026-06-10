# Publishing

The module path is **`fct.dev`** (a vanity import path), so `go install fct.dev/...`
works regardless of where the code is hosted.

## 1. Host the code

Push this repo to a Git host (e.g. `github.com/<org>/facet`).

## 2. Serve the vanity import meta at `fct.dev`

Go's tooling fetches `https://fct.dev/<path>?go-get=1` and reads a meta tag to find
the real repo. Serve this for any `fct.dev/...` request (a one-file static host or
a tiny redirect service):

```html
<meta name="go-import" content="fct.dev git https://github.com/<org>/facet">
<meta name="go-source" content="fct.dev https://github.com/<org>/facet https://github.com/<org>/facet/tree/main{/dir} https://github.com/<org>/facet/blob/main{/dir}/{file}#L{line}">
```

Then `go install fct.dev/cmd/fct@latest` and `import "fct.dev/fa"` resolve to your
GitHub repo. (`k8s.io`, `gorm.io`, `gopkg.in` work exactly this way.)

> Hosting on GitHub without a vanity domain instead? Change the module path to
> `github.com/<org>/facet` (`go mod edit -module …` + update imports) and skip this
> step.

## 3. Tag releases (SemVer)

```sh
git tag v0.1.0 && git push --tags
```

Pre-1.0, minor versions may break. After `v1.0.0`, the `fa` API and FDL follow
semantic versioning (see GOVERNANCE.md). The release notes live in CHANGELOG.md.

## 4. The facet registry (`fct add`)

`fct add <name>` fetches `${FA_REGISTRY:-https://registry.fct.dev}/<name>.fct`;
`fct add <url>` and `fct add <path>` also work. To run a registry, serve `.fct`
package files over HTTPS at those paths. Validation (`fct check`) runs on install,
so malformed packages are rejected before anything is written.

## 5. Editor extension

`editor/vscode` — `cd editor/vscode && npm install && npx vsce package` produces a
`.vsix`; publish to the Marketplace or share the file. Highlighting needs no LSP;
diagnostics use the installed `fct` binary (`fct lsp`).
