# Working with Databases

FA deliberately ships **no ORM and no database layer**. Your app is a Go
program; state lives wherever Go can reach — and the standard `database/sql`
ecosystem is the supported path. The framework's only opinion is *where*
queries happen: **in your route functions and event handlers, on the server.**
The browser never sees a query, a connection string, or an API.

The pattern is always the same three lines:

```
read state from the DB  →  render the facet  →  (on change) write, re-render, push
```

This page wires the [guestbook tutorial](Building-Your-First-Website.md) to
**PostgreSQL**. SQLite and others differ only in driver and DSN.

## 1. Run Postgres locally (Docker)

```sh
docker run -d --name guestbook-db \
  -e POSTGRES_USER=app -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=guestbook \
  -p 5432:5432 postgres:17
```

Or put it in a `docker-compose.yml` (the [Deployment](Deployment.md) page has
a full compose file with the app + Postgres + Redis together):

```yaml
services:
  db:
    image: postgres:17
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: guestbook
    ports: ["5432:5432"]
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
```

## 2. Connect

Add the standard Postgres driver (pgx is the de-facto choice):

```sh
go get github.com/jackc/pgx/v5/stdlib
```

In `main.go`:

```go
import (
    "database/sql"
    _ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
    db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
    if err != nil { log.Fatal(err) }
    defer db.Close()
    if err := db.Ping(); err != nil { log.Fatal("db unreachable:", err) }
    // …the rest of the app, passing db where needed…
}
```

```sh
export DATABASE_URL='postgres://app:secret@localhost:5432/guestbook?sslmode=disable'
go run .
```

> **Config convention:** FA itself reads `FA_ADDR`, `FA_SIGNING_KEY`,
> `REDIS_ADDR`. Keep your own config in env vars too (`DATABASE_URL`), so the
> same binary runs in dev, Docker, and production unchanged.

## 3. Schema & migrations

The framework has no migration tool — use the Go ecosystem's. Two good
options, simplest first:

**a) Embedded schema, applied at boot** (fine for small apps):

```go
//go:embed schema.sql
var schema string
…
if _, err := db.Exec(schema); err != nil { log.Fatal(err) }
```

```sql
-- schema.sql
CREATE TABLE IF NOT EXISTS entries (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name    TEXT NOT NULL,
    message TEXT NOT NULL,
    at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**b) Versioned migrations** with
[golang-migrate](https://github.com/golang-migrate/migrate) or
[goose](https://github.com/pressly/goose) once the schema evolves:

```sh
migrate create -ext sql -dir migrations -seq create_entries
migrate -path migrations -database "$DATABASE_URL" up
```

## 4. Read in routes, write in handlers

Replace the in-memory slice from the tutorial. **Routes read:**

```go
app.Route("/", "Guestbook", func(rc fa.RouteCtx) template.HTML {
    rows, err := db.QueryContext(rc.R.Context(),
        `SELECT id, name, message FROM entries ORDER BY at DESC LIMIT 50`)
    if err != nil { return c.MustRender("ErrorState", map[string]any{"Message": "try again"}) }
    defer rows.Close()

    var items []Entry
    for rows.Next() {
        var e Entry
        if err := rows.Scan(&e.ID, &e.Name, &e.Message); err == nil {
            items = append(items, e)
        }
    }
    return c.MustRender("Entries", map[string]any{
        "Book": map[string]any{"Entries": items},
    }) + renderForm("", "", "", "")
})
```

**The form POST writes, then pushes** — same as the tutorial, with the
`INSERT` where the slice append was:

```go
var e Entry
err := db.QueryRowContext(r.Context(),
    `INSERT INTO entries (name, message) VALUES ($1, $2) RETURNING id, name, message`,
    f.Get("name"), f.Get("message"),
).Scan(&e.ID, &e.Name, &e.Message)
if err != nil { http.Error(w, "could not save", 500); return }

app.Hub().Broadcast(fa.Event{Op: "prepend", FacetID: "Entries",
    Fragment: string(c.MustRender("GuestEntry", map[string]any{"Entry": e}))})
http.Redirect(w, r, "/", http.StatusSeeOther)
```

**Event handlers** (`app.On`) work identically — `ctx.R.Context()` gives you
the request context for queries:

```go
app.On("entry.delete", func(ctx fa.Ctx) ([]fa.Event, error) {
    if _, err := db.ExecContext(ctx.R.Context(),
        `DELETE FROM entries WHERE id = $1`, ctx.Payload["entryId"]); err != nil {
        return nil, err // → 500, nothing pushed
    }
    return []fa.Event{{Op: "replace", FacetID: "GuestEntry:entry:" + ctx.Payload["entryId"], Fragment: ""}}, nil
})
```

Notice what's absent: no serializers, no DTOs, no cache invalidation. The
database **is** the state; the facet render **is** the API response.

## 5. Always use parameters, never string-build SQL

`$1, $2` placeholders (as in every example above) make SQL injection
structurally impossible. The same rule as fragments: never concatenate user
input into a query, never concatenate user input into a `Fragment` — render
through facets, which escape.

## 6. SQLite (zero-dependency dev / small deploys)

```sh
go get modernc.org/sqlite        # pure Go, no CGO — works with the distroless Dockerfile
```

```go
db, err := sql.Open("sqlite", "file:guestbook.db?_pragma=busy_timeout(5000)")
```

Queries are near-identical (placeholders are `?`). A single-binary FA app plus
a single-file database is a genuinely tiny deployment.

## 7. Production notes

- **Pool sizing:** `db.SetMaxOpenConns(n)` — start with ~2× CPU cores;
  Postgres defaults to 100 max connections total across all app instances.
- **Timeouts:** always query with a context (`rc.R.Context()` /
  `ctx.R.Context()`), so a hung query can't pin a handler forever.
- **Multi-instance:** the database is shared state, the SSE hub is not — when
  you run more than one instance, add the Redis broker so pushed fragments
  reach users connected to other instances. See
  [Deployment](Deployment.md#multiple-instances).
- **Admin:** point the built-in [Admin Panel](Admin-Panel.md) `List`/`Get`
  callbacks at the same queries and you get a database admin UI for free.
