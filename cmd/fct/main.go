// Command fct is the Facet Architecture compiler and CLI.
//
// Subcommands: new (scaffold a project), dev (run, rebuilding on change),
// build (compile a .fct to template + manifest), parse/lex (debug the
// compiler), version. See README.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fct.dev/internal/codegen"
	"fct.dev/internal/lexer"
	"fct.dev/internal/parser"
)

const version = "0.0.0-walking-skeleton"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("fct", version)
	case "lex":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct lex <file.fct>")
			os.Exit(2)
		}
		if err := runLex(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "parse":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct parse <file.fct>")
			os.Exit(2)
		}
		if err := runParse(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "build":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct build <file.fct> [outdir]")
			os.Exit(2)
		}
		outDir := "generated"
		if len(os.Args) >= 4 {
			outDir = os.Args[3]
		}
		if err := runBuild(os.Args[2], outDir); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "check":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct check <file.fct|dir>")
			os.Exit(2)
		}
		if err := runCheck(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	case "fmt":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct fmt <file.fct|dir>")
			os.Exit(2)
		}
		if err := runFmt(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "add":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct add <pkg|url|path> [facets-dir]")
			os.Exit(2)
		}
		dir := "facets"
		if len(os.Args) >= 4 {
			dir = os.Args[3]
		}
		if err := runAdd(os.Args[2], dir); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "audit":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct audit <file.fct>")
			os.Exit(2)
		}
		if err := runAudit(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "new":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fct new <dir> [module]")
			os.Exit(2)
		}
		dir := os.Args[2]
		module := filepath.Base(dir)
		if len(os.Args) >= 4 {
			module = os.Args[3]
		}
		if err := runNew(dir, module); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "dev":
		dir := "."
		if len(os.Args) >= 3 {
			dir = os.Args[2]
		}
		if err := runDev(dir); err != nil {
			fmt.Fprintln(os.Stderr, "fct: "+err.Error())
			os.Exit(1)
		}
	case "lsp":
		if err := runLSP(); err != nil {
			fmt.Fprintln(os.Stderr, "fct lsp: "+err.Error())
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func runLex(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	toks, err := lexer.Lex(string(src))
	if err != nil {
		return err
	}
	for _, t := range toks {
		fmt.Println(t)
	}
	return nil
}

func runParse(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	facets, err := parser.Parse(string(src))
	if err != nil {
		return err
	}
	for _, f := range facets {
		fmt.Printf("facet %s  (facet-id: %s)\n", f.Name, f.DerivedFacetID())
		for _, fld := range f.Fields {
			kind := "prop"
			if fld.IsCustomType() {
				kind = "prop/custom"
			}
			fmt.Printf("  what    %s: %s (%s)\n", fld.Name, fld.Type, kind)
		}
		for _, w := range f.Whens {
			fmt.Printf("  when    %s\n", strings.Join(w.Events, ", "))
			for _, m := range w.Mutations {
				with := ""
				if m.With != "" {
					with = " with " + m.With
				}
				fmt.Printf("            %s %s%s\n", m.Op, m.Target, with)
			}
		}
		fmt.Printf("  looks   %d nodes\n", len(f.Looks))
	}
	return nil
}

func runBuild(path, outDir string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	facets, err := parser.Parse(string(src))
	if err != nil {
		return err
	}
	out, err := codegen.Generate(facets)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for name, tmpl := range out.Templates {
		fn := filepath.Join(outDir, codegen.TemplateFileName(name))
		if err := os.WriteFile(fn, []byte(tmpl), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", fn)
	}
	mf := filepath.Join(outDir, "manifest.json")
	if err := os.WriteFile(mf, out.Manifest, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", mf)

	// Typed data structs (#5): renderable with compile-time-checked data.
	pkg := sanitizePkg(filepath.Base(outDir))
	tf := filepath.Join(outDir, "types.go")
	if err := os.WriteFile(tf, []byte(codegen.GoStructs(pkg, facets)), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", tf)
	return nil
}

// sanitizePkg turns a directory name into a valid Go package identifier.
func sanitizePkg(s string) string {
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "facets"
	}
	return out
}

// runAudit compiles a .fct file and prints its access-control surface (the
// security manifest) — every facet's `who:` requirements and redactions, and
// which facets are public. Diffable in CI (audit C2 / §9.4).
func runAudit(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	facets, err := parser.Parse(string(src))
	if err != nil {
		return err
	}
	out, err := codegen.Generate(facets)
	if err != nil {
		return err
	}
	var m struct {
		Facets []struct {
			Name string `json:"name"`
			Who  *struct {
				Require []string `json:"require"`
				Redact  []struct {
					Field  string `json:"field"`
					Unless string `json:"unless"`
				} `json:"redact"`
			} `json:"who"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		return err
	}

	fmt.Printf("FA security manifest — %s\n\n", path)
	protected := 0
	var public []string
	for _, f := range m.Facets {
		if f.Who == nil {
			public = append(public, f.Name)
			continue
		}
		protected++
		fmt.Printf("  %s\n", f.Name)
		if len(f.Who.Require) > 0 {
			fmt.Printf("      require: %s\n", strings.Join(f.Who.Require, ", "))
		}
		for _, r := range f.Who.Redact {
			cond := "always"
			if r.Unless != "" {
				cond = "unless " + r.Unless
			}
			fmt.Printf("      redact %s (%s)\n", r.Field, cond)
		}
	}
	fmt.Printf("\n%d facet(s): %d protected, %d public\n", len(m.Facets), protected, len(public))
	if len(public) > 0 {
		fmt.Printf("public (no who:): %s\n", strings.Join(public, ", "))
	}
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "fct "+version)
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  fct new <dir> [module]   scaffold a runnable FA project")
	fmt.Fprintln(os.Stderr, "  fct dev [dir]            run, rebuilding on .fct change")
	fmt.Fprintln(os.Stderr, "  fct check <file|dir>     validate facets (parse + codegen + composition)")
	fmt.Fprintln(os.Stderr, "  fct fmt <file|dir>       format .fct files in place")
	fmt.Fprintln(os.Stderr, "  fct add <pkg|url|path>   install a facet package into facets/")
	fmt.Fprintln(os.Stderr, "  fct build <file.fct> [outdir]")
	fmt.Fprintln(os.Stderr, "  fct audit <file.fct>     print the access-control surface")
	fmt.Fprintln(os.Stderr, "  fct parse <file.fct>")
	fmt.Fprintln(os.Stderr, "  fct lex <file.fct>")
	fmt.Fprintln(os.Stderr, "  fct version")
}
