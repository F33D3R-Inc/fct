package fa

import (
	"context"
	"html/template"
	"net/http"
	"strings"
)

// Admin is a built-in, Django-style admin panel: register your resources
// ("models") and FA auto-generates a navigable UI to browse and inspect them,
// plus a live system dashboard. It mounts on any http.ServeMux under a prefix and
// is authorization-gated (deny-by-default: you MUST set Authorize).
//
//	adm := fa.NewAdmin("Acme").
//	    Authorize(func(r *http.Request) bool { return sess.Get(r, "role") == "admin" }).
//	    WithMetrics(app.Metrics()).
//	    Resource(fa.AdminResource{
//	        Name: "users", Label: "Users", Columns: []string{"Handle", "Name"},
//	        List: func(ctx context.Context) ([]fa.AdminRow, error) { … },
//	        Get:  func(ctx context.Context, id string) ([]fa.AdminField, error) { … },
//	    })
//	adm.Mount(mux, "/admin")
//
// This is the enterprise feature React never had and Django is loved for — here it
// is part of the framework, server-rendered and self-contained.
type Admin struct {
	title     string
	resources []*AdminResource
	byName    map[string]*AdminResource
	authorize func(*http.Request) bool
	metrics   *Metrics
}

// AdminResource describes one manageable resource (a "model").
type AdminResource struct {
	Name    string                                                     // url id, e.g. "users"
	Label   string                                                     // display label
	Columns []string                                                   // list-view headers
	List    func(ctx context.Context) ([]AdminRow, error)              // rows for the list view
	Get     func(ctx context.Context, id string) ([]AdminField, error) // fields for the detail view
}

// AdminRow is one row in a resource's list view.
type AdminRow struct {
	ID    string   // record id (used for the detail link)
	Cells []string // values aligned to Columns
}

// AdminField is one labelled value in a record's detail view.
type AdminField struct {
	Label string
	Value string
}

// NewAdmin creates an admin panel titled title. Set Authorize before mounting —
// without it every request is denied (fail-safe).
func NewAdmin(title string) *Admin {
	if title == "" {
		title = "Admin"
	}
	return &Admin{title: title, byName: map[string]*AdminResource{}}
}

// Authorize sets the gate deciding who may use the admin (e.g. a session role
// check). Required: with no authorizer set, every request is denied.
func (a *Admin) Authorize(fn func(*http.Request) bool) *Admin {
	a.authorize = fn
	return a
}

// WithMetrics adds the live system dashboard (counters from app.Metrics()).
func (a *Admin) WithMetrics(m *Metrics) *Admin {
	a.metrics = m
	return a
}

// Resource registers a manageable resource. Chainable.
func (a *Admin) Resource(r AdminResource) *Admin {
	if r.Label == "" {
		r.Label = titleCase(r.Name)
	}
	rc := r
	a.resources = append(a.resources, &rc)
	a.byName[r.Name] = &rc
	return a
}

// Mount registers the admin under prefix (e.g. "/admin") on mux.
func (a *Admin) Mount(mux *http.ServeMux, prefix string) {
	prefix = "/" + strings.Trim(prefix, "/")
	h := func(w http.ResponseWriter, r *http.Request) {
		if a.authorize == nil || !a.authorize(r) {
			http.Error(w, "admin: forbidden", http.StatusForbidden)
			return
		}
		a.route(w, r, prefix)
	}
	mux.HandleFunc("GET "+prefix, h)
	mux.HandleFunc("GET "+prefix+"/", h)
}

func (a *Admin) route(w http.ResponseWriter, r *http.Request, prefix string) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	switch {
	case rest == "":
		a.dashboard(w, r, prefix)
	case strings.HasPrefix(rest, "r/"):
		parts := strings.SplitN(strings.TrimPrefix(rest, "r/"), "/", 2)
		res := a.byName[parts[0]]
		if res == nil {
			a.render(w, prefix, "Not found", "<p>No such resource.</p>")
			return
		}
		if len(parts) == 2 && parts[1] != "" {
			a.detail(w, r, prefix, res, parts[1])
		} else {
			a.list(w, r, prefix, res)
		}
	default:
		http.NotFound(w, r)
	}
}

func (a *Admin) dashboard(w http.ResponseWriter, r *http.Request, prefix string) {
	var b strings.Builder
	b.WriteString(`<h2>Dashboard</h2>`)
	if a.metrics != nil {
		b.WriteString(`<div class="adm-cards">`)
		for _, kv := range orderedMetrics(a.metrics.snapshot()) {
			b.WriteString(`<div class="adm-card"><span class="adm-card__v">`)
			b.WriteString(template.HTMLEscapeString(kv.v))
			b.WriteString(`</span><span class="adm-card__l">`)
			b.WriteString(template.HTMLEscapeString(kv.k))
			b.WriteString(`</span></div>`)
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`<h3>Resources</h3><ul class="adm-reslist">`)
	for _, res := range a.resources {
		b.WriteString(`<li><a href="` + prefix + `/r/` + res.Name + `">` + template.HTMLEscapeString(res.Label) + `</a></li>`)
	}
	b.WriteString(`</ul>`)
	a.render(w, prefix, "Dashboard", template.HTML(b.String()))
}

func (a *Admin) list(w http.ResponseWriter, r *http.Request, prefix string, res *AdminResource) {
	var b strings.Builder
	b.WriteString(`<h2>` + template.HTMLEscapeString(res.Label) + `</h2>`)
	rows, err := res.List(r.Context())
	if err != nil {
		b.WriteString(`<p class="adm-err">` + template.HTMLEscapeString(err.Error()) + `</p>`)
		a.render(w, prefix, res.Label, template.HTML(b.String()))
		return
	}
	b.WriteString(`<table class="adm-table"><thead><tr>`)
	for _, col := range res.Columns {
		b.WriteString(`<th>` + template.HTMLEscapeString(col) + `</th>`)
	}
	b.WriteString(`<th></th></tr></thead><tbody>`)
	for _, row := range rows {
		b.WriteString(`<tr>`)
		for _, cell := range row.Cells {
			b.WriteString(`<td>` + template.HTMLEscapeString(cell) + `</td>`)
		}
		b.WriteString(`<td><a href="` + prefix + `/r/` + res.Name + `/` + template.HTMLEscapeString(row.ID) + `">view</a></td></tr>`)
	}
	b.WriteString(`</tbody></table>`)
	a.render(w, prefix, res.Label, template.HTML(b.String()))
}

func (a *Admin) detail(w http.ResponseWriter, r *http.Request, prefix string, res *AdminResource, id string) {
	var b strings.Builder
	b.WriteString(`<p><a href="` + prefix + `/r/` + res.Name + `">← ` + template.HTMLEscapeString(res.Label) + `</a></p>`)
	b.WriteString(`<h2>` + template.HTMLEscapeString(res.Label) + ` · ` + template.HTMLEscapeString(id) + `</h2>`)
	if res.Get == nil {
		b.WriteString(`<p>This resource has no detail view.</p>`)
		a.render(w, prefix, res.Label, template.HTML(b.String()))
		return
	}
	fields, err := res.Get(r.Context(), id)
	if err != nil {
		b.WriteString(`<p class="adm-err">` + template.HTMLEscapeString(err.Error()) + `</p>`)
		a.render(w, prefix, res.Label, template.HTML(b.String()))
		return
	}
	b.WriteString(`<dl class="adm-fields">`)
	for _, f := range fields {
		b.WriteString(`<dt>` + template.HTMLEscapeString(f.Label) + `</dt><dd>` + template.HTMLEscapeString(f.Value) + `</dd>`)
	}
	b.WriteString(`</dl>`)
	a.render(w, prefix, res.Label, template.HTML(b.String()))
}

func (a *Admin) render(w http.ResponseWriter, prefix, page string, content template.HTML) {
	secureHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = adminTmpl.Execute(w, adminPage{
		Title: a.title, Page: page, Prefix: prefix,
		Resources: a.resources, Content: content,
	})
}

type adminPage struct {
	Title     string
	Page      string
	Prefix    string
	Resources []*AdminResource
	Content   template.HTML
}

type kv struct{ k, v string }

func orderedMetrics(m map[string]int64) []kv {
	order := []string{"conns_active", "conns_total", "events_in", "events_out", "rate_limited", "forbidden"}
	out := make([]kv, 0, len(order))
	for _, k := range order {
		out = append(out, kv{k: k, v: itoa64(m[k])})
	}
	return out
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

var adminTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Page}} · {{.Title}} Admin</title>
<style>
:root{--b:#cfd9de;--fg:#0f1419;--mut:#5b7083;--ac:#1d9bf0;--bg:#fff}
*{box-sizing:border-box}body{margin:0;font:15px/1.5 system-ui,sans-serif;color:var(--fg);background:#f7f9f9}
.adm{display:grid;grid-template-columns:240px 1fr;min-height:100vh}
.adm__side{background:var(--bg);border-right:1px solid var(--b);padding:16px}
.adm__brand{font-size:18px;margin:0 0 16px}
.adm__side nav{display:flex;flex-direction:column;gap:2px}
.adm__side a{padding:8px 12px;border-radius:8px;text-decoration:none;color:var(--fg)}
.adm__side a:hover{background:#eff3f4}
.adm__main{padding:24px;max-width:900px}
.adm-cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:12px;margin:12px 0 24px}
.adm-card{background:var(--bg);border:1px solid var(--b);border-radius:12px;padding:16px;display:flex;flex-direction:column}
.adm-card__v{font-size:28px;font-weight:800;font-variant-numeric:tabular-nums}
.adm-card__l{color:var(--mut);font-size:13px}
.adm-table{width:100%;border-collapse:collapse;background:var(--bg);border:1px solid var(--b);border-radius:12px;overflow:hidden}
.adm-table th{text-align:left;color:var(--mut);font-size:13px;padding:10px 12px;border-bottom:1px solid var(--b)}
.adm-table td{padding:10px 12px;border-bottom:1px solid var(--b)}
.adm-table a{color:var(--ac);text-decoration:none}
.adm-reslist{list-style:none;padding:0}.adm-reslist a{color:var(--ac);text-decoration:none}
.adm-fields{display:grid;grid-template-columns:200px 1fr;gap:0;background:var(--bg);border:1px solid var(--b);border-radius:12px;overflow:hidden}
.adm-fields dt{padding:10px 12px;color:var(--mut);border-bottom:1px solid var(--b);font-weight:600}
.adm-fields dd{padding:10px 12px;margin:0;border-bottom:1px solid var(--b)}
.adm-err{color:#f4212e}
</style></head>
<body><div class="adm">
<aside class="adm__side">
<h1 class="adm__brand">{{.Title}}</h1>
<nav>
<a href="{{.Prefix}}">Dashboard</a>
{{range .Resources}}<a href="{{$.Prefix}}/r/{{.Name}}">{{.Label}}</a>{{end}}
</nav>
</aside>
<main class="adm__main">{{.Content}}</main>
</div></body></html>`))
