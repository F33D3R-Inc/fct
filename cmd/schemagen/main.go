// schemagen is item 4 of SCHEMA_IDL_SCOPE.md's Tier C: the one command per
// repo that regenerates Rust/Go/TS wire types from the single FCT schema
// source, so the CI drift guard (compare committed output to a fresh run)
// has something to invoke and no consumer ever hand-edits a wire type again.
//
// Usage:
//
//	schemagen <schema.fct> <out-prefix> [go-package-name]
//
// Writes <out-prefix>.rs, <out-prefix>.go, and <out-prefix>.ts.
package main

import (
	"fmt"
	"go/format"
	"os"

	"facet/internal/codegen"
	"facet/internal/compile"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: schemagen <schema.fct> <out-prefix> [go-package-name]")
		os.Exit(2)
	}
	schemaPath, outPrefix := os.Args[1], os.Args[2]
	goPackage := "wire"
	if len(os.Args) > 3 {
		goPackage = os.Args[3]
	}

	src, err := os.ReadFile(schemaPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read schema:", err)
		os.Exit(1)
	}
	g, err := compile.String(string(src))
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile schema:", err)
		os.Exit(1)
	}
	schema := codegen.FromIR(g)

	rustOut, err := codegen.GenerateRust(schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate rust:", err)
		os.Exit(1)
	}
	goOut, err := codegen.GenerateGo(schema, goPackage)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate go:", err)
		os.Exit(1)
	}
	// Formatted so the CI drift guard's gofmt check (fct's own ci.yml) never
	// flags a generated file — canonical gofmt output, not merely valid Go.
	if formatted, err := format.Source([]byte(goOut)); err == nil {
		goOut = string(formatted)
	} else {
		fmt.Fprintln(os.Stderr, "warning: generated Go did not gofmt cleanly:", err)
	}
	tsOut, err := codegen.GenerateTS(schema)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate ts:", err)
		os.Exit(1)
	}

	writes := []struct {
		path, content string
	}{
		{outPrefix + ".rs", rustOut},
		{outPrefix + ".go", goOut},
		{outPrefix + ".ts", tsOut},
	}
	for _, w := range writes {
		if err := os.WriteFile(w.path, []byte(w.content), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write", w.path, ":", err)
			os.Exit(1)
		}
		fmt.Println("wrote", w.path)
	}
}
