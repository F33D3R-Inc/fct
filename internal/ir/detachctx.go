package ir

import (
	"fmt"
	"reflect"
	"sort"

	"facet/internal/ast"
)

// Daemon-context procs.
//
// A daemon body and the process entry point (`proc main`) run on their own
// goroutine, under no request's lock, for as long as they like — which is
// what makes listen/listenOn/accept, `detach` and `act` legal there and
// nowhere else. A proc that is only ever started by `detach` from such a
// body runs exactly the same way: nothing waits for it, and it holds no lock
// its starter holds. So such a proc is lowered as a daemon-context body too,
// and a daemon (or main) can hand a whole accept loop — one that itself
// detaches a handler per connection — to a task of its own.
//
// "Only ever" is the rule, computed to a fixpoint: a proc is daemon-context
// iff it is referenced at least once and EVERY reference to it anywhere in
// the program — `do`, `spawn`, `detach`, or a call in an expression, from an
// action, a view, a derive, a proc or a daemon — is a `detach` from a
// daemon-context body (a daemon, main, or another daemon-context proc), and
// it is reachable from a daemon or main by those detaches. One ordinary
// call is enough to make it an ordinary proc again (a `do` from an action
// would run it under that action's lock), and the diagnostic for its accept
// or detach then names that call.

// procRef is one reference to a proc: how (detach or not) and from where.
type procRef struct {
	detach bool
	from   string // proc name, "" for a daemon or main, "\x00" for anything else
	where  string // for diagnostics: "`do P(...)` at line N in proc X"
}

// daemonContextProcs returns the set of daemon-context procs and, for each
// proc that is detached somewhere but is not daemon-context, the reference
// that disqualifies it.
func daemonContextProcs(app *ast.App) (map[string]bool, map[string]string) {
	procs := map[string]bool{}
	for _, p := range app.Procs {
		procs[p.Name] = true
	}
	refs := map[string][]procRef{}
	collect := func(v any, from, what string) {
		walkProcRefs(reflect.ValueOf(v), 0, func(name string, detach bool, kind string, line int) {
			if !procs[name] {
				return
			}
			call := kind + " " + name + "(...)"
			if kind == "call" {
				call = name + "(...)"
			}
			refs[name] = append(refs[name], procRef{detach: detach, from: from, where: fmt.Sprintf("`%s` at line %d in %s", call, line, what)})
		})
	}
	for _, p := range app.Procs {
		from := p.Name
		if isProcessEntry(p) {
			from = ""
		}
		collect(p.Body, from, "proc "+p.Name)
	}
	for _, d := range app.Daemons {
		collect(d.Body, "", "daemon "+d.Name)
	}
	rest := reflect.ValueOf(app).Elem()
	for i := 0; i < rest.NumField(); i++ {
		name := rest.Type().Field(i).Name
		if name == "Procs" || name == "Daemons" || !rest.Type().Field(i).IsExported() {
			continue
		}
		collect(rest.Field(i).Interface(), "\x00", "the app's "+name)
	}

	ctx := map[string]bool{}
	for name := range procs {
		if len(refs[name]) > 0 {
			ctx[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name := range ctx {
			for _, r := range refs[name] {
				if !r.detach || (r.from != "" && !ctx[r.from]) {
					delete(ctx, name)
					changed = true
					break
				}
			}
		}
	}
	// Reachable from a daemon or main through detaches alone.
	reach := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name := range ctx {
			if reach[name] {
				continue
			}
			for _, r := range refs[name] {
				if r.from == "" || reach[r.from] {
					reach[name] = true
					changed = true
					break
				}
			}
		}
	}
	why := map[string]string{}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if reach[name] {
			continue
		}
		detached := false
		for _, r := range refs[name] {
			detached = detached || r.detach
		}
		if !detached {
			continue
		}
		for _, r := range refs[name] {
			if !r.detach {
				why[name] = r.where
				break
			}
			if r.from != "" && !reach[r.from] {
				why[name] = r.where + ", which is not itself only ever detached from a daemon or main"
			}
		}
	}
	return reach, why
}

// walkProcRefs visits every proc reference reachable from v: Do, Spawn and
// Detach statements, and calls in expressions. line is the innermost
// enclosing node's Line.
func walkProcRefs(v reflect.Value, line int, visit func(name string, detach bool, kind string, line int)) {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			walkProcRefs(v.Elem(), line, visit)
		}
		return
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkProcRefs(v.Index(i), line, visit)
		}
		return
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkProcRefs(iter.Value(), line, visit)
		}
		return
	case reflect.Struct:
	default:
		return
	}
	if f := v.FieldByName("Line"); f.IsValid() && f.Kind() == reflect.Int && f.Int() > 0 {
		line = int(f.Int())
	}
	switch n := v.Interface().(type) {
	case ast.Do:
		visit(n.Proc, false, "do", line)
	case ast.Spawn:
		visit(n.Proc, false, "spawn", line)
	case ast.Detach:
		visit(n.Proc, true, "detach", line)
	case ast.Call:
		visit(n.Name, false, "call", line)
	}
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).IsExported() {
			walkProcRefs(v.Field(i), line, visit)
		}
	}
}
