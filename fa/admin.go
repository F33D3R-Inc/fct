package fa

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// Admin is FA's built-in admin panel — and it is built the Facet Architecture way:
// rendered from compiled facets (so every part has a data-facet-id and flows
// through the neutral tree to native clients), served through the Playground shell
// (so the runtime + SSE are present), navigable client-side without reloads, and
// LIVE — the metrics dashboard updates over a scoped SSE channel. It is not a
// classic server-rendered admin; it dogfoods the framework.
//
//	adm := fa.NewAdmin("Acme").
//	    Authorize(func(r *http.Request) bool { return sess.Get(r, "role") == "admin" }).
//	    WithMetrics(app.Metrics()).
//	    Resource(fa.AdminResource{Name: "users", Label: "Users", Columns: []string{"Handle"},
//	        List: ..., Get: ...})
//	adm.Mount(app, mux, "/admin")
//	adm.StartLiveMetrics(app, 2*time.Second) // cards update live
//
// Deny-by-default: with no Authorize set every request is refused.
type Admin struct {
	title     string
	prefix    string
	resources []*AdminResource
	byName    map[string]*AdminResource
	authorize func(*http.Request) bool
	metrics   *Metrics
	c         *Compiled // the admin's own compiled facets
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
	ID    string
	Cells []string
}

// AdminField is one labelled value in a record's detail view.
type AdminField struct {
	Label string
	Value string
}

// AdminChannel is the SSE channel the live dashboard subscribes to. Authorize it
// for admins via App.ChannelAuth if you call StartLiveMetrics.
const AdminChannel = "fa.admin"

// adminMetricsID is the facet-id the live metrics block re-renders into.
const adminMetricsID = "AdminMetrics"

// NewAdmin creates an admin panel. The admin's own facets are compiled here.
func NewAdmin(title string) *Admin {
	if title == "" {
		title = "Admin"
	}
	c, err := Compile(adminFDL)
	if err != nil {
		panic("fa: admin facets failed to compile: " + err.Error()) // static FDL — a bug if it fails
	}
	return &Admin{title: title, byName: map[string]*AdminResource{}, c: c}
}

func (a *Admin) Authorize(fn func(*http.Request) bool) *Admin { a.authorize = fn; return a }
func (a *Admin) WithMetrics(m *Metrics) *Admin                { a.metrics = m; return a }

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

// Mount registers the admin under prefix on mux, served through app's Playground
// shell (so the runtime, SSE, and client-side navigation all work).
func (a *Admin) Mount(app *App, mux *http.ServeMux, prefix string) {
	a.prefix = "/" + strings.Trim(prefix, "/")
	h := func(w http.ResponseWriter, r *http.Request) {
		if a.authorize == nil || !a.authorize(r) {
			http.Error(w, "admin: forbidden", http.StatusForbidden)
			return
		}
		title, content := a.content(r)
		// Three response shapes, exactly like the app router: full shell, client-nav
		// fragment, or a native neutral tree.
		switch {
		case isNative(r):
			writeNative(w, title, content)
		case isNav(r):
			writeNav(w, title, content)
		default:
			secureHeaders(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(app.Page(content, ShellOptions{Title: title + " · " + a.title, CSS: adminCSS})))
		}
	}
	mux.HandleFunc("GET "+a.prefix, h)
	mux.HandleFunc("GET "+a.prefix+"/", h)
}

// StartLiveMetrics pushes a fresh metrics block to admin viewers every interval
// over the scoped AdminChannel (authorize it for admins via app.ChannelAuth). The
// dashboard subscribes automatically (data-fa-subscribe), so the cards update with
// no reload — the signature FA capability, in the admin itself.
func (a *Admin) StartLiveMetrics(app *App, interval time.Duration) {
	if a.metrics == nil {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			frag := a.c.MustRender("AdminMetrics", a.metricsData())
			app.Hub().EmitChannel(AdminChannel, Event{Op: "replace", FacetID: adminMetricsID, Fragment: string(frag)})
		}
	}()
}

// content renders the page for the current path and returns (title, html). The
// html is composed from the admin's facets, wrapped in the AdminLayout facet.
func (a *Admin) content(r *http.Request) (string, template.HTML) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, a.prefix), "/")
	switch {
	case rest == "":
		return "Dashboard", a.wrap(a.dashboard())
	case strings.HasPrefix(rest, "r/"):
		parts := strings.SplitN(strings.TrimPrefix(rest, "r/"), "/", 2)
		res := a.byName[parts[0]]
		if res == nil {
			return "Not found", a.wrap("<p>No such resource.</p>")
		}
		if len(parts) == 2 && parts[1] != "" {
			return res.Label, a.wrap(a.detail(r, res, parts[1]))
		}
		return res.Label, a.wrap(a.list(r, res))
	}
	return a.title, a.wrap("<p>Not found.</p>")
}

// wrap composes content into the AdminLayout facet (sidebar nav + main).
func (a *Admin) wrap(content template.HTML) template.HTML {
	return a.c.MustRender("AdminLayout", map[string]any{
		"Nav":     a.navData(),
		"Content": content, // template.HTML → not re-escaped
	})
}

func (a *Admin) dashboard() template.HTML {
	var parts []string
	parts = append(parts, `<h2>Dashboard</h2>`)
	if a.metrics != nil {
		parts = append(parts, string(a.c.MustRender("AdminMetrics", a.metricsData())))
	}
	parts = append(parts, `<h3>Resources</h3>`)
	parts = append(parts, string(a.c.MustRender("AdminResList", map[string]any{"Nav": a.navData()})))
	return template.HTML(strings.Join(parts, ""))
}

func (a *Admin) list(r *http.Request, res *AdminResource) template.HTML {
	head := template.HTML(`<h2>` + template.HTMLEscapeString(res.Label) + `</h2>`)
	rows, err := res.List(r.Context())
	if err != nil {
		return head + template.HTML(`<p class="adm-err">`+template.HTMLEscapeString(err.Error())+`</p>`)
	}
	data := map[string]any{"Columns": res.Columns}
	rowsData := make([]any, 0, len(rows))
	for _, row := range rows {
		rowsData = append(rowsData, map[string]any{
			"ID": row.ID, "Cells": row.Cells,
			"Href": a.prefix + "/r/" + res.Name + "/" + row.ID,
		})
	}
	data["Rows"] = rowsData
	return head + a.c.MustRender("AdminTable", map[string]any{"Table": data})
}

func (a *Admin) detail(r *http.Request, res *AdminResource, id string) template.HTML {
	back := template.HTML(`<p><a href="` + a.prefix + `/r/` + res.Name + `" data-nav>← ` + template.HTMLEscapeString(res.Label) + `</a></p>`)
	head := template.HTML(`<h2>` + template.HTMLEscapeString(res.Label) + ` · ` + template.HTMLEscapeString(id) + `</h2>`)
	if res.Get == nil {
		return back + head + `<p>No detail view.</p>`
	}
	fields, err := res.Get(r.Context(), id)
	if err != nil {
		return back + head + template.HTML(`<p class="adm-err">`+template.HTMLEscapeString(err.Error())+`</p>`)
	}
	fieldsData := make([]any, 0, len(fields))
	for _, f := range fields {
		fieldsData = append(fieldsData, map[string]any{"Label": f.Label, "Value": f.Value})
	}
	return back + head + a.c.MustRender("AdminDetail", map[string]any{"Record": map[string]any{"Fields": fieldsData}})
}

func (a *Admin) navData() map[string]any {
	items := []any{map[string]any{"Label": "Dashboard", "Href": a.prefix}}
	for _, res := range a.resources {
		items = append(items, map[string]any{"Label": res.Label, "Href": a.prefix + "/r/" + res.Name})
	}
	return map[string]any{"Title": a.title, "Items": items}
}

func (a *Admin) metricsData() map[string]any {
	snap := a.metrics.snapshot()
	order := []string{"conns_active", "conns_total", "events_in", "events_out", "rate_limited", "forbidden"}
	items := make([]any, 0, len(order))
	for _, k := range order {
		items = append(items, map[string]any{"Label": k, "Value": itoa64(snap[k])})
	}
	return map[string]any{"Items": items}
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

// adminFDL defines the admin UI as FACETS — composed, with data-facet-ids, and
// flowing through the neutral tree to native clients. AdminLayout takes the page
// content as an html field (pre-rendered, trusted) and subscribes to the live
// metrics channel via data-fa-subscribe.
const adminFDL = `
facet AdminMetric:
    what:
        label: str
        value: str
    looks:
        <div class="adm-card"><span class="adm-card__v">{value}</span><span class="adm-card__l">{label}</span></div>

facet AdminMetrics:
    facet-id: "AdminMetrics"
    what:
        items: MetricList
    looks:
        <div class="adm-cards">
            for m in items:
                <AdminMetric label="{m.label}" value="{m.value}" />
        </div>

facet AdminResList:
    facet-id: "AdminResList"
    what:
        nav: AdminNav
    looks:
        <ul class="adm-reslist">
            for item in nav.items:
                <li><a href="{item.href}" data-nav>{item.label}</a></li>
        </ul>

facet AdminRow:
    what:
        row: AdminRowData
    looks:
        <tr>
            for c in row.cells:
                <td>{c}</td>
            <td><a href="{row.href}" data-nav>view</a></td>
        </tr>

facet AdminTable:
    facet-id: "AdminTable"
    what:
        table: AdminTableData
    looks:
        <table class="adm-table">
            <thead>
                <tr>
                    for col in table.columns:
                        <th>{col}</th>
                    <th></th>
                </tr>
            </thead>
            <tbody>
                for r in table.rows:
                    <AdminRow row="{r}" />
            </tbody>
        </table>

facet AdminDetail:
    facet-id: "AdminDetail"
    what:
        record: AdminRecordData
    looks:
        <dl class="adm-fields">
            for f in record.fields:
                <dt>{f.label}</dt>
                <dd>{f.value}</dd>
        </dl>

facet AdminLayout:
    facet-id: "AdminLayout"
    what:
        nav: AdminNav
        content: html
    looks:
        <div class="adm" data-fa-subscribe="fa.admin">
            <aside class="adm__side">
                <h1 class="adm__brand">{nav.title}</h1>
                <nav>
                    for item in nav.items:
                        <a href="{item.href}" data-nav>{item.label}</a>
                </nav>
            </aside>
            <main class="adm__main">{content}</main>
        </div>
`

const adminCSS template.CSS = `
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
.adm-table a,.adm-reslist a{color:var(--ac);text-decoration:none}
.adm-reslist{list-style:none;padding:0}
.adm-fields{display:grid;grid-template-columns:200px 1fr;background:var(--bg);border:1px solid var(--b);border-radius:12px;overflow:hidden}
.adm-fields dt{padding:10px 12px;color:var(--mut);border-bottom:1px solid var(--b);font-weight:600}
.adm-fields dd{padding:10px 12px;margin:0;border-bottom:1px solid var(--b)}
.adm-err{color:#f4212e}
@media (max-width:768px){
.adm{grid-template-columns:1fr}
.adm__side{border-right:0;border-bottom:1px solid var(--b);position:sticky;top:0;z-index:10}
.adm__side nav{flex-direction:row;flex-wrap:wrap}
.adm__main{padding:16px}
.adm-fields{grid-template-columns:1fr}
.adm-fields dt{border-bottom:0;padding-bottom:2px}
.adm-table{display:block;overflow-x:auto}
}
`
