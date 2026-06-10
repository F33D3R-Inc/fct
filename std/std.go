// Package std is the Facet Architecture standard library: a set of ready-to-use
// facets (buttons, badges, avatars, alerts, cards, forms, …) so apps start from
// working components instead of a blank file.
//
// Compile the stdlib alongside your own facets:
//
//	c, _ := fa.Compile(std.Source() + myAppFDL)
//	// or with CompileDir:
//	c, _ := fa.Compile(std.Source() + readAll("facets"))
//
// Then use them by name (in your facets via <Button .../> / <Card>…</Card>, or
// directly via c.Render("Button", ...)). Serve std.CSS for the default theme.
package std

import (
	"embed"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/F33D3R-Inc/fct/fa"
)

//go:embed facets
var facetsFS embed.FS

//go:embed style.css
var CSS string

// Source returns every standard-library facet concatenated as FDL, ready to pass
// to fa.Compile alongside an app's own facets.
func Source() string {
	var files []string
	_ = fs.WalkDir(facetsFS, "facets", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".fct") {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		data, _ := facetsFS.ReadFile(f)
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

// CompileDir compiles every .fct file in dir on top of the standard library, so
// app facets can use the full catalog (Avatar, PostCard, LiveChat, …) by name.
// This is what a scaffolded app calls: fa.CompileDir("facets") but with the
// stdlib already available.
func CompileDir(dir string) (*fa.Compiled, error) {
	return fa.CompileDirWith(Source(), dir)
}

var facetRe = regexp.MustCompile(`(?m)^facet\s+([A-Za-z_][A-Za-z0-9_]*)\s*:`)

// Names lists every facet the standard library provides, sorted.
func Names() []string {
	var names []string
	for _, m := range facetRe.FindAllStringSubmatch(Source(), -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}
