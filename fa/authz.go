package fa

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"reflect"
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
	for _, r := range a.redactions {
		if r.unless != "" {
			if fn := c.policies[r.unless]; fn != nil && fn(v) {
				continue // policy passes → keep the field
			}
		}
		redacted, ok := redactField(data, r.field)
		if !ok {
			// A declared redaction that cannot be located in the render data is a
			// misconfiguration we MUST NOT ignore: silently rendering would leak the
			// very field the app asked to hide. Fail closed.
			return "", fmt.Errorf("fa: facet %q declares `redact %s` but no such field exists in the render data; refusing to render rather than risk leaking it", facet, r.field)
		}
		data = redacted
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

// redactField returns a copy of data with the dotted field path (e.g. "user.ssn")
// removed, plus whether the path could be resolved. It walks maps, structs,
// pointers, and slices, so redaction applies whether the app renders with
// map[string]any or fct-generated typed structs. Field names are matched against
// the idiomatic Go keys the templates use (user.avatar_url → .User.AvatarURL).
//
// The caller's data is never mutated: every container along the path is rebuilt;
// sibling subtrees are shared by reference (the templates only read them). The
// returned ok is false only when a redaction provably cannot apply (a struct with
// no such field) — the caller fails closed rather than leak the unredacted field.
func redactField(data any, path string) (any, bool) {
	v := reflect.ValueOf(data)
	if !v.IsValid() {
		return data, true // nil data → nothing to leak
	}
	out, ok := redactedValue(v, strings.Split(path, "."))
	if !out.IsValid() {
		return data, ok
	}
	return out.Interface(), ok
}

// redactedValue is redactField's recursive core, working on reflect.Values. It
// always returns freshly built containers (so nothing the caller holds is
// mutated) and never panics on unexported struct fields (those are skipped — the
// templates can't read them anyway).
func redactedValue(v reflect.Value, parts []string) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v, true
		}
		inner, ok := redactedValue(v.Elem(), parts)
		out := reflect.New(v.Type()).Elem()
		out.Set(inner) // concrete value is assignable to the interface
		return out, ok

	case reflect.Pointer:
		if v.IsNil() {
			return v, true
		}
		inner, ok := redactedValue(v.Elem(), parts)
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(inner)
		return out, ok

	case reflect.Slice:
		if v.IsNil() {
			return v, true
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		ok := true
		for i := 0; i < v.Len(); i++ {
			e, eok := redactedValue(v.Index(i), parts)
			out.Index(i).Set(e)
			ok = ok && eok
		}
		return out, ok

	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		ok := true
		for i := 0; i < v.Len(); i++ {
			e, eok := redactedValue(v.Index(i), parts)
			out.Index(i).Set(e)
			ok = ok && eok
		}
		return out, ok

	case reflect.Map:
		// Only string-keyed maps can be addressed by a field path. They are a
		// dynamic shape we can't validate, so they always count as applicable.
		if v.IsNil() || v.Type().Key().Kind() != reflect.String {
			return v, v.Kind() == reflect.Map
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		key := goName(parts[0])
		for _, k := range v.MapKeys() {
			if k.String() == key {
				if len(parts) == 1 {
					continue // drop the key → redacted
				}
				nv, _ := redactedValue(v.MapIndex(k), parts[1:])
				out.SetMapIndex(k, nv)
				continue
			}
			out.SetMapIndex(k, v.MapIndex(k)) // sibling shared (read-only render)
		}
		return out, true

	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		field := goName(parts[0])
		idx := -1
		for i := 0; i < v.NumField(); i++ {
			if !out.Field(i).CanSet() {
				continue // unexported — templates can't see it; leave zero
			}
			out.Field(i).Set(v.Field(i)) // sibling shared (read-only render)
			if v.Type().Field(i).Name == field {
				idx = i
			}
		}
		if idx == -1 {
			return out, false // declared redaction targets a non-existent field
		}
		if len(parts) == 1 {
			out.Field(idx).Set(reflect.Zero(v.Type().Field(idx).Type)) // redact
			return out, true
		}
		nv, ok := redactedValue(v.Field(idx), parts[1:])
		out.Field(idx).Set(nv)
		return out, ok

	default:
		return v, false // a leaf where we expected a container → cannot apply
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
