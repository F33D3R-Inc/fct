# Getting Started

This walks you from an empty directory to a running, multi-user app with auth,
data, live updates, and a JSON API — in one file.

## 1. Scaffold a project

```sh
facet new myapp
cd myapp
```

This writes a starter `app.fct`, a README, a `Dockerfile` + `docker-compose.yml`,
an `.env.example`, and seed/test fixtures.

## 2. Run it with hot reload (no database needed)

```sh
facet dev app.fct
```

Open <http://localhost:7373>. Edit `app.fct` and the page reloads itself.
`facet dev` keeps all data in memory, so it is perfect for learning and
iterating. The first account you sign up becomes the **admin**.

## 3. Read the starter app

The scaffold is a tiny social feed. Here is the shape of it:

```
app Starter:
    auth

    entity Post:
        id: int
        author: text
        body: text
        created: int

    state username: text = "" @client
    state password: text = "" @client
    state draft: text = "" @client

    derive postCount: int = count(Post)

    policy mine(id: int):
        actor == Post(id).author

    action post(body: text):
        add Post { author: actor, body: body, created: now() }

    action remove(id: int):
        requires mine(id)
        remove Post(id)

    view Home at "/":
        box:
            text "{postCount} posts"
            if actor == "guest":
                input bind username placeholder "username"
                input bind password placeholder "password"
                button "sign up" -> signup(username, password)
                button "log in" -> login(username, password)
            if actor != "guest":
                text "signed in as {actor} ({role})"
                button "log out" -> logout
                input bind draft placeholder "what's happening?"
                button "post" -> post(draft)
            for p in Post by created desc limit 50:
                box:
                    text "{p.author}: {p.body}"
                    button "delete" -> remove(p.id)
```

Notice what you **didn't** write:

- You never said `post` runs on the server — but it calls `now()` (the server
  clock), so the compiler placed it on the authority.
- You never said `draft` lives in the browser — but it's `@client`, so it does,
  and typing into it never touches the network.
- `remove` has `requires mine(id)`: the authority enforces that you can only
  delete your own post, no matter what a client sends.

This is the **[placement calculus](Placement-Calculus.md)** at work.

## 4. See the other projections

The same graph is already three things. With the app running:

```sh
# the application contract (entities + actions + types)
curl localhost:7373/api

# the durable rows
curl localhost:7373/api/Post

# invoke an action
curl -X POST localhost:7373/api/post -H 'content-type: application/json' \
  -d '{"args":["hello from curl"]}'
```

Open the site in two tabs and post in one — the other updates live over SSE.

## 5. Test your app's behavior

```sh
facet test app.fct
```

This runs the behavior tests in `app.test.json` against an in-memory instance —
no database, deterministic. See **[CLI Reference → test](CLI-Reference.md#facet-test)**.

## 6. Run it for real

Point at Postgres, set a secret, and serve:

```sh
export FACET_DATABASE_URL=postgres://user:pw@localhost:5432/yourdb
export FACET_SECRET=$(facet config --gen-secret)
facet run app.fct
```

`facet run` reconciles the database schema on startup (or run
`facet migrate app.fct` explicitly), serves the hardened web + API projections,
and shuts down gracefully on SIGTERM.

## Where to go next

- **[Data Modeling](Data-Modeling.md)** — types, relations, queries.
- **[Actions & Logic](Actions-and-Logic.md)** — how behavior works.
- **[Views & UI](Views-and-UI.md)** — build richer pages.
- **[Authorization & Security](Authorization-and-Security.md)** — lock it down.
- **[Operations](Operations.md)** — deploy and scale it.
