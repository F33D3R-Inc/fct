package codegen

import (
	"fmt"
	"strings"

	"facet/internal/ir"
)

// GenerateTS emits TypeScript. TS is the easy target of the three: interfaces
// reference each other and themselves by name with no boxing/pointer concept
// at all, and a discriminated union (`{type: "insert_node", ...} | ...`) is
// TS's native way to spell a tagged union — no per-variant marshal shimming
// like Go needs.
func GenerateTS(s Schema) (string, error) {
	var b strings.Builder
	b.WriteString("// Code generated from an FCT wire schema. DO NOT EDIT.\n")
	b.WriteString("// Source: SCHEMA_IDL_SCOPE.md Tier C (FCT-as-IDL).\n\n")

	for _, e := range s.Enums {
		vals := make([]string, len(e.Values))
		for i := range e.Values {
			// The literal union member is the value's real wire text
			// (enumWireName), not its FCT-source name — the two differ only
			// when an `as "Wire"` override is declared.
			vals[i] = fmt.Sprintf("%q", enumWireName(e, i))
		}
		fmt.Fprintf(&b, "export type %s = %s;\n\n", e.Name, strings.Join(vals, " | "))
	}

	enums := enumNameSet(s.Enums)
	for _, t := range s.Types {
		if t.Query {
			b.WriteString("// Bound from the URL query string, never a JSON body.\n")
		}
		fmt.Fprintf(&b, "export interface %s {\n", t.Name)
		for _, f := range t.Fields {
			writeTSField(&b, f, enums)
		}
		b.WriteString("}\n\n")
	}

	for _, m := range s.Messages {
		names := make([]string, len(m.Variants))
		for i, v := range m.Variants {
			names[i] = m.Name + snakeToUpperCamel(v.Name)
		}
		fmt.Fprintf(&b, "export type %s = %s;\n\n", m.Name, strings.Join(names, " | "))
		for _, v := range m.Variants {
			fmt.Fprintf(&b, "export interface %s {\n", m.Name+snakeToUpperCamel(v.Name))
			fmt.Fprintf(&b, "  type: %q;\n", v.Name)
			for _, f := range v.Fields {
				writeTSField(&b, f, enums)
			}
			b.WriteString("}\n\n")
		}
	}
	return b.String(), nil
}

// writeTSField writes one interface field. TS interfaces are compile-time
// only — there is no runtime "apply this default" step the way Rust's
// generated `fn default_...()` or Go's genGoTypeDefaults provides — so a
// defaulted field is marked optional (`?`) with the actual default value in
// a trailing comment, and a caller constructing a request is responsible for
// supplying it or accepting the server's default, same as it already must
// for every hand-written type today.
func writeTSField(b *strings.Builder, f ir.WireField, enums map[string]bool) {
	opt := ""
	if f.Optional || f.Default != "" {
		opt = "?"
	}
	comment := ""
	if f.Default != "" {
		comment = fmt.Sprintf(" // default: %s", f.Default)
	}
	fmt.Fprintf(b, "  %s%s: %s;%s\n", f.Name, opt, tsFieldType(f, enums), comment)
}

func tsFieldType(f ir.WireField, enums map[string]bool) string {
	t := tsScalar(f.Type, enums)
	if f.List {
		t = t + "[]"
	}
	return t
}

func tsScalar(core string, enums map[string]bool) string {
	switch core {
	case "text", "date", "datetime":
		return "string"
	case "bool":
		return "boolean"
	case "int", "money", "number", "float":
		return "number"
	case "json":
		return "unknown"
	}
	if enums[core] {
		return core // the enum's own generated union-of-string-literals type
	}
	return core // a Ref: another Type/Message's generated interface, by name
}
