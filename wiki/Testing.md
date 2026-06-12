# Testing

`github.com/F33D3R-Inc/fct/fatest` is to FA what `httptest` is to `net/http`:
test facets and handlers **with no server, no browser, no network**. Because
all application logic lives on the server, your whole app is unit-testable in
plain `go test`.

## Render tests — "does this facet produce the right HTML?"

```go
func TestLikeButton(t *testing.T) {
    src, _ := os.ReadFile("facets/like_button.fct")

    html := fatest.Render(t, string(src), "LikeButton", map[string]any{
        "Post": map[string]any{"ID": "p1"}, "Count": 5, "Liked": true,
    })

    if !strings.Contains(html, "♥ 5") {
        t.Errorf("liked render wrong: %s", html)
    }
    if !strings.Contains(html, `data-facet-id="LikeButton:post:p1"`) {
        t.Error("missing per-instance facet id")
    }
}
```

`Render` compiles the source and renders in one call — compile errors fail the
test with the compiler's message, so render tests double as FDL regression
tests.

For `who:`-protected facets, use `RenderFor` with a `fa.View` to test both the
allow and deny paths (and that redacted fields are absent from the output):

```go
html, err := fatest.RenderFor(t, c, fa.View{Identity: "ada"}, "Profile", data)
```

## Handler tests — "does this event do the right thing?"

`Dispatch` runs an event through the real router — guards included — and
returns the events the actor would receive:

```go
func TestLike(t *testing.T) {
    app := buildApp(t) // your fa.New + On(...) wiring, factored for tests

    events := fatest.Dispatch(t, app, "post.like", map[string]string{"postId": "p1"})

    fatest.AssertFragment(t, events, "post:p1", "active", "♥")
}
```

- `Dispatch` — as an anonymous actor.
- `DispatchAs(t, app, type, payload, "user-42")` — as a logged-in identity;
  use it to test guards both ways:

```go
func TestDeleteRequiresOwner(t *testing.T) {
    app := buildApp(t)
    if evs := fatest.Dispatch(t, app, "post.delete", payload); len(evs) != 0 {
        t.Error("anonymous delete should be rejected")
    }
    fatest.DispatchAs(t, app, "post.delete", payload, "owner-id") // should succeed
}
```

`AssertFragment(t, events, idPart, wants…)` finds the event whose `FacetID`
contains `idPart` and asserts the fragment contains every `wants` string.

## Patterns

- **Factor app construction** into `func buildApp(...deps) *fa.App` so tests
  and `main` share the exact wiring (routes, guards, handlers).
- **Inject the database.** Handlers that take a `*sql.DB` (or a small
  interface) can run against a test Postgres
  (`docker run … postgres` in CI) or in-memory SQLite.
- **`fct check facets/`** in CI catches FDL errors (bad expressions, unknown
  child facets, wrong props) without running anything.
- The framework's own suite is the reference: `go test ./...` at the repo
  root; `go test ./fa -bench .` for the performance baselines.
