# Installation

Facet ships as a single static binary, `facet`. It contains the compiler, the
web server, the JSON API, the client runtime, and every tool — there is nothing
else to install except a database when you run for real.

## Download a release (recommended)

Grab the binary for your platform from the
[latest release](https://github.com/F33D3R-Inc/fct/releases/latest):

- **macOS / Linux**
  ```sh
  chmod +x facet-*
  sudo mv facet-* /usr/local/bin/facet
  facet version
  ```
- **Windows** — rename `facet-windows-amd64.exe` to `facet.exe` and put it on
  your `PATH`.

Every release binary is pure Go (no CGO) and runs anywhere. The download is
supply-chain verifiable — see [Operations → Supply chain](Operations.md#supply-chain).

## Build from source

You need Go 1.26+.

```sh
git clone https://github.com/F33D3R-Inc/fct
cd fct
go build -o facet ./cmd/facet
./facet version
```

## A database (for `facet run`)

Facet persists entity data in **Postgres**. Point `FACET_DATABASE_URL` at your
database:

```sh
export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/yourdb
```

You do **not** need a database to learn the language: `facet dev` runs entirely
in memory with hot reload, and `facet build` / `facet test` / `facet console`
work without one too. You only need Postgres for `facet run` (production) and
`facet migrate`.

The fastest way to a full stack is the bundled compose file (app + Postgres):

```sh
facet new myapp && cd myapp
cp .env.example .env          # then set FACET_SECRET (facet config --gen-secret)
docker compose up --build
```

See **[Operations](Operations.md)** for production deployment and
**[Configuration](Configuration.md)** for every environment variable.

## Next

→ **[Getting Started](Getting-Started.md)** builds and runs your first app.
