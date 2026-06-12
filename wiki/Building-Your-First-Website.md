# Building Your First Website

The full tour, Django-tutorial style. We'll build a **guestbook**: a multi-page
site where visitors sign their name and a message, entries appear **live on
every open browser** without a reload, and signing requires a valid form.

You'll touch every part of the framework: facets, routes, events, forms, and
live fan-out. Start from a fresh scaffold:

```sh
fct new guestbook && cd guestbook
```

State lives in memory for this tutorial so you can focus on the framework;
[Working with Databases](Working-with-Databases.md) swaps it for Postgres with
the handlers almost unchanged.

## 1. Model the data (plain Go)

There is no ORM and no model layer — your data is Go. At the top of `main.go`:

```go
type Entry struct {
    ID      string
    Name    string
    Message string
}

var (
    mu      sync.Mutex
    entries []Entry
)
```

## 2. Write the facets

A facet is a server-rendered HTML fragment plus the data it needs. Create
`facets/entry.fct` — one guestbook entry:

```
facet GuestEntry:
    what:
        entry: Entry
    looks:
        <div class="entry">
            <strong>{entry.name}</strong>
            <p>{entry.message}</p>
        </div>
```

`what:` declares the data contract. `Entry` (capitalized) means "one of your
Go structs" — fields are matched by name, and a typo in `looks:` (say,
`{entry.mesage}`) is a **compile error naming the facet and field**, not a
blank spot at runtime.

Now `facets/entries.fct` — the list container. Lists travel inside a struct
(here `Book`, with an `Entries` field), and we pin the facet-id to a stable
name because we'll push new entries into this exact node later:

```
facet Entries:
    facet-id: "Entries"
    what:
        book: Book
    looks:
        <section class="entries">
            for e in book.entries:
                <GuestEntry entry="{e}"/>
        </section>
```

Two things to notice: `for` loops over a field of the struct, and
`<GuestEntry .../>` is a **child facet call** — facets compose like
components. (Without the `facet-id:` override the id would be auto-derived
per-instance from the first custom-typed field; a singleton list wants one
stable address.)

Finally `facets/sign_form.fct` — the form (a regular HTML form; full-page
actions like submissions POST to a route, while micro-interactions use
`data-action` — more on that split in section 5):

```
facet SignForm:
    what:
        name: str
        message: str
        name_error: str
        message_error: str
    looks:
        <form class="sign" method="post" action="/sign">
            <label>Name <input name="name" value="{name}"></label>
            if name_error:
                <p class="error">{name_error}</p>
            <label>Message <textarea name="message">{message}</textarea></label>
            if message_error:
                <p class="error">{message_error}</p>
            <button type="submit">Sign the guestbook</button>
        </form>
```

(Field names are snake_case in FDL and map to idiomatic Go names —
`name_error` → `NameError`.)

All interpolated values are HTML-escaped automatically (Go `html/template`
under the hood) — a visitor whose "name" is `<script>…</script>` renders as
inert text.

## 3. Pages and routing

In `main.go`, register the pages. `app.Route` takes a URL pattern, a page
title, and a function that returns the page's HTML content (the framework
wraps it in the document shell — you never write `<html>`):

```go
renderForm := func(name, message, nameErr, msgErr string) template.HTML {
    return c.MustRender("SignForm", map[string]any{
        "Name": name, "Message": message,
        "NameError": nameErr, "MessageError": msgErr,
    })
}
renderEntries := func() template.HTML {
    mu.Lock()
    items := append([]Entry(nil), entries...)
    mu.Unlock()
    return c.MustRender("Entries", map[string]any{
        "Book": map[string]any{"Entries": items},
    })
}

app.Route("/", "Guestbook", func(rc fa.RouteCtx) template.HTML {
    return renderEntries() + renderForm("", "", "", "")
})

app.Route("/about", "About", func(rc fa.RouteCtx) template.HTML {
    return c.MustRender("About", nil)
})

app.Mount(mux)       // /sse, /events, /manifest.json, /fa-runtime.js
app.MountRouter(mux, fa.ShellOptions{Title: "Guestbook", CSS: template.CSS(std.CSS) + appCSS})
```

Patterns can capture parameters: `app.Route("/entry/:id", …)` then
`rc.Param("id")`. Links marked `<a href="/about" data-nav>` navigate
**without a page reload** — the SSE connection and every live facet survive
across pages.

## 4. Handle the form POST

The form POSTs to `/sign` like any classic web app. Register a plain handler
on the mux, validate with `fa.NewForm`, and either re-render the form with
errors or save and redirect:

```go
mux.HandleFunc("POST /sign", func(w http.ResponseWriter, r *http.Request) {
    f := fa.NewForm(r)
    f.Required("name", "Tell us who you are").MaxLen("name", 40, "40 characters max")
    f.Required("message", "Say something!").MaxLen("message", 280, "Keep it under 280")

    if !f.Valid() {
        // Re-render the page with the errors and the user's input preserved.
        w.WriteHeader(http.StatusUnprocessableEntity)
        page := renderEntries() +
            renderForm(f.Get("name"), f.Get("message"), f.Error("name"), f.Error("message"))
        _, _ = w.Write([]byte(app.Page(page, fa.ShellOptions{Title: "Guestbook"})))
        return
    }

    e := Entry{ID: fmt.Sprint(time.Now().UnixNano()), Name: f.Get("name"), Message: f.Get("message")}
    mu.Lock()
    entries = append([]Entry{e}, entries...)
    mu.Unlock()

    // The live part: push the new entry into the Entries facet on EVERY open
    // page. "prepend" inserts inside the element with data-facet-id="Entries".
    app.Hub().Broadcast(fa.Event{
        Op:       "prepend",
        FacetID:  "Entries",
        Fragment: string(c.MustRender("GuestEntry", map[string]any{"Entry": e})),
    })

    http.Redirect(w, r, "/", http.StatusSeeOther)
})
```

Open the site in two browser windows and sign in one — the entry appears in
both, instantly. The pushed fragment is HMAC-signed by the framework and
verified by the runtime before it touches the DOM.

> `Broadcast` is for public content. For per-user or per-group delivery
> (notifications, DMs, dashboards) use `EmitTo` / `EmitChannel` — see
> [Realtime Patterns](Realtime-Patterns.md).

## 5. Micro-interactions: `data-action` events

Form POSTs are for full submissions. For small in-place interactions — like,
follow, vote, dismiss — you wire an element with `data-action` and handle the
event on the server. Add a wave button to `entry.fct`:

```
            <button data-action="entry.wave" data-entry-id="{entry.id}">👋 wave</button>
```

When clicked, the runtime POSTs `{type: "entry.wave", payload: {entryId: …}}`
to `/events` (every `data-*` attribute on the element becomes a payload field,
camelCased). One handler per event type:

```go
app.On("entry.wave", func(ctx fa.Ctx) ([]fa.Event, error) {
    id := ctx.Payload["entryId"]
    // …change state, then return the DOM mutations to push back…
    return []fa.Event{{Op: "replace", FacetID: "GuestEntry:entry:" + id,
        Fragment: string(c.MustRender("GuestEntry", map[string]any{"Entry": find(id)}))}}, nil
})
```

Events returned from a handler go **only to the clicking user's connection**
by default — no accidental cross-user leaks. Note `GuestEntry`'s facet-id is
per-instance (`GuestEntry:entry:<id>`, auto-derived from its first
custom-typed `what:` field), so the replace updates exactly one card.

## 6. Login (the short version)

Sessions are built in — signed cookies, tamper-rejected:

```go
sess := app.Sessions()
app.Identify(sess.Identity) // logged-in user id becomes the SSE delivery identity

// on successful login:  sess.Save(w, map[string]string{"uid": user.ID})
// on logout:            sess.Clear(w)
// in any handler:       sess.Get(r, "uid")   /   ctx.Identity in event handlers
```

Gate events with `app.Guard("entry.delete", func(ctx fa.Ctx) bool { return ctx.Identity == ownerOf(ctx.Payload["entryId"]) })`,
and protect rendered fields with the `who:` block. The full story — password
hashing, guards, `who:` policies, redaction — is in
[Sessions, Auth & Forms](Sessions-Auth-and-Forms.md).

## 7. Run it

```sh
fct dev        # rebuilds on every .fct save
```

You now have a multi-page, live-updating, validated, XSS-safe website — and
you wrote zero JavaScript, zero API endpoints, and zero client state.

## Where to next

- Real persistence: **[Working with Databases](Working-with-Databases.md)**
- Ship it: **[Deployment](Deployment.md)**
- Don't hand-roll UI: **[Standard Library](Standard-Library.md)** (PostCard,
  Composer, Modal, Toast… 229 facets)
- Test it: **[Testing](Testing.md)**
