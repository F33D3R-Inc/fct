package fa

import (
	"errors"
	"html/template"
	"net/http"
	"strings"
	"unicode"
)

// ErrForbidden is returned by RenderFor when a `who: require` policy denies the
// viewer.
var ErrForbidden = errors.New("fa: forbidden")

// View is the viewer context for an authorized render — who is asking. Build it
// with App.View(r) or Ctx.View().
type View struct {
	Identity string
	R        *http.Request
}

type facetAuth struct {
	require    []string
	redactions []redaction
}

type redaction struct {
	field  string // dotted field path, e.g. "user.ssn"
	unless string // policy name; "" means redact unconditionally
}

// Policy registers a named authorization policy used by `who:` require/redact.
// Chainable.
func (c *Compiled) Policy(name string, fn func(View) bool) *Compiled {
	c.policies[name] = fn
	return c
}

// RenderFor renders a facet for a viewer, ENFORCING its `who:` block: every
// `require` policy must pass (else ErrForbidden), and `redact` fields are
// stripped from a COPY of the data before render (the caller's data is never
// mutated). Public facets (no who:) render normally.
func (c *Compiled) RenderFor(v View, facet string, data any) (template.HTML, error) {
	a, protected := c.auth[facet]
	if !protected {
		return c.render(facet, data)
	}
	for _, name := range a.require {
		fn := c.policies[name]
		if fn == nil {
			return "", errors.New("facet " + facet + " requires policy " + name + " which is not registered (Compiled.Policy)")
		}
		if !fn(v) {
			return "", ErrForbidden
		}
	}
	if len(a.redactions) > 0 {
		data = cloneData(data)
		for _, r := range a.redactions {
			if r.unless != "" {
				if fn := c.policies[r.unless]; fn != nil && fn(v) {
					continue // policy passes → keep the field
				}
			}
			redactPath(data, r.field)
		}
	}
	return c.render(facet, data)
}

// View builds a viewer context from a request (its identity via the App resolver).
func (a *App) View(r *http.Request) View {
	return View{Identity: a.identityOf(r), R: r}
}

// View builds a viewer context from a handler's action context.
func (c Ctx) View() View {
	return View{Identity: c.Identity, R: c.R}
}

// cloneData deep-copies nested map[string]any so redaction never mutates the
// caller's data. Non-map values are returned as-is (immutable enough for v0).
func cloneData(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, vv := range m {
		out[k] = cloneData(vv)
	}
	return out
}

// redactPath deletes a dotted field (e.g. "user.ssn") from map data, matching
// the idiomatic Go keys the templates use (user.avatar_url → .User.AvatarURL).
func redactPath(data any, path string) {
	m, ok := data.(map[string]any)
	if !ok {
		return
	}
	parts := strings.Split(path, ".")
	for i, p := range parts {
		key := goName(p)
		if i == len(parts)-1 {
			delete(m, key)
			return
		}
		next, ok := m[key].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
}

// goInitialisms / goName mirror internal/codegen so redaction keys match the
// generated templates. Keep in sync.
var goInitialisms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI", "api": "API", "html": "HTML",
	"http": "HTTP", "https": "HTTPS", "json": "JSON", "xml": "XML", "sql": "SQL",
	"uuid": "UUID", "css": "CSS", "db": "DB", "ip": "IP", "tcp": "TCP",
	"udp": "UDP", "tls": "TLS", "ssh": "SSH", "ui": "UI", "cpu": "CPU", "ttl": "TTL",
}

func goName(field string) string {
	var b strings.Builder
	for _, p := range strings.Split(field, "_") {
		if p == "" {
			continue
		}
		if up, ok := goInitialisms[strings.ToLower(p)]; ok {
			b.WriteString(up)
			continue
		}
		r := []rune(p)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}
