package codegen

import (
	"fmt"
	"strings"

	"facet/internal/ir"
)

// GenerateRust emits serde-derived Rust types matching the shape
// facetql/src/api/routes.rs already hand-writes: a plain struct per `type`,
// and a `#[serde(tag = "type", rename_all = "snake_case")]` enum per
// `message` — an internally-tagged enum is serde's native tagged-union
// representation and needs no custom (De)Serialize impl, unlike Go's
// discriminated-struct workaround.
func GenerateRust(s Schema) (string, error) {
	var b strings.Builder
	b.WriteString("// Code generated from an FCT wire schema. DO NOT EDIT.\n")
	b.WriteString("// Source: SCHEMA_IDL_SCOPE.md Tier C (FCT-as-IDL).\n\n")
	b.WriteString("use serde::{Deserialize, Serialize};\n\n")

	for _, e := range s.Enums {
		fmt.Fprintf(&b, "#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]\n")
		fmt.Fprintf(&b, "pub enum %s {\n", e.Name)
		// An explicit `#[serde(rename = "...")]` per variant, not a blanket
		// `rename_all = "snake_case"`: a value's wire form is whatever
		// enumWireName resolves to (its own lowercase text by default, or an
		// `as "Wire"` override — e.g. Visibility's real, already-shipped
		// "Private"/"Public", which snake_case could never produce). Explicit
		// per-variant rename means both cases go through one code path with
		// no special-casing at the emitter level.
		for i, v := range e.Values {
			fmt.Fprintf(&b, "    #[serde(rename = %q)]\n", enumWireName(e, i))
			fmt.Fprintf(&b, "    %s,\n", snakeToUpperCamel(v))
		}
		b.WriteString("}\n\n")
	}

	enums := enumNameSet(s.Enums)
	var defaultFns strings.Builder
	for _, t := range s.Types {
		if t.Query {
			// Bound from a URL query string (axum's `Query<T>`, e.g.
			// facetql's real QueryParams) — never sent/received as a JSON
			// body, so it derives Deserialize only. `Query<T>` itself needs
			// nothing more than that to work.
			b.WriteString("/// Bound from the URL query string, never a JSON body.\n")
			b.WriteString("#[derive(Debug, Clone, Deserialize)]\n")
		} else {
			b.WriteString("#[derive(Debug, Clone, Serialize, Deserialize)]\n")
		}
		fmt.Fprintf(&b, "pub struct %s {\n", t.Name)
		for _, f := range t.Fields {
			writeRustFieldAttrs(&b, "    ", f, t.Name, enums, &defaultFns)
			fmt.Fprintf(&b, "    pub %s: %s,\n", rustIdent(f.Name), rustFieldType(f, enums))
		}
		b.WriteString("}\n\n")
	}

	for _, m := range s.Messages {
		b.WriteString("#[derive(Debug, Clone, Serialize, Deserialize)]\n")
		fmt.Fprintf(&b, "#[serde(tag = %q, rename_all = \"snake_case\")]\n", m.TagName())
		fmt.Fprintf(&b, "pub enum %s {\n", m.Name)
		for _, v := range m.Variants {
			if v.Wire != "" {
				fmt.Fprintf(&b, "    #[serde(rename = %q)]\n", v.Wire)
			}
			fmt.Fprintf(&b, "    %s {\n", snakeToUpperCamel(v.Name))
			for _, f := range v.Fields {
				writeRustFieldAttrs(&b, "        ", f, m.Name+"_"+v.Name, enums, &defaultFns)
				fmt.Fprintf(&b, "        %s: %s,\n", rustIdent(f.Name), rustFieldType(f, enums))
			}
			b.WriteString("    },\n")
		}
		b.WriteString("}\n\n")
	}
	b.WriteString(defaultFns.String())
	return b.String(), nil
}

// rustDefaultLiteral renders a schema default literal (already validated at
// IR-build time: a quoted string, a bare integer, true/false, `[]`, or `{}`)
// as the Rust expression a generated `fn default_...() -> T` should return.
// No enum/ref case is needed here because a default is only ever allowed on
// bool/int/money/number/text/date/json, or a list ([]) — ir/build.go's
// resolveWireField enforces this before codegen ever sees it.
func rustDefaultLiteral(lit, rustType string) string {
	switch lit {
	case "[]":
		return "Vec::new()"
	case "{}":
		return "serde_json::json!({})"
	}
	if rustType == "String" {
		return lit + ".to_string()"
	}
	return lit
}

// writeRustFieldAttrs writes the `#[serde(...)]` line(s) a field needs: an
// explicit `rename` when the field name collides with a Rust keyword (so the
// wire key stays correct regardless of how serde derives a raw identifier's
// default name — not assumed, made explicit), `skip_serializing_if` when the
// field is optional, and `default = "fn"` (plus the generated fn itself,
// appended to defaultFns) when the field declared a real default — combined
// into one attribute when more than one applies. A defaulted field is never
// also Optional (ir/build.go enforces this), so the two default-handling
// paths never collide.
func writeRustFieldAttrs(b *strings.Builder, indent string, f ir.WireField, declName string, enums map[string]bool, defaultFns *strings.Builder) {
	var opts []string
	if rustKeywords[f.Name] {
		opts = append(opts, fmt.Sprintf("rename = %q", f.Name))
	}
	if f.Default != "" {
		fnName := fmt.Sprintf("default_%s_%s", strings.ToLower(declName), f.Name)
		opts = append(opts, fmt.Sprintf("default = %q", fnName))
		rt := rustFieldType(f, enums)
		fmt.Fprintf(defaultFns, "fn %s() -> %s { %s }\n", fnName, rt, rustDefaultLiteral(f.Default, rt))
	}
	if f.Optional {
		opts = append(opts, `skip_serializing_if = "Option::is_none"`, "default")
	}
	if len(opts) > 0 {
		fmt.Fprintf(b, "%s#[serde(%s)]\n", indent, strings.Join(opts, ", "))
	}
}

// rustKeywords are the field/variant names FCT is likely to produce that
// collide with a Rust reserved word — `where` is the real one in the frozen
// wire surface (TxOp.delete_where's predicate field). A raw identifier
// (`r#where`) sidesteps the collision; the explicit `#[serde(rename)]` above
// guarantees the wire key regardless.
var rustKeywords = map[string]bool{
	"as": true, "break": true, "const": true, "continue": true, "crate": true,
	"else": true, "enum": true, "extern": true, "false": true, "fn": true,
	"for": true, "if": true, "impl": true, "in": true, "let": true, "loop": true,
	"match": true, "mod": true, "move": true, "mut": true, "pub": true, "ref": true,
	"return": true, "self": true, "Self": true, "static": true, "struct": true,
	"super": true, "trait": true, "true": true, "type": true, "unsafe": true,
	"use": true, "where": true, "async": true, "await": true, "dyn": true,
}

func rustIdent(name string) string {
	if rustKeywords[name] {
		return "r#" + name
	}
	return name
}

func rustFieldType(f ir.WireField, enums map[string]bool) string {
	inner := rustScalar(f.Type, enums, f.Ref)
	switch {
	case f.Map:
		inner = "std::collections::HashMap<String, " + inner + ">"
	case f.List:
		for i := 0; i < max(f.Depth, 1); i++ {
			inner = "Vec<" + inner + ">"
		}
	case f.Ref:
		// A single (non-list) self/mutual reference must be boxed: without
		// indirection the struct would have infinite size the moment two
		// Types/Messages refer to each other or to themselves (Expr.l: Expr).
		inner = "Box<" + inner + ">"
	}
	if f.Optional || f.Nullable {
		inner = "Option<" + inner + ">"
	}
	return inner
}

func rustScalar(core string, enums map[string]bool, isRef bool) string {
	if isRef {
		return core // another generated struct/enum, referenced by name
	}
	switch core {
	case "text", "datetime":
		return "String"
	case "bool":
		return "bool"
	case "float":
		return "f64"
	case "int", "money":
		// The wire schema has no bit-width concept (JSON numbers don't have
		// one); a real cutover narrows a specific field further (e.g.
		// Coordinate's u8 components) with its own validation, same as any
		// hand-written type already must.
		return "i64"
	case "date":
		// A JSON string (ISO-8601/RFC3339) on the wire in every target
		// language — chosen so Rust/Go/TS agree on the wire shape, not just
		// on having *a* representation. See tsScalar/goScalar for the same
		// choice made the same way.
		return "String"
	case "json":
		return "serde_json::Value"
	case "number":
		return "f64"
	}
	if enums[core] {
		return core
	}
	return core
}
