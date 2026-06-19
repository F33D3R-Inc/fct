# Data Modeling

How durable data works in Facet: entities, the type system, relations, queries,
indexes, and migrations.

## Entities are tables

An `entity` is a durable, shared record type — one real Postgres table. Always
give it an `id: int` (the primary key).

```
entity Post:
    id: int
    author: text
    body: text
    likes: int
    created: int
```

Each field becomes a typed column: `int`/`money`/`date` → `BIGINT`, `text` →
`TEXT`, `bool` → `BOOLEAN`. Entities live on the **server** by the
[placement calculus](Placement-Calculus.md) — they are durable and shared, so
they cannot be anything else.

## The type system

| Type | Column | Notes |
|---|---|---|
| `int` | `BIGINT` | |
| `text` | `TEXT` | |
| `bool` | `BOOLEAN` | |
| `money` | `BIGINT` | integer minor units (store cents, not floats) |
| `date` | `BIGINT` | unix seconds; pair with `now()` |
| `EnumName` | `TEXT` | a [closed text type](Language-Reference.md#enum) |
| `EntityName` | `BIGINT` | a [relation](#relations) (foreign key) |

**Modifiers:**

- `T?` — nullable column.
- `@secret` — encrypted at rest (see
  [Security](Authorization-and-Security.md#secret-field-encryption)).

## Relations

A field typed as another entity is a relation. The column stores the referenced
row's **id** as a foreign key with **`ON DELETE CASCADE`**.

```
entity User:
    id: int
    name: text

entity Message:
    id: int
    to: User              # relation -> stores a User id
    body: text
    sent: date
```

- **Write** a relation by passing the id: `add Message { to: 1, body: draft, sent: now() }`.
- **Read across** it with a nested lookup: `User(m.to).name`.
- **Delete cascades:** removing a `User` removes their `Message`s — in the
  database *and* the live in-memory working set, fanned out over SSE.
- **Reverse lookups** (a user's messages) are indexed.

## Queries

The `for` node (in a view) and the JSON entity endpoint share one query model:
**filter → sort → paginate.**

```
for m in Message where m.to == 1 by sent desc limit 20:
    box:
        text "to {User(m.to).name}: {m.body}"
```

| Clause | Meaning |
|---|---|
| `where <cond>` | row filter; `field == value` and comparisons |
| `by <field> [asc\|desc]` | sort (default ascending) |
| `limit <n>` | maximum rows |

In the JSON API these compile to **indexed `SELECT`s with keyset cursor
pagination** — a large table is never loaded whole. See
[Projections & the API](Projections-and-API.md#querying-entities).

## Indexes

You do not declare indexes. The compiler flags every field it sees **filtered**
(`where`), **ordered** (`by`), or used as a **relation**, and the store builds a
real database index for it. Reads stay sub-linear as the table grows past what
fits in memory.

## Migrations

The schema is reconciled from the IR:

```sh
facet migrate app.fct           # apply pending changes
facet migrate app.fct --plan    # dry-run: print what would change
```

- Changes are **additive** — `CREATE TABLE`, `ADD COLUMN`, `ADD CONSTRAINT`,
  `CREATE INDEX` — so they are safe to run with no downtime.
- Every applied statement is versioned in the `facet_migrations` table.
- The same reconciliation runs **automatically on `facet run` startup**, so a
  deploy is self-migrating; run `facet migrate` explicitly when you want to see
  or gate the plan in CI.

> **Upgrading from v1.0.0 (JSON-document storage)?** v1.1.0+ uses columnar
> tables. Point at a fresh database, or migrate old rows out of the legacy
> `data` column, then `facet migrate` to build the typed schema.

## Transactions

A multi-statement action commits **atomically**: all of its writes ride one
transaction, and any failure rolls the whole action back. The database never
holds a half-applied action.

## Seeding and snapshots

- **`facet seed app.fct [data.json]`** loads fixture rows (defaults to
  `app.seed.json`; `--dry` keeps them in memory).
- **`facet backup`** / **`facet restore`** write and replay a logical snapshot.

See the [CLI Reference](CLI-Reference.md).

→ Next: **[Actions & Logic](Actions-and-Logic.md)**.
