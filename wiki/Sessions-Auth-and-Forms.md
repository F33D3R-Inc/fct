# Sessions, Auth & Forms

Everything for logged-in users: who they are (sessions), what they may do
(guards + `who:`), and how data gets in (forms).

## Sessions

Signed-cookie sessions are built in: HMAC-signed with the app key,
HttpOnly + SameSite, and a tampered cookie reads as **no session at all**.

```go
sess := app.Sessions()              // or fa.NewSessions(key, opts…)
app.Identify(sess.Identity)         // "uid" from the session = the user's SSE identity

// login (after YOU verify credentials — see below):
sess.Save(w, map[string]string{"uid": user.ID, "role": user.Role})

// logout:
sess.Clear(w)

// read anywhere you have the request:
uid  := sess.Identity(r)            // shorthand for Get(r, "uid")
role := sess.Get(r, "role")
```

Options: `fa.SessionName("…")`, `fa.SessionMaxAge(30*24*time.Hour)`,
`fa.SessionInsecure()` (dev over plain HTTP only).

`app.Identify` is the keystone: it tells the framework which user owns each
SSE connection, which is what makes `EmitTo(userID, …)` and `ctx.Identity`
work. Wire it once at startup.

### Verifying credentials (your job, with stdlib + one import)

FA doesn't ship a user table. Store users yourself
([Working with Databases](Working-with-Databases.md)) and hash passwords with
a real KDF:

```go
import "golang.org/x/crypto/argon2"   // or golang.org/x/crypto/bcrypt

// registering:  store argon2id(salt, password)
// logging in:   recompute and constant-time compare (subtle.ConstantTimeCompare)
```

A login flow is a normal form POST: validate → look up user → compare hash →
`sess.Save` → redirect. (OIDC/SSO guidance is tracked in
[ENTERPRISE.md](../ENTERPRISE.md).)

## Authorization — three layers

### 1. `app.Guard` — gate events before the handler runs

```go
app.Guard("post.delete", func(ctx fa.Ctx) bool {
    return ctx.Identity != "" && ctx.Identity == ownerOf(ctx.Payload["postId"])
})
```

A failing guard responds 403 and the handler never runs. Guard every event
that mutates or reads private state — `ctx.Identity` is the session-derived
user ("" = anonymous), and it cannot be spoofed by the client.

### 2. `who:` — declarative, render-time enforcement

In the facet:

```
facet Profile:
    who:
        require: is_member
        redact user.email unless is_self
    what:
        user: User
    looks:
        …
```

In the app, implement the named policies and render with a view:

```go
app.Route("/u/:handle", "Profile", func(rc fa.RouteCtx) template.HTML {
    u := db.UserByHandle(rc.Param("handle"))
    return c.RenderFor(rc.View(), "Profile", u)   // checks require, strips redact
})
```

A `who:`-protected facet **refuses** the plain `Render` path entirely, and
redacting a field that doesn't exist fails closed (render errors rather than
leaks). In event handlers, `ctx.View()` gives the same view. `fct audit
profile.fct` prints what's required and redacted.

### 3. Delivery scoping — who receives pushes

Covered in [Realtime Patterns](Realtime-Patterns.md); the rule: handler
returns go only to the acting connection; wider delivery (`EmitTo`,
`EmitChannel`, `Broadcast`) is explicit, and channels are **deny-by-default**
(`app.ChannelAuth`).

## Forms

Two transport shapes, one validator:

- **Full submissions** (create, login, settings) → a regular
  `<form method="post" action="/sign">` handled on your mux. Build the
  validator with `fa.NewForm(r)`.
- **Event payloads** (a `data-action` element's `data-*` attributes) → build
  it with `fa.FormFromPayload(ctx.Payload)`.

```go
f := fa.NewForm(r)
f.Required("email", "Email is required").Email("email", "Enter a valid email")
f.Required("password", "Required").MinLen("password", 8, "At least 8 characters")
f.Confirm("password", "confirm", "Passwords must match")
f.Matches("handle", handleRe, "Letters, numbers, _ only")
f.Check("age", userIsAdult, "Must be 18+")          // arbitrary predicate

if !f.Valid() {
    // f.Errors is map[field]firstError — re-render the form facet with it
}
name := f.Get("name")                                // trimmed value
msg  := f.Error("email")                             // first error for a field ("" if ok)
file, hdr, err := f.File("avatar")                   // multipart upload
```

The validator accumulates **one error per field** (first failure wins). Pass
each error to the form facet as its own `str` field (`email_error: str` in
`what:`, filled from `f.Error("email")`) — or feed the stdlib's `FieldError`
facet:

```
        <FieldError message="{email_error}"/>
```

### The re-render-with-errors pattern

```go
mux.HandleFunc("POST /signup", func(w http.ResponseWriter, r *http.Request) {
    f := fa.NewForm(r)
    f.Required("email", "Required").Email("email", "Invalid email")
    if !f.Valid() {
        w.WriteHeader(http.StatusUnprocessableEntity)
        page := c.MustRender("SignupForm", map[string]any{
            "Email": f.Get("email"), "EmailError": f.Error("email"),
        })
        _, _ = w.Write([]byte(app.Page(page, fa.ShellOptions{Title: "Sign up"})))
        return
    }
    // …create the user, sess.Save, redirect…
})
```

The user's input is preserved, errors render next to their fields, everything
stays server-side.
