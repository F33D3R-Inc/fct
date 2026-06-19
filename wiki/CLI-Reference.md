# CLI Reference

Every `facet` subcommand. The toolchain is one binary — compiler, server, tools.

```
facet <command> [args]
```

| Command | Summary |
|---|---|
| [`new`](#facet-new) | scaffold a new project |
| [`dev`](#facet-dev) | run with hot reload (in-memory, no DB) |
| [`run`](#facet-run) | serve the web + API projections |
| [`build`](#facet-build) | compile and print the IR |
| [`console`](#facet-console) | interactive REPL against the app |
| [`test`](#facet-test) | run the app's behavior tests |
| [`seed`](#facet-seed) | load fixture rows |
| [`migrate`](#facet-migrate) | reconcile the database schema |
| [`backup` / `restore`](#facet-backup--facet-restore) | logical snapshots |
| [`generate`](#facet-generate) | emit native mobile clients |
| [`deploy`](#facet-deploy) | write Docker/compose assets |
| [`config`](#facet-config) | show resolved config / mint a secret |
| [`lsp`](#facet-lsp) | run the editor language server |
| [`version`](#facet-version) | print the toolchain version |

A local `.env` is folded into the environment for every command (real env wins).

---

## facet new

```sh
facet new <name>
```

Scaffolds `./<name>/` with `app.fct`, a README, Docker/compose + `.env.example`,
and `app.seed.json` / `app.test.json` fixtures.

## facet dev

```sh
facet dev <file.fct> [addr]
```

Runs a **hot-reloading** dev server (default `:7373`) entirely **in memory** — no
database required. Edits to the file reload the page. Best for learning and
iterating.

## facet run

```sh
facet run <file.fct> [addr]
```

Serves the production web + API projections (default `:7373`). Reconciles the
schema on startup, prints a banner (data store, security, ops, enterprise
posture, admin URL), and shuts down gracefully on SIGTERM. Needs
`FACET_DATABASE_URL`; set `FACET_SECRET` in production.

## facet build

```sh
facet build <file.fct>
```

Compiles the app and prints the **IR** (the application graph) as JSON —
including the computed `placement` of every state and action. The fastest way to
see what the compiler decided.

## facet console

```sh
facet console <file.fct>
```

An interactive REPL against the app — evaluate expressions and run actions.

## facet test

```sh
facet test <file.fct> [tests.json]
```

Runs behavior tests (default `app.test.json`) against an in-memory instance —
deterministic, no database. Exits non-zero on failure. A test file:

```json
{
  "tests": [
    {
      "name": "you may delete only your own post",
      "steps": [
        { "as": { "actor": "ada" }, "run": "post", "args": ["mine"] },
        { "as": { "actor": "bob" }, "run": "remove", "args": [1], "fails": "forbidden" },
        { "as": { "actor": "ada" }, "run": "remove", "args": [1] },
        { "expect": "count(Post)", "equals": 0 }
      ]
    }
  ]
}
```

Each step may set the actor (`as`), `run` an action with `args` (optionally
asserting it `fails` with a message substring), or `expect` an expression to
equal a value.

## facet seed

```sh
facet seed <file.fct> [data.json] [--dry]
```

Loads fixture rows (default `app.seed.json`). `--dry` loads into the in-memory
store without persisting. Format:

```json
{ "Post": [ { "author": "ada", "body": "first", "created": 1718800000 } ] }
```

## facet migrate

```sh
facet migrate <file.fct>          # apply
facet migrate <file.fct> --plan   # dry-run (print pending changes)
```

Reconciles the Postgres schema with the IR. Additive and versioned in
`facet_migrations`. See [Data Modeling → Migrations](Data-Modeling.md#migrations).

## facet backup / facet restore

```sh
facet backup  <file.fct> [out.json]   # stdout by default
facet restore <file.fct> [in.json]    # stdin by default
```

Write / replay a logical snapshot of all rows.

## facet generate

```sh
facet generate <file.fct> [dir]      # default dir: ./mobile
```

Emits typed native client SDKs (Swift, Kotlin, TypeScript) from the IR. See
[Enterprise → Mobile clients](Enterprise.md#mobile-clients).

## facet deploy

```sh
facet deploy <file.fct>
```

Writes `Dockerfile`, `.dockerignore`, `docker-compose.yml`, `.env.example` into
the current directory (existing files are kept). See
[Operations → Deploying](Operations.md#deploying).

## facet config

```sh
facet config                  # print resolved config + warnings
facet config --gen-secret     # mint a fresh FACET_SECRET (32-byte hex)
```

See **[Configuration](Configuration.md)**.

## facet lsp

```sh
facet lsp
```

Runs the editor language server over stdio (diagnostics, completion, hovers).
Editor integrations live under `editors/` (VS Code, Vim, Neovim).

## facet version

```sh
facet version          # also: -v, --version
```

→ See **[Configuration](Configuration.md)** for environment variables.
