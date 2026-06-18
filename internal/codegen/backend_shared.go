package codegen

import (
	"unicode"

	"github.com/F33D3R-Inc/fct/internal/ast"
)

// inputFields returns a facet's what: fields that the caller must supply — the
// plain input props, excluding computed (=) fields, which are derived at render
// time. Every backend's Types emission iterates these (mirrors GoStructs).
func inputFields(f *ast.Facet) []ast.Field {
	var inputs []ast.Field
	for _, fld := range f.Fields {
		if !fld.IsComputed() {
			inputs = append(inputs, fld)
		}
	}
	return inputs
}

// upper upper-cases a single rune (ASCII-safe head-of-word helper for camel/Pascal
// casing in the backends).
func upper(r rune) rune { return unicode.ToUpper(r) }
