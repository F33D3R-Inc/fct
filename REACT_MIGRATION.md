# Coming from React

FA inverts React: there is no client app holding a copy of server state. The
server renders HTML and pushes updates; the browser just displays. Most React
concepts have a server-side counterpart — and several disappear entirely.

## Concept map

| React | Facet Architecture |
|---|---|
| Component | **Facet** (`.fct`: `what` props, `looks` template) |
| `props` | the `what:` block (typed) |
| `{expr}` in JSX | `{expr}` in `looks` (paths, calls, comparisons, arithmetic) |
| `<Child prop={x}/>` | `<Child prop="{x}"/>` (composition) |
| `children` / slots | `slot:` + block form `<Card> … </Card>` |
| `useState` / `useReducer` | **server state** (a Go variable / your DB) — gone from the client |
| `useEffect` / data fetching | server resolves data before render — gone |
| Event handler (`onClick`) | `data-action="x"` → `app.On("x", handler)` on the server |
| Context | pass via `what:` props, or resolve server-side |
| `dangerouslySetInnerHTML` | auto-escaped by default; a marked raw boundary (planned) |
| Conditional rendering | `if cond:` / inline `{ if cond } … { end }` |
| Lists (`.map`) | `for x in xs:` |
| Optimistic UI (manual) | `data-fa-optimistic="class"` (declarative, auto-reverts) |
| Router + data loaders | server routes + `app.HandlePage`; `data-nav` links |
| Redux / React Query / SWR | **deleted** — there is no client cache to sync |
| Error boundaries | handlers return errors; server renders the truth |
| `key` for reconciliation | `data-facet-id` (derived from your data) — surgical, no diff |

## What you stop writing

- State management, caches, and the bugs that come with them.
- An API layer (REST/GraphQL/tRPC) — the server renders HTML directly.
- Loading-state choreography — the server knows what it knows.
- A bundler/hydration pipeline — the client runtime is one fixed ~8 KB file.

## A like button, before & after

```jsx
// React
function Like({ post }) {
  const [liked, setLiked] = useState(post.liked);
  const [n, setN] = useState(post.likes);
  return <button onClick={async () => {
    setLiked(!liked); setN(n + (liked ? -1 : 1));         // optimistic
    const r = await fetch(`/api/like/${post.id}`, {method:'POST'});
    const d = await r.json(); setLiked(d.liked); setN(d.likes); // reconcile
  }} className={liked ? 'active' : ''}>{n}</button>;
}
```

```
# FA — facet
facet LikeButton:
    what:
        post: Post
        liked: bool
    looks:
        <button class="like{ if liked } active{ end }" data-action="post.like"
                data-fa-optimistic="active" data-post-id="{post.id}">{post.like_count}</button>
```
```go
// FA — handler (server owns the state)
app.On("post.like", func(c fa.Ctx) ([]fa.Event, error) {
    p := toggleLike(c.Payload["postId"])     // your DB
    return []fa.Event{{Op:"replace", FacetID:"LikeButton:post:"+p.ID, Fragment: render(p)}}, nil
})
```

The optimistic flip is one attribute; reconciliation is automatic (the server's
replace is the truth). No `useState`, no fetch, no JSON, no rollback code.

## Migrate incrementally

FA is server-rendered HTML — it coexists with anything. Start by replacing one
page or one widget; FA owns its DOM subtree via `data-facet-id`.
