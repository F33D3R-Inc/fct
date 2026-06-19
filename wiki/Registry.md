# The Facet Registry — remote facets

A **facet** is a reusable slice of an application — data, server logic, and UI
bundled together — published as an ordinary GitHub repository and pulled into
another app by its repo path:

```
import "github.com/acme/dislike"
```

There is **no central registry server**. GitHub *is* the registry: a facet is a
public (or private) repo of `.fct` files, versions are git tags, and a fetched
facet is pinned to an immutable commit. This is the Go-modules model —
reproducible, offline after first fetch, and zero infrastructure to operate.

Remote imports build on [local module imports](Modules.md): once a remote facet
is fetched and cached, it is just files on disk, merged into your app exactly
like a local `.fct`. **Placement still runs once over the merged graph**, so an
imported facet never has to say whether its pieces run on the server or in the
browser — the compiler decides, in the context of your app.

---

## Consuming a facet

### 1. Add the dependency

```sh
facet add github.com/acme/dislike
```

This resolves the latest `vX.Y.Z` release, downloads it, and writes a
**`facet.lock`** entry pinning the exact commit and tarball hash. Pin a specific
version instead with `@`:

```sh
facet add github.com/acme/dislike@v1.2.0     # exact tag
facet add github.com/acme/dislike@^1.2.0     # highest 1.x ≥ 1.2.0
facet add github.com/acme/dislike@~1.2.0     # highest 1.2.x ≥ 1.2.0
facet add github.com/acme/dislike@main       # a branch (not reproducible — warns)
facet add github.com/acme/dislike@<40-hex>   # an exact commit
```

> Versions live **only** in `facet.lock`, never in the `import` line. A facet
> used by five files is updated in one place. Writing `@version` inside an
> `import "..."` string is a compile error that points you here.

### 2. Import it in your app

```
import "github.com/acme/dislike"

app MyApp:
    auth
    view Home at "/":
        box:
            use DislikeButton(post.id)
```

Importing the repo root loads its **entry file** (the manifest `main`). Import a
specific file with a subpath:

```
import "github.com/acme/audit/trail.fct"
```

### 3. Run it

```sh
facet run app.fct
```

`build`, `run`, and `dev` **auto-fetch** any locked-but-missing dependency, so a
fresh `git clone` + `facet run` works after a `git pull`. **Commit `facet.lock`**
(it is never git-ignored) so teammates get the exact same facet bytes.

---

## Publishing a facet

A published facet is a repo containing:

1. One or more `.fct` files (each a normal, runnable `app` module).
2. A **`facet.json`** manifest at the repo root.
3. Git **tags** of the form `vMAJOR.MINOR.PATCH` marking releases.

### `facet.json`

```jsonc
{
  "name": "github.com/acme/dislike",  // MUST equal the repo path
  "version": "1.2.0",                  // should equal the git tag
  "main": "dislike.fct",               // entry file when the repo root is imported
  "description": "A reusable dislike capability (data + action + UI).",
  "facet": ">=1.4.0",                  // minimum toolchain (semver range)
  "license": "MIT"
}
```

- `name` **must** equal `github.com/<owner>/<repo>`; a mismatch is rejected on
  fetch (this prevents repo-rename confusion).
- `main` defaults to `main.fct`, then to the sole `.fct` in the repo root.
- `facet` is checked against the running toolchain; too-old toolchains fail the
  build with a clear message.
- Unknown keys are ignored, so the format can grow.

### Publish = push + tag

```sh
facet publish
```

`facet publish` validates the manifest (name matches the git origin, version
present), runs `facet build` on the entry file to prove it compiles, requires a
clean working tree, then creates and pushes the `v<version>` tag. It refuses if
the tag already exists or the build fails. The manual equivalent is simply:

```sh
git tag v1.2.0 && git push origin v1.2.0
```

---

## Versioning & resolution

- **Versions are git tags** `vX.Y.Z`. A repo with no release tag can still be
  consumed by pinning a branch or commit, with a "not reproducible" warning for
  branches.
- **Resolution** prefers the `facet.lock` pin (offline, exact). Only an
  unlocked or updated dependency lists tags over the network.
- **One version per repo.** Transitive dependencies resolve into the **single**
  project `facet.lock`. The highest required version within a major wins;
  requiring the **same repo at two different majors is a hard error**.

Update to newer releases deliberately:

```sh
facet update                         # re-resolve every dependency to latest
facet update github.com/acme/dislike # just one
```

Trace why a facet is in your build:

```sh
facet why github.com/acme/dislike
# github.com/acme/dislike is imported by:
#   app.fct → github.com/acme/feed → github.com/acme/dislike
```

---

## Cache, lockfile, and offline builds

Fetched facets are cached, content-addressed by commit, under
`${FACET_CACHE:-$HOME/.facet/cache}`:

```
$FACET_CACHE/github.com/<owner>/<repo>/<commit-sha>/…
```

A commit directory is written once (atomically) and reused forever; once every
locked commit is cached, **builds never touch the network**.

`facet.lock` (committed, machine-maintained — don't hand-edit) records the
resolved version, the immutable commit SHA, the tarball integrity hash, and the
resolved entry file:

```jsonc
{
  "lockfileVersion": 1,
  "facet": "1.4.0",
  "modules": {
    "github.com/acme/dislike": {
      "version": "v1.2.0",
      "commit": "a1b2c3d4…",
      "integrity": "sha256-…",
      "main": "dislike.fct"
    }
  }
}
```

For fully air-gapped builds, vendor everything into the repo:

```sh
facet vendor    # copies resolved facets into ./facet_modules (git-ignored by default)
```

The resolver prefers a vendored copy over the global cache when present.

---

## Security & trust model

- **HTTPS only.** The only network egress is to `api.github.com` and
  `codeload.github.com`. Arbitrary redirects to other hosts are not followed.
- **Immutable pins.** The lock pins a **commit SHA**, not a tag (tags can be
  moved); downloads are by SHA.
- **Integrity.** The tarball's sha256 is recorded in the lock and verified on
  fetch. A changed hash for the same commit is treated as tampering and is a
  hard failure. Set `FACET_VERIFY=1` to re-verify even cache hits.
- **Safe extraction.** Path-traversal entries (`..`, absolute paths) and
  symlinks are rejected; total extracted size is capped (decompression-bomb
  guard).
- **Tokens.** Private repos and higher rate limits use `FACET_GITHUB_TOKEN`. The
  token is sent only to GitHub, never logged, and never written to the lock.
- **No fetch-time code execution.** A facet is declarative `.fct`; fetching and
  extracting never runs facet code. And because **placement soundness applies to
  imported code too**, an imported module's server action cannot read your
  app's `@client` state or see secrets it wasn't given. This is a real
  supply-chain advantage — but it is not total safety: an imported facet can
  still define server actions and entities your app then exposes. **Importing a
  facet is trusting its author, the same as any dependency.** Pin versions,
  review what you add, and commit your lock.

---

## Command summary

| Command | What it does |
|---|---|
| `facet add <ref>[@version]` | Resolve, fetch, and pin a remote facet in `facet.lock`. |
| `facet get [file.fct]` | Fetch every locked dependency into the cache (the fresh-clone path). |
| `facet update [<ref>]` | Re-resolve dependencies to their latest allowed version. |
| `facet why <ref> [file]` | Show the import path(s) by which a facet enters the build. |
| `facet publish` | Validate + build + tag + push a release (run in a facet repo). |
| `facet vendor` | Copy resolved facets into `./facet_modules` for offline builds. |

→ Back to **[Home](Home.md)** · see also **[Modules & Imports](Modules.md)** and
the **[CLI Reference](CLI-Reference.md)**.
