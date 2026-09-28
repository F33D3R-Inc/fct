package ir

import (
	"encoding/json"
	"sort"
)

// SearchedFields answers, per entity, the text fields some query in g tests
// case-insensitively by substring — `contains`, `starts_with` or
// `ends_with` of `lower(x.f)`, where x ranges over the entity (an
// aggregate's item variable, a view's `for`, a filtered statement's). They
// are the fields a store keeps a folded text index over, so that search is
// read through an index rather than by reading every row. It is a fact about
// how the program reads, derived from it — not part of the IR, the schema or
// any contract, so it changes nothing a client or another runtime sees.
//
// The walk is over the IR's JSON rather than its Go types, so an expression
// in any part of the program — an action, a derive, a view, a component, a
// job — is found without a walker per construct that the IR's growth could
// leave behind.
func SearchedFields(g *IR) map[string][]string {
	out := map[string][]string{}
	if g == nil {
		return out
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return out
	}
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return out
	}
	text := map[string]map[string]bool{} // entity -> its text fields
	for _, e := range g.Entities {
		text[e.Name] = map[string]bool{}
		for _, f := range e.Fields {
			if f.Type == "text" {
				text[e.Name][f.Name] = true
			}
		}
	}
	found := map[string]map[string]bool{}
	var walk func(v any, bound map[string]string)
	walk = func(v any, bound map[string]string) {
		switch t := v.(type) {
		case []any:
			for _, x := range t {
				walk(x, bound)
			}
		case map[string]any:
			if varName, ok := t["var"].(string); ok && varName != "" {
				ent := ""
				if k, _ := t["kind"].(string); k == "agg" {
					ent, _ = t["name"].(string)
				}
				if c, ok := t["coll"].(string); ok && ent == "" {
					ent = c
				}
				if c, ok := t["entity"].(string); ok && ent == "" {
					ent = c
				}
				if _, isEntity := text[ent]; isEntity {
					inner := make(map[string]string, len(bound)+1)
					for k, v := range bound {
						inner[k] = v
					}
					inner[varName] = ent
					bound = inner
				}
			}
			if ent, field, ok := searchedField(t, bound); ok && text[ent][field] {
				if found[ent] == nil {
					found[ent] = map[string]bool{}
				}
				found[ent][field] = true
			}
			for _, x := range t {
				walk(x, bound)
			}
		}
	}
	walk(root, map[string]string{})
	for ent, fs := range found {
		var fields []string
		for f := range fs {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		out[ent] = fields
	}
	return out
}

// searchedField recognises `op(lower(x.f), …)` for a substring op, x bound
// to an entity: that entity and f. The op is the call the source wrote
// (`contains(lower(x.f), q)`) or its binary form.
func searchedField(t map[string]any, bound map[string]string) (string, string, bool) {
	var first any
	switch k, _ := t["kind"].(string); k {
	case "call":
		name, _ := t["name"].(string)
		args, _ := t["args"].([]any)
		if !substringOp(name) || len(args) != 2 {
			return "", "", false
		}
		first = args[0]
	case "bin":
		op, _ := t["op"].(string)
		if !substringOp(op) {
			return "", "", false
		}
		first = t["l"]
	default:
		return "", "", false
	}
	low, ok := first.(map[string]any)
	if !ok || low["kind"] != "call" || low["name"] != "lower" {
		return "", "", false
	}
	largs, _ := low["args"].([]any)
	if len(largs) != 1 {
		return "", "", false
	}
	get, ok := largs[0].(map[string]any)
	if !ok || get["kind"] != "get" {
		return "", "", false
	}
	field, _ := get["field"].(string)
	obj, ok := get["obj"].(map[string]any)
	if !ok || obj["kind"] != "ref" || field == "" {
		return "", "", false
	}
	name, _ := obj["name"].(string)
	ent, ok := bound[name]
	return ent, field, ok
}

func substringOp(op string) bool {
	return op == "contains" || op == "starts_with" || op == "ends_with"
}
