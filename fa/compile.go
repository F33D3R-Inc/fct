package fa

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/F33D3R-Inc/fct/internal/codegen"
	"github.com/F33D3R-Inc/fct/internal/parser"
)

// Compiled holds the result of compiling FDL: all facets in one shared
// html/template set, plus the manifest. This is the public entry point to the
// compiler for applications — they import "github.com/F33D3R-Inc/fct/fa", never the internal
// compiler packages (Go forbids external internal imports).
type Compiled struct {
	root     *template.Template // all facets, one set, so child calls resolve
	Manifest []byte
	auth     map[string]facetAuth       // facet → who: requirements (protected facets)
	policies map[string]func(View) bool // policy name → implementation
	meta     map[string]facetMeta       // facet → per-kind runtime rules (primitives.go)
}

// Compile compiles FDL source (one or more facets) into renderable templates.
// All facets share one template set so child-facet calls — {{template "Avatar"}}
// — resolve across facets, with the faData helper available to build child data.
func Compile(src string) (*Compiled, error) {
	facets, err := parser.Parse(src)
	if err != nil {
		return nil, err
	}
	out, err := codegen.Generate(facets)
	if err != nil {
		return nil, err
	}
	// Per-set funcmap: faData builds child data; faSlot renders a slot's fill
	// template with the parent's data (closure over the set being built); the
	// arithmetic funcs back the FDL +/-/*//% operators (comparisons and boolean
	// use html/template's builtins eq/ne/lt/le/gt/ge/and/or/not).
	var root *template.Template
	funcs := template.FuncMap{
		"faData": faData,
		"faSlot": func(name string, data any) (template.HTML, error) {
			var buf bytes.Buffer
			if err := root.ExecuteTemplate(&buf, name, data); err != nil {
				return "", err
			}
			return template.HTML(buf.String()), nil
		},
		"add": arith("add", func(a, b float64) float64 { return a + b }),
		"sub": arith("sub", func(a, b float64) float64 { return a - b }),
		"mul": arith("mul", func(a, b float64) float64 { return a * b }),
		"div": arith("div", func(a, b float64) float64 { return a / b }),
		"mod": func(a, b any) (any, error) {
			x, ok1 := toFloat(a)
			y, ok2 := toFloat(b)
			if !ok1 || !ok2 || int64(y) == 0 {
				return nil, fmt.Errorf("mod: bad operands")
			}
			return int64(x) % int64(y), nil
		},
	}
	root = template.New("").Funcs(funcs)
	for name, ts := range out.Templates {
		if _, err := root.New(name).Parse(ts); err != nil {
			return nil, fmt.Errorf("facet %s: generated template is invalid: %w", name, err)
		}
	}
	for name, ts := range out.Aux { // internal slot-fill templates
		if _, err := root.New(name).Parse(ts); err != nil {
			return nil, fmt.Errorf("slot template invalid: %w", err)
		}
	}
	// Typed per-kind runtime rules (feed order, stream throttle/window, signal
	// ttl, lifecycle states). A malformed duration/count fails compilation here
	// rather than misbehaving silently at runtime.
	meta, err := parseFacetMeta(out.Manifest)
	if err != nil {
		return nil, err
	}
	c := &Compiled{
		root:     root,
		Manifest: out.Manifest,
		auth:     make(map[string]facetAuth),
		policies: make(map[string]func(View) bool),
		meta:     meta,
	}
	// Record who: requirements so protected facets can only render via RenderFor.
	for _, f := range facets {
		if f.HasWho() {
			a := facetAuth{require: f.Who.Require}
			for _, r := range f.Who.Redactions {
				a.redactions = append(a.redactions, redaction{field: r.Field, unless: r.UnlessPolicy})
			}
			c.auth[f.Name] = a
		}
	}
	return c, nil
}

// arith adapts a float op into a template func over any numeric values. Whole
// results render without a decimal point (float64(6) prints "6").
func arith(name string, op func(a, b float64) float64) func(a, b any) (any, error) {
	return func(a, b any) (any, error) {
		x, ok1 := toFloat(a)
		y, ok2 := toFloat(b)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%s: non-numeric operand", name)
		}
		return op(x, y), nil
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// faData builds a child facet's data map from the (key, value) pairs the
// compiler emits for a child-facet call. Keys are Title-cased field names.
func faData(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("faData: odd argument count")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("faData: key %d is not a string", i)
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}

// CompileDir compiles every *.fct file in dir (sorted) as one facet set. This is
// what a scaffolded app calls at startup so it runs from .fct with no build step.
func CompileDir(dir string) (*Compiled, error) {
	return CompileDirWith("", dir)
}

// CompileDirWith compiles every .fct file in dir, prepended with prelude source.
// The prelude is typically the standard library (std.Source()), so app facets can
// use Avatar, PostCard, LiveChat, … directly. Pass "" for no prelude (identical
// to CompileDir).
func CompileDirWith(prelude, dir string) (*Compiled, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no .fct files in %s", dir)
	}
	sort.Strings(names)

	var b strings.Builder
	if prelude != "" {
		b.WriteString(prelude)
		b.WriteByte('\n')
	}
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return Compile(b.String())
}

// Render executes a PUBLIC facet's template with data. A facet that declares a
// `who:` block is refused here — it must go through RenderFor(view, …) so its
// authorization is enforced. Child-facet calls render in the same set.
func (c *Compiled) Render(facet string, data any) (template.HTML, error) {
	if _, protected := c.auth[facet]; protected {
		return "", fmt.Errorf("facet %q declares who:; render it with RenderFor(view, …)", facet)
	}
	return c.render(facet, data)
}

// render executes a facet without authorization checks (internal).
func (c *Compiled) render(facet string, data any) (template.HTML, error) {
	if c.root.Lookup(facet) == nil {
		return "", fmt.Errorf("no facet %q compiled", facet)
	}
	var buf bytes.Buffer
	if err := c.root.ExecuteTemplate(&buf, facet, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// MustRender is Render that panics on error — for app code that knows the facet
// exists and the data shape is correct.
func (c *Compiled) MustRender(facet string, data any) template.HTML {
	h, err := c.Render(facet, data)
	if err != nil {
		panic(err)
	}
	return h
}
