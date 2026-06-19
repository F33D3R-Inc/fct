package runtime

// Auto-admin — a generated, admin-only dashboard over every entity, Django-admin
// style, served at /admin. It is pure projection of the same IR the app runs on:
// it lists each entity, browses its rows, and creates / edits / deletes them
// through ordinary store writes that fan out to live clients exactly like an
// action's would. No app code declares it; the compiler's entity/field metadata
// is the whole schema the dashboard renders from. Every page and every mutation
// is gated on an admin session, and form posts carry the per-session CSRF token.
//
// It is on by default; set FACET_ADMIN=0 to remove it (the routes 404).

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"sort"
	"strings"

	"facet/internal/ir"
)

// adminEnabled reports whether the admin dashboard is served (default on; set
// FACET_ADMIN=0 to remove it).
func adminEnabled() bool { return os.Getenv("FACET_ADMIN") != "0" }

// AdminEnabled is the exported view of adminEnabled, so the CLI can decide
// whether to advertise the admin console URL in the startup banner.
func AdminEnabled() bool { return adminEnabled() }

// handleAdmin is the dashboard's single entry point: it authenticates an admin,
// then routes within /admin by path and method. Reads render HTML; writes go to
// the CSRF-checked _save / _delete endpoints.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !adminEnabled() {
		http.NotFound(w, r)
		return
	}
	sid := s.session(w, r)
	if !s.isAdmin(sid) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, adminShell, html.EscapeString(s.ir.App), "",
			`<h1>Admin</h1><p>You must be signed in as an admin to view this dashboard.</p><p><a href="/">Home</a></p>`)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, "/admin")
	rest = strings.Trim(rest, "/")

	switch {
	case rest == "" :
		s.adminIndex(w, sid)
	case rest == "_save":
		s.adminSave(w, r, sid)
	case rest == "_delete":
		s.adminDelete(w, r, sid)
	default:
		parts := strings.SplitN(rest, "/", 2)
		entity := parts[0]
		e, ok := s.entityByName(entity)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case len(parts) == 1:
			s.adminList(w, sid, e)
		case parts[1] == "new":
			s.adminForm(w, sid, e, nil)
		default:
			s.adminEdit(w, sid, e, parts[1])
		}
	}
}

// adminIndex lists every entity with its row count.
func (s *Server) adminIndex(w http.ResponseWriter, sid string) {
	s.mu.Lock()
	type row struct {
		name  string
		count int
	}
	var rows []row
	for _, e := range s.ir.Entities {
		rows = append(rows, row{e.Name, len(s.entities[e.Name])})
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	var b strings.Builder
	b.WriteString(`<h1>Admin</h1><p class="muted">Generated dashboard over every entity in this app.</p>`)
	b.WriteString(`<table><thead><tr><th>Entity</th><th>Rows</th><th></th></tr></thead><tbody>`)
	for _, rw := range rows {
		fmt.Fprintf(&b, `<tr><td><a href="/admin/%s">%s</a></td><td>%d</td><td><a class="btn" href="/admin/%s/new">+ New</a></td></tr>`,
			urlSeg(rw.name), html.EscapeString(rw.name), rw.count, urlSeg(rw.name))
	}
	b.WriteString(`</tbody></table>`)
	s.adminPage(w, sid, "", b.String())
}

// adminList shows an entity's rows (most recent first), each linking to its edit
// page, with a delete control.
func (s *Server) adminList(w http.ResponseWriter, sid string, e ir.Entity) {
	cols := columns(e)
	s.mu.Lock()
	rows := append([]any{}, s.entities[e.Name]...)
	s.mu.Unlock()
	// newest first by id
	sort.SliceStable(rows, func(i, j int) bool {
		return toInt(asRecord(rows[i])["id"]) > toInt(asRecord(rows[j])["id"])
	})
	if len(rows) > 200 {
		rows = rows[:200]
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<p><a href="/admin">← Admin</a></p><h1>%s <span class="muted">(%d)</span></h1>`,
		html.EscapeString(e.Name), len(rows))
	fmt.Fprintf(&b, `<p><a class="btn" href="/admin/%s/new">+ New %s</a></p>`, urlSeg(e.Name), html.EscapeString(e.Name))
	b.WriteString(`<table><thead><tr>`)
	for _, c := range cols {
		fmt.Fprintf(&b, `<th>%s</th>`, html.EscapeString(c))
	}
	b.WriteString(`<th></th></tr></thead><tbody>`)
	for _, r := range rows {
		m := asRecord(r)
		id := toInt(m["id"])
		b.WriteString(`<tr>`)
		for _, c := range cols {
			fmt.Fprintf(&b, `<td>%s</td>`, html.EscapeString(displayCell(e, c, m[c])))
		}
		fmt.Fprintf(&b, `<td class="actions"><a href="/admin/%s/%d">edit</a> %s</td></tr>`,
			urlSeg(e.Name), id, s.adminDeleteForm(sid, e.Name, id))
	}
	b.WriteString(`</tbody></table>`)
	s.adminPage(w, sid, e.Name, b.String())
}

// adminForm renders a create form (row == nil) or an edit form for a row.
func (s *Server) adminForm(w http.ResponseWriter, sid string, e ir.Entity, row record) {
	title := "New " + e.Name
	id := 0
	if row != nil {
		id = toInt(row["id"])
		title = fmt.Sprintf("%s #%d", e.Name, id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<p><a href="/admin/%s">← %s</a></p><h1>%s</h1>`, urlSeg(e.Name), html.EscapeString(e.Name), html.EscapeString(title))
	fmt.Fprintf(&b, `<form method="post" action="/admin/_save"><input type="hidden" name="_csrf" value="%s"><input type="hidden" name="_entity" value="%s"><input type="hidden" name="_id" value="%d">`,
		html.EscapeString(csrfToken(sid)), html.EscapeString(e.Name), id)
	for _, f := range e.Fields {
		if f.Name == "id" {
			continue
		}
		var cur any
		if row != nil {
			cur = row[f.Name]
		}
		fmt.Fprintf(&b, `<label>%s <span class="muted">%s</span></label>%s`,
			html.EscapeString(f.Name), html.EscapeString(fieldTypeLabel(f)), adminInput(f, cur))
	}
	b.WriteString(`<div class="formrow"><button type="submit">Save</button></div></form>`)
	s.adminPage(w, sid, e.Name, b.String())
}

// adminEdit loads a row by id and renders its edit form.
func (s *Server) adminEdit(w http.ResponseWriter, sid string, e ir.Entity, idStr string) {
	id := toInt(idStr)
	s.mu.Lock()
	var row record
	for _, r := range s.entities[e.Name] {
		if m, ok := r.(record); ok && toInt(m["id"]) == id {
			row = copyRecord(m)
			break
		}
	}
	s.mu.Unlock()
	if row == nil {
		http.NotFound(w, nil)
		return
	}
	s.adminForm(w, sid, e, row)
}

// adminSave creates or updates a row from a submitted form, writing through the
// store and fanning the change out to live clients.
func (s *Server) adminSave(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	if !csrfValid(sid, r.FormValue("_csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	entity := r.FormValue("_entity")
	e, ok := s.entityByName(entity)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := toInt(r.FormValue("_id"))

	s.mu.Lock()
	var row record
	if id > 0 {
		for _, rr := range s.entities[entity] {
			if m, ok := rr.(record); ok && toInt(m["id"]) == id {
				row = m
				break
			}
		}
	}
	isNew := row == nil
	if isNew {
		s.nextID[entity]++
		id = s.nextID[entity]
		row = record{"id": id}
		s.entities[entity] = append(s.entities[entity], row)
	}
	for _, f := range e.Fields {
		if f.Name == "id" {
			continue
		}
		if f.Type == "bool" {
			row[f.Name] = r.FormValue(f.Name) != ""
			continue
		}
		// Leave an unspecified password untouched on edit (the field is blank in the
		// form so a hash is not echoed); a non-empty value overwrites it.
		if f.Name == "password" && !isNew && r.FormValue(f.Name) == "" {
			continue
		}
		row[f.Name] = coerce(r.FormValue(f.Name), f.Type)
	}
	s.commit([]durOp{{kind: "save", entity: entity, row: row}})
	notReserved := !isReservedEntity(entity)
	snapshot := append([]any{}, s.entities[entity]...)
	s.mu.Unlock()

	if notReserved {
		s.broadcast(map[string]any{entity: snapshot})
	}
	s.recordAudit(s.actorName(sid), "admin:save", true, entity)
	http.Redirect(w, r, "/admin/"+urlSeg(entity), http.StatusSeeOther)
}

// adminDelete removes a row (cascading like the database) and fans the change
// out.
func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request, sid string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	if !csrfValid(sid, r.FormValue("_csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	entity := r.FormValue("_entity")
	if _, ok := s.entityByName(entity); !ok {
		http.NotFound(w, r)
		return
	}
	id := toInt(r.FormValue("_id"))

	s.mu.Lock()
	entChanged := map[string]bool{}
	rows := s.entities[entity]
	for i, rr := range rows {
		if m, ok := rr.(record); ok && toInt(m["id"]) == id {
			s.entities[entity] = append(rows[:i], rows[i+1:]...)
			s.commit([]durOp{{kind: "delete", entity: entity, id: id}})
			entChanged[entity] = true
			s.cascadeMem(entity, map[int]bool{id: true}, entChanged)
			break
		}
	}
	deltas := map[string]any{}
	for ent := range entChanged {
		if !isReservedEntity(ent) {
			deltas[ent] = append([]any{}, s.entities[ent]...)
		}
	}
	s.mu.Unlock()

	if len(deltas) > 0 {
		s.broadcast(deltas)
	}
	s.recordAudit(s.actorName(sid), "admin:delete", true, entity)
	http.Redirect(w, r, "/admin/"+urlSeg(entity), http.StatusSeeOther)
}

// adminDeleteForm renders an inline delete button as its own tiny CSRF-protected
// form, so a delete is always a POST.
func (s *Server) adminDeleteForm(sid, entity string, id int) string {
	return fmt.Sprintf(
		`<form method="post" action="/admin/_delete" class="inline" onsubmit="return confirm('Delete %s #%d?')">`+
			`<input type="hidden" name="_csrf" value="%s"><input type="hidden" name="_entity" value="%s">`+
			`<input type="hidden" name="_id" value="%d"><button class="danger" type="submit">delete</button></form>`,
		html.EscapeString(entity), id, html.EscapeString(csrfToken(sid)), html.EscapeString(entity), id)
}

// adminInput renders the right control for a field's type, pre-filled with cur.
func adminInput(f ir.Field, cur any) string {
	switch {
	case f.Type == "bool":
		checked := ""
		if truthy(cur) {
			checked = " checked"
		}
		return fmt.Sprintf(`<input type="checkbox" name="%s" value="1"%s>`, html.EscapeString(f.Name), checked)
	case f.Enum != "":
		// rendered as a free text field; the closed set is documented in the label.
		return fmt.Sprintf(`<input type="text" name="%s" value="%s">`, html.EscapeString(f.Name), html.EscapeString(toStr(cur)))
	case f.Name == "password":
		return fmt.Sprintf(`<input type="password" name="%s" placeholder="(unchanged)">`, html.EscapeString(f.Name))
	case f.IsRelation() || f.Type == "int" || f.Type == "money" || f.Type == "date":
		v := ""
		if cur != nil {
			v = itoa(toInt(cur))
		}
		return fmt.Sprintf(`<input type="number" name="%s" value="%s">`, html.EscapeString(f.Name), html.EscapeString(v))
	default:
		return fmt.Sprintf(`<input type="text" name="%s" value="%s">`, html.EscapeString(f.Name), html.EscapeString(toStr(cur)))
	}
}

// displayCell renders a cell for the list table, masking credential-shaped fields.
func displayCell(e ir.Entity, col string, v any) string {
	if col == "password" {
		if toStr(v) == "" {
			return ""
		}
		return "••••••"
	}
	for _, f := range e.Fields {
		if f.Name == col && f.Secret {
			return "••••••"
		}
	}
	s := toStr(v)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

func fieldTypeLabel(f ir.Field) string {
	if f.Enum != "" {
		return f.Enum
	}
	if f.IsRelation() {
		return f.Ref + " id"
	}
	return f.Type
}

// adminPage wraps body content in the dashboard shell.
func (s *Server) adminPage(w http.ResponseWriter, sid, active, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, adminShell, html.EscapeString(s.ir.App), s.adminNav(active), body)
}

// adminNav renders the entity sidebar.
func (s *Server) adminNav(active string) string {
	var names []string
	for _, e := range s.ir.Entities {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(`<a href="/admin" class="brand">⌘ Admin</a>`)
	for _, n := range names {
		cls := "navlink"
		if n == active {
			cls += " active"
		}
		fmt.Fprintf(&b, `<a class="%s" href="/admin/%s">%s</a>`, cls, urlSeg(n), html.EscapeString(n))
	}
	return b.String()
}

// actorName returns a session's actor for audit detail.
func (s *Server) actorName(sid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, _ := s.actorOf(sid)
	return a
}

// url escapes an entity name for use in a path segment (entity names are
// identifiers, so this is belt-and-braces).
func urlSeg(s string) string { return strings.ReplaceAll(s, "/", "") }

const adminShell = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — Admin</title>
<style>
 :root{--bg:#0f1115;--panel:#161a22;--fg:#e7eaf0;--muted:#8b93a7;--accent:#5b8cff;--border:#262c39;--danger:#ff6b6b}
 *{box-sizing:border-box} body{margin:0;font:15px/1.5 system-ui,sans-serif;color:var(--fg);background:var(--bg);display:flex;min-height:100vh}
 nav{width:220px;background:var(--panel);border-right:1px solid var(--border);padding:1rem;display:flex;flex-direction:column;gap:.25rem}
 .brand{font-weight:700;font-size:1.1rem;margin-bottom:.75rem;color:var(--fg);text-decoration:none}
 .navlink{color:var(--muted);text-decoration:none;padding:.35rem .5rem;border-radius:6px}
 .navlink:hover{background:#1d2330;color:var(--fg)} .navlink.active{background:#1d2330;color:var(--accent)}
 main{flex:1;padding:1.5rem 2rem;max-width:960px}
 h1{font-size:1.4rem;margin:.2rem 0 1rem} .muted{color:var(--muted);font-weight:400}
 table{width:100%%;border-collapse:collapse;margin:.5rem 0} th,td{text-align:left;padding:.5rem .6rem;border-bottom:1px solid var(--border);vertical-align:top}
 th{color:var(--muted);font-weight:600;font-size:.85rem} td a{color:var(--accent)}
 a{color:var(--accent)} .btn{display:inline-block;background:var(--accent);color:#fff;padding:.35rem .7rem;border-radius:6px;text-decoration:none;font-size:.9rem}
 form{display:flex;flex-direction:column;gap:.35rem;max-width:32rem} label{color:var(--muted);font-size:.85rem;margin-top:.5rem}
 input{font:inherit;padding:.45rem .6rem;background:#0c0e13;border:1px solid var(--border);border-radius:6px;color:var(--fg)}
 input[type=checkbox]{width:auto} .formrow{margin-top:1rem}
 button{font:inherit;padding:.45rem .9rem;border:0;border-radius:6px;background:var(--accent);color:#fff;cursor:pointer}
 button.danger{background:transparent;color:var(--danger);border:1px solid var(--border);padding:.2rem .5rem;font-size:.85rem}
 form.inline{display:inline} .actions{white-space:nowrap}
</style></head>
<body><nav>%s</nav><main>%s</main></body></html>
`
