// Package codegen emits Rust, Go, and TypeScript types from FCT's wire-schema
// declarations (`type`/`message`, internal/ir's WireType/WireMessage) — the
// codegen half of Tier C (SCHEMA_IDL_SCOPE.md): the FacetQL wire protocol is
// authored once, in FCT, and every language consumer is generated from that
// one source instead of hand-copied. See internal/ast.Type's doc comment for
// why this is a distinct construct from Record.
//
// Each emitter is intentionally dumb: it walks the already-validated IR (name
// resolution, duplicate checks, and Ref-vs-scalar classification all happened
// in internal/ir/build.go) and prints. No emitter re-validates the schema.
package codegen

import (
	"strings"

	"facet/internal/ir"
)

// Schema is the subset of an IR relevant to wire codegen — enums, types, and
// messages, in declaration order (callers should compile a single dedicated
// wire-schema .fct file, so declaration order is the author's, not sorted).
type Schema struct {
	Enums    []ir.Enum
	Types    []ir.WireType
	Messages []ir.WireMessage
}

// FromIR extracts a Schema from a compiled IR, ignoring everything the wire
// schema doesn't own (entities, views, actions, ...) — a schema file compiles
// as an ordinary (view-less) FCT app, so its IR carries a full App's worth of
// fields most of which are simply empty.
func FromIR(g *ir.IR) Schema {
	return Schema{Enums: g.Enums, Types: g.Types, Messages: g.Messages}
}

func enumNameSet(enums []ir.Enum) map[string]bool {
	m := make(map[string]bool, len(enums))
	for _, e := range enums {
		m[e.Name] = true
	}
	return m
}

// enumWireName returns the value's actual wire-serialized text — its
// `as "Wire"` override if it declared one, its own lowercase text otherwise.
// Defensive against a nil/short WireNames (only possible if something bypasses
// the parser to construct an ir.Enum by hand, e.g. an old test fixture): falls
// back to the value itself rather than panicking or emitting an empty string.
func enumWireName(e ir.Enum, i int) string {
	if i < len(e.WireNames) && e.WireNames[i] != "" {
		return e.WireNames[i]
	}
	return e.Values[i]
}

// snakeToUpperCamel turns a variant's wire discriminant ("insert_node") into
// an exported Go/Rust/TS type-name fragment ("InsertNode").
func snakeToUpperCamel(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}
