package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/F33D3R-Inc/fct/internal/ast"
	"github.com/F33D3R-Inc/fct/internal/codegen"
	"github.com/F33D3R-Inc/fct/internal/parser"
)

// compileCheck parses + generates FDL, returning the facets or the first error.
func compileCheck(src string) ([]*ast.Facet, error) {
	facets, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	if _, err := codegen.Generate(facets); err != nil {
		return nil, err
	}
	return facets, nil
}

// fctFiles returns the .fct files for a path: the file itself, or every .fct in
// a directory (sorted).
func fctFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	var files []string
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
			files = append(files, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no .fct files in %s", path)
	}
	return files, nil
}

// readFctSource concatenates the FDL for a file or directory (facets in a dir
// must compile together because they may reference each other).
func readFctSource(path string) (string, error) {
	files, err := fctFiles(path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// runCheck validates a .fct file or directory: full parse + codegen + composition
// checks, reporting the first error with its position, or OK. Exit 1 on failure.
func runCheck(path string) error {
	src, err := readFctSource(path)
	if err != nil {
		return err
	}
	facets, err := parser.Parse(src)
	if err != nil {
		return fmt.Errorf("✗ %s: %v", path, err)
	}
	if _, err := codegen.Generate(facets); err != nil {
		return fmt.Errorf("✗ %s: %v", path, err)
	}
	fmt.Printf("✓ %s — %d facet(s) OK\n", path, len(facets))
	return nil
}

// runFmt normalises .fct files in place: trims trailing whitespace, collapses
// runs of blank lines, and ensures a single trailing newline. Conservative — it
// never reflows HTML or changes structure.
func runFmt(path string) error {
	files, err := fctFiles(path)
	if err != nil {
		return err
	}
	changed := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		out := fmtFDL(string(data))
		if out != string(data) {
			if err := os.WriteFile(f, []byte(out), 0o644); err != nil {
				return err
			}
			fmt.Println("formatted", f)
			changed++
		}
	}
	if changed == 0 {
		fmt.Println("already formatted")
	}
	return nil
}

func fmtFDL(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blanks := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" {
			blanks++
			if blanks > 1 {
				continue // collapse multiple blank lines to one
			}
		} else {
			blanks = 0
		}
		out = append(out, ln)
	}
	// drop trailing blank lines, ensure exactly one final newline
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}
