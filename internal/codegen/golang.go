package codegen

import (
	"fmt"
	"strings"

	"facet/internal/ir"
)

// GenerateGo emits Go types matching the pattern fct/runtime/fqclient.go's
// hand-written fqTxOp already uses, since Go has no native tagged-union: a
// kitchen-sink struct carrying every variant's fields, plus MarshalJSON /
// UnmarshalJSON that switch on the discriminant to encode/decode only the one
// variant's fields — so an insert_node never gets a stray "where" key, and a
// decode never leaves fields from a different variant looking populated.
func GenerateGo(s Schema, packageName string) (string, error) {
	// Body built first, header (package + imports) prepended after: a schema
	// with no `message` and no `json`-scalar field emits no code that uses
	// encoding/json or fmt (a plain `type`'s fields need neither), so those
	// imports would otherwise be unconditionally unused and fail `go build` —
	// a real gap this exact case surfaced (see TestEnumWireRenameRoundTrip),
	// not a hypothetical.
	var b strings.Builder

	for _, e := range s.Enums {
		fmt.Fprintf(&b, "type %s string\n\n", e.Name)
		b.WriteString("const (\n")
		// The constant's value is the value's actual wire text (enumWireName:
		// its own lowercase text by default, or an `as "Wire"` override) —
		// `type X string` has no separate (de)serialize step for a plain
		// field, so the Go constant's own value IS what goes on the wire.
		for i, v := range e.Values {
			fmt.Fprintf(&b, "\t%s%s %s = %q\n", e.Name, snakeToUpperCamel(v), e.Name, enumWireName(e, i))
		}
		b.WriteString(")\n\n")
	}

	enums := enumNameSet(s.Enums)
	for _, t := range s.Types {
		if t.Query {
			b.WriteString("// Bound from the URL query string, never a JSON body.\n")
		}
		fmt.Fprintf(&b, "type %s struct {\n", t.Name)
		for _, f := range t.Fields {
			fmt.Fprintf(&b, "\t%s %s `json:\"%s%s\"`\n", snakeToUpperCamel(f.Name), goFieldType(f, enums), f.Name, jsonTag(f))
		}
		b.WriteString("}\n\n")
		genGoTypeDefaults(&b, t, enums)
	}

	for _, m := range s.Messages {
		genGoMessage(&b, m, enums)
	}

	body := b.String()
	var header strings.Builder
	header.WriteString("// Code generated from an FCT wire schema. DO NOT EDIT.\n")
	header.WriteString("// Source: SCHEMA_IDL_SCOPE.md Tier C (FCT-as-IDL).\n\n")
	fmt.Fprintf(&header, "package %s\n\n", packageName)
	needsJSON := strings.Contains(body, "json.")
	needsFmt := strings.Contains(body, "fmt.")
	if needsJSON || needsFmt {
		header.WriteString("import (\n")
		if needsJSON {
			header.WriteString("\t\"encoding/json\"\n")
		}
		if needsFmt {
			header.WriteString("\t\"fmt\"\n")
		}
		header.WriteString(")\n\n")
	}
	return header.String() + body, nil
}

func jsonTag(f ir.WireField) string {
	if f.Optional {
		return ",omitempty"
	}
	return ""
}

func goFieldType(f ir.WireField, enums map[string]bool) string {
	inner := goScalar(f.Type, enums, f.Ref)
	if f.List {
		return "[]" + inner
	}
	if f.Ref {
		// Always a pointer: a Go struct field naming another generated type
		// must be a pointer to be recursion-safe (self/mutual reference), and
		// a pointer already doubles as "may be absent" for the optional case
		// — Go has no separate Option<T>/Box<T> distinction to make.
		return "*" + inner
	}
	return inner
}

func goScalar(core string, enums map[string]bool, isRef bool) string {
	if isRef {
		return core
	}
	switch core {
	case "text", "date", "datetime":
		return "string"
	case "bool":
		return "bool"
	case "int", "money":
		return "int64"
	case "json":
		return "json.RawMessage"
	case "number", "float":
		return "float64"
	}
	if enums[core] {
		return core
	}
	return core
}

// goDefaultLiteral renders a schema default literal (already validated at
// IR-build time: a quoted string, a bare integer, true/false, `[]`, or `{}`)
// as the Go expression to pre-populate a field with. `[]` needs the field's
// own Go type to build a typed empty-slice composite literal
// (`[]EdgeSpec{}`); `{}` becomes an empty-object `json.RawMessage` (Go's
// `json` scalar type, see goScalar) — the same reason rust.go's
// rustDefaultLiteral special-cases both.
func goDefaultLiteral(f ir.WireField, enums map[string]bool) string {
	switch f.Default {
	case "[]":
		return goFieldType(f, enums) + "{}"
	case "{}":
		return `json.RawMessage("{}")`
	}
	return f.Default
}

// genGoTypeDefaults emits a custom UnmarshalJSON for a `type` that has at
// least one defaulted field — plain `encoding/json` has no concept of "value
// if the key is absent" the way Rust's `#[serde(default = "fn")]` does, so a
// Go struct with a real (non-zero) default needs this to behave the same way
// facetql's hand-written SequenceRequest.count (`#[serde(default = "one")]`)
// already does: pre-populate an alias of the struct with the defaults, then
// decode over it — json.Unmarshal only overwrites keys actually present, so
// an absent key keeps the preset default instead of falling back to Go's
// zero value. A type with no defaulted field emits nothing here; plain field
// decoding already does the right thing for it.
func genGoTypeDefaults(b *strings.Builder, t ir.WireType, enums map[string]bool) {
	var defaulted []ir.WireField
	for _, f := range t.Fields {
		if f.Default != "" {
			defaulted = append(defaulted, f)
		}
	}
	if len(defaulted) == 0 {
		return
	}
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error {\n", t.Name)
	fmt.Fprintf(b, "\ttype alias %s\n", t.Name)
	b.WriteString("\taux := alias{\n")
	for _, f := range defaulted {
		fmt.Fprintf(b, "\t\t%s: %s,\n", snakeToUpperCamel(f.Name), goDefaultLiteral(f, enums))
	}
	b.WriteString("\t}\n")
	b.WriteString("\tif err := json.Unmarshal(data, &aux); err != nil {\n\t\treturn err\n\t}\n")
	fmt.Fprintf(b, "\t*v = %s(aux)\n", t.Name)
	b.WriteString("\treturn nil\n}\n\n")
}

// genGoMessage writes the kitchen-sink struct, MarshalJSON, and UnmarshalJSON
// for one tagged union.
//
// Deliberate scope gap: a variant field's Default (if any) is not applied
// here the way genGoTypeDefaults applies a `type`'s. No variant in the real
// frozen wire surface has a defaulted field today (only two `type`s,
// SequenceRequest.count and the query family's item_var, do) — adding
// untested speculative handling for a case that doesn't exist yet would be
// exactly the kind of premature generality this project's own rules warn
// against. If a future message variant needs one, extend UnmarshalJSON's
// per-variant anonymous struct here the same way genGoTypeDefaults does.
func genGoMessage(b *strings.Builder, m ir.WireMessage, enums map[string]bool) {
	// Union of every variant's fields, deduplicated by (name,type) — two
	// variants may legitimately share a field name with the same type (e.g.
	// "address" in both insert_node and delete_node); a genuine name clash
	// with different types is a schema-author error the compiler should have
	// caught before codegen ever runs (fields are per-variant in the IR, so
	// nothing here re-validates that — it is out of scope for an emitter).
	type kv struct {
		name, typ string
	}
	seen := map[string]bool{}
	var union []kv
	for _, v := range m.Variants {
		for _, f := range v.Fields {
			k := kv{f.Name, goFieldType(f, enums)}
			key := k.name + "\x00" + k.typ
			if seen[key] {
				continue
			}
			seen[key] = true
			union = append(union, k)
		}
	}

	fmt.Fprintf(b, "type %s struct {\n", m.Name)
	b.WriteString("\tType string\n")
	for _, k := range union {
		fmt.Fprintf(b, "\t%s %s\n", snakeToUpperCamel(k.name), k.typ)
	}
	b.WriteString("}\n\n")

	// MarshalJSON: one switch arm per variant, each an anonymous struct
	// carrying only that variant's fields, so encoding never leaks a
	// different variant's zero value onto the wire as a stray key.
	fmt.Fprintf(b, "func (m %s) MarshalJSON() ([]byte, error) {\n", m.Name)
	b.WriteString("\tswitch m.Type {\n")
	for _, v := range m.Variants {
		fmt.Fprintf(b, "\tcase %q:\n", v.WireName())
		b.WriteString("\t\treturn json.Marshal(struct {\n")
		fmt.Fprintf(b, "\t\t\tType string `json:\"%s\"`\n", m.TagName())
		for _, f := range v.Fields {
			fmt.Fprintf(b, "\t\t\t%s %s `json:\"%s%s\"`\n", snakeToUpperCamel(f.Name), goFieldType(f, enums), f.Name, jsonTag(f))
		}
		b.WriteString("\t\t}{\n")
		fmt.Fprintf(b, "\t\t\tType: m.Type,\n")
		for _, f := range v.Fields {
			fmt.Fprintf(b, "\t\t\t%s: m.%s,\n", snakeToUpperCamel(f.Name), snakeToUpperCamel(f.Name))
		}
		b.WriteString("\t\t})\n")
	}
	fmt.Fprintf(b, "\tdefault:\n\t\treturn nil, fmt.Errorf(%q, m.Type)\n", "unknown "+m.Name+" variant %q")
	b.WriteString("\t}\n}\n\n")

	// UnmarshalJSON: probe the discriminant first, then decode only that
	// variant's fields — a decode of one variant never populates another
	// variant's fields with a zero value that looks like real data.
	fmt.Fprintf(b, "func (m *%s) UnmarshalJSON(data []byte) error {\n", m.Name)
	fmt.Fprintf(b, "\tvar probe struct {\n\t\tType string `json:\"%s\"`\n\t}\n", m.TagName())
	b.WriteString("\tif err := json.Unmarshal(data, &probe); err != nil {\n\t\treturn err\n\t}\n")
	b.WriteString("\tswitch probe.Type {\n")
	for _, v := range m.Variants {
		fmt.Fprintf(b, "\tcase %q:\n", v.WireName())
		b.WriteString("\t\tvar v struct {\n")
		for _, f := range v.Fields {
			fmt.Fprintf(b, "\t\t\t%s %s `json:\"%s%s\"`\n", snakeToUpperCamel(f.Name), goFieldType(f, enums), f.Name, jsonTag(f))
		}
		b.WriteString("\t\t}\n")
		b.WriteString("\t\tif err := json.Unmarshal(data, &v); err != nil {\n\t\t\treturn err\n\t\t}\n")
		fmt.Fprintf(b, "\t\t*m = %s{Type: probe.Type", m.Name)
		for _, f := range v.Fields {
			fmt.Fprintf(b, ", %s: v.%s", snakeToUpperCamel(f.Name), snakeToUpperCamel(f.Name))
		}
		b.WriteString("}\n\t\treturn nil\n")
	}
	fmt.Fprintf(b, "\tdefault:\n\t\treturn fmt.Errorf(%q, probe.Type)\n", "unknown "+m.Name+" variant %q")
	b.WriteString("\t}\n}\n\n")
}
