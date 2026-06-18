// Package compile is the front-to-IR pipeline: source text → ast → IR. It is
// the one entry point both the CLI and the runtime use, so there is exactly one
// definition of what an application means.
package compile

import (
	"facet/internal/ir"
	"facet/internal/parser"
)

// String compiles Facet source to the IR.
func String(src string) (*ir.IR, error) {
	app, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	return ir.Build(app)
}
