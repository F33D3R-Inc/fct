# Admin Panel

The Django feature React never had, built in: register your resources and FA
generates a navigable, server-rendered admin — a dashboard with live system
metrics, a list view per resource, and a detail view — under any URL prefix.

**Deny-by-default:** with no `Authorize` callback, every request is refused.

## Minimal setup

```go
adm := fa.NewAdmin("Acme").
    Authorize(func(r *http.Request) bool {
        return sess.Get(r, "role") == "admin"     // YOU decide who's an admin
    }).
    WithMetrics(app.Metrics()).                   // live counters on the dashboard
    Resource(fa.AdminResource{
        Name:    "users",
        Label:   "Users",
        Columns: []string{"Handle", "Name"},
        List: func(ctx context.Context) ([]fa.AdminRow, error) {
            rows, err := db.QueryContext(ctx, `SELECT id, handle, name FROM users ORDER BY id`)
            if err != nil { return nil, err }
            defer rows.Close()
            var out []fa.AdminRow
            for rows.Next() {
                var id, handle, name string
                if err := rows.Scan(&id, &handle, &name); err != nil { return nil, err }
                out = append(out, fa.AdminRow{ID: id, Cells: []string{handle, name}})
            }
            return out, rows.Err()
        },
        Get: func(ctx context.Context, id string) ([]fa.AdminField, error) {
            var handle, name, email string
            err := db.QueryRowContext(ctx,
                `SELECT handle, name, email FROM users WHERE id = $1`, id,
            ).Scan(&handle, &name, &email)
            if err != nil { return nil, err }
            return []fa.AdminField{
                {Label: "Handle", Value: handle},
                {Label: "Name", Value: name},
                {Label: "Email", Value: email},
            }, nil
        },
    })

adm.Mount(mux, "/admin")
```

Open `/admin` as an authorized user: dashboard (with the app's live event/
connection metrics if you passed `WithMetrics`), a "Users" section, list view,
click-through to detail.

## Notes

- You provide `List`/`Get` — any data source works (Postgres above, but a
  slice, an API, a KV store are fine). The admin renders; it doesn't own your
  data model.
- Add one `Resource(…)` call per resource.
- It's all server-rendered FA — no separate admin frontend to deploy, and the
  layout is responsive (sidebar collapses on narrow windows).
- Keep the `Authorize` check strict; pair it with the session role you set at
  login ([Sessions, Auth & Forms](Sessions-Auth-and-Forms.md)).
