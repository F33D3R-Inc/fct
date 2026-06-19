package runtime

import (
	"encoding/json"
	"fmt"
	"sort"

	"facet/internal/ir"
)

// `facet seed` loads starter/fixture rows into an app from a JSON file, so a
// fresh database (or a test run) starts with realistic data. The file is an
// object of entity name -> array of row objects:
//
//	{
//	  "User":  [ { "id": 1, "name": "ada" } ],
//	  "Post":  [ { "author": 1, "body": "hello" } ]
//	}
//
// A backup file (with a top-level "data" key) is also accepted, so a snapshot
// doubles as a seed. Rows may set "id" (to wire up relations) or omit it (auto-
// assigned). Values are coerced to each field's type before insert.

// seedDoc is the accepted on-disk shape: either a bare entity->rows map or a
// backup file whose rows live under "data".
func parseSeed(raw []byte) (map[string][]map[string]any, error) {
	// Try the backup shape first (has "data").
	var wrapped struct {
		Data map[string][]map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Data != nil {
		return wrapped.Data, nil
	}
	var bare map[string][]map[string]any
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("seed file must be a JSON object of Entity -> [rows]: %w", err)
	}
	return bare, nil
}

// Seed inserts the rows from a seed file into the app. With dry, it runs against
// the in-memory store (validating the seed without touching the database);
// otherwise it writes through to Postgres. It returns the number of rows
// inserted. Parents are inserted before children where an order can be inferred
// from the relation graph, so foreign keys resolve.
func Seed(graph *ir.IR, raw []byte, dry bool) (int, error) {
	data, err := parseSeed(raw)
	if err != nil {
		return 0, err
	}

	var srv *Server
	if dry {
		srv, err = NewInMemory(graph)
	} else {
		srv, err = New(graph)
	}
	if err != nil {
		return 0, err
	}
	defer srv.Shutdown()

	fields := map[string]map[string]ir.Field{}
	for _, e := range graph.Entities {
		fm := map[string]ir.Field{}
		for _, f := range e.Fields {
			fm[f.Name] = f
		}
		fields[e.Name] = fm
	}

	count := 0
	for _, ent := range seedOrder(graph) {
		rows, ok := data[ent]
		if !ok {
			continue
		}
		fm, known := fields[ent]
		if !known {
			return count, fmt.Errorf("seed references unknown entity %q", ent)
		}
		for _, row := range rows {
			coerced := map[string]any{}
			for k, v := range row {
				if k == "id" {
					coerced[k] = toInt(v)
					continue
				}
				f, ok := fm[k]
				if !ok {
					return count, fmt.Errorf("entity %q has no field %q", ent, k)
				}
				coerced[k] = coerce(v, seedType(f))
			}
			if _, err := srv.AddRow(ent, coerced); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

// seedType is a field's coercion type: a relation stores an int id, an enum is
// text, everything else is its declared type.
func seedType(f ir.Field) string {
	if f.IsRelation() {
		return "int"
	}
	return f.Type
}

// seedOrder returns the entities in an order where a parent is inserted before
// any entity that references it (a topological sort over the relation graph),
// falling back to declaration order for cycles.
func seedOrder(graph *ir.IR) []string {
	deps := map[string][]string{} // entity -> entities it references
	all := []string{}
	for _, e := range graph.Entities {
		all = append(all, e.Name)
		for _, f := range e.Fields {
			if f.IsRelation() && f.Ref != e.Name {
				deps[e.Name] = append(deps[e.Name], f.Ref)
			}
		}
	}
	visited := map[string]bool{}
	var order []string
	var visit func(string, map[string]bool)
	visit = func(n string, stack map[string]bool) {
		if visited[n] || stack[n] {
			return
		}
		stack[n] = true
		refs := append([]string{}, deps[n]...)
		sort.Strings(refs)
		for _, d := range refs {
			visit(d, stack)
		}
		stack[n] = false
		visited[n] = true
		order = append(order, n)
	}
	for _, n := range all {
		visit(n, map[string]bool{})
	}
	return order
}
