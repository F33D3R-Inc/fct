package compile

import (
	"fmt"
	"sort"
	"strings"

	"facet/internal/ast"
)

// isLayered reports whether a collected facet set uses the typed-brick model
// (any facet declares a kind beyond a plain `app`). The entry facet of a layered
// build is always the `playground` baseplate.
func isLayered(facets []*ast.App) bool {
	for _, f := range facets {
		switch f.Kind {
		case "playground", "wireframe", "ui", "data":
			return true
		}
	}
	return false
}

// compose snaps a set of typed bricks into one flat application graph. The shape
// is fixed by the architecture: a single `playground` baseplate mounts one
// `wireframe`; the wireframe exposes typed `socket`s; `ui` and `data` facets snap
// into sockets whose declared kind matches. The compiler then composites every
// brick's content into the wireframe frame and merges all data and logic into one
// graph — so the layering is how the app is built, and the output is a single
// surface placement runs over exactly once.
func compose(facets []*ast.App) (*ast.App, error) {
	// 1. Partition the bricks by kind and enforce one baseplate.
	var playground, frame *ast.App
	var wireframes, bricks []*ast.App
	for _, f := range facets {
		switch f.Kind {
		case "playground":
			if playground != nil {
				return nil, fmt.Errorf("two playgrounds (%q and %q) — an app has exactly one baseplate", playground.Name, f.Name)
			}
			playground = f
		case "wireframe":
			wireframes = append(wireframes, f)
		case "ui", "data":
			bricks = append(bricks, f)
		case "", "app":
			return nil, fmt.Errorf("plain `app` %q cannot be mixed into a layered build — make it a `ui`/`data` facet, or build from a playground", f.Name)
		}
	}
	if playground == nil {
		return nil, fmt.Errorf("a layered app needs a `playground` baseplate to build from")
	}

	// 2. The playground mounts exactly one wireframe (and accepts nothing else).
	for _, w := range wireframes {
		if w.Name == playground.Mount {
			frame = w
			break
		}
	}
	if frame == nil {
		return nil, fmt.Errorf("playground %q mounts wireframe %q, which was not found (did you import it?)", playground.Name, playground.Mount)
	}

	// 3. Index the wireframe's typed sockets.
	sockets := map[string]string{} // name -> accepted kind
	var socketOrder []string
	for _, s := range frame.Sockets {
		if _, dup := sockets[s.Name]; dup {
			return nil, fmt.Errorf("wireframe %q declares socket %q twice", frame.Name, s.Name)
		}
		sockets[s.Name] = s.Accept
		socketOrder = append(socketOrder, s.Name)
	}

	// 4. Snap each brick into its socket, checking the studs line up.
	fill := map[string][]ast.Node{}
	for _, b := range bricks {
		accept, ok := sockets[b.Into]
		if !ok {
			return nil, fmt.Errorf("%s facet %q snaps into socket %q, but wireframe %q has no such socket%s",
				b.Kind, b.Name, b.Into, frame.Name, socketHint(socketOrder))
		}
		if accept != b.Kind {
			return nil, fmt.Errorf("socket %q accepts `%s` facets, but %q is a `%s` facet — the bricks don't fit",
				b.Into, accept, b.Name, b.Kind)
		}
		fill[b.Into] = append(fill[b.Into], b.Content...)
	}

	// 5. Composite the surface: the wireframe frame with each `slot <name>`
	//    replaced by the content of the bricks snapped into that socket.
	root, err := resolveFrame(frame.Frame, fill)
	if err != nil {
		return nil, err
	}

	// 6. Flatten every brick's data and logic into one graph. The playground's
	//    theme is the base; the wireframe and then the bricks layer on top (later
	//    entries win per key), so skin composes the same way structure does.
	app := &ast.App{Name: playground.Name, Kind: "app", Line: playground.Line}
	app.Auth = playground.Auth
	app.Theme = append(app.Theme, playground.Theme...)
	app.Theme = append(app.Theme, frame.Theme...)
	for _, b := range bricks {
		app.Auth = app.Auth || b.Auth
		app.Entities = append(app.Entities, b.Entities...)
		app.Enums = append(app.Enums, b.Enums...)
		app.States = append(app.States, b.States...)
		app.Derives = append(app.Derives, b.Derives...)
		app.Policies = append(app.Policies, b.Policies...)
		app.Actions = append(app.Actions, b.Actions...)
		app.Jobs = append(app.Jobs, b.Jobs...)
		app.Components = append(app.Components, b.Components...)
		app.Layouts = append(app.Layouts, b.Layouts...)
		app.Theme = append(app.Theme, b.Theme...)
	}

	// 7. The composited frame is the single surface, served at "/".
	app.Views = []*ast.View{{Name: "Surface", Path: "/", Root: root, Line: frame.Line}}
	return app, nil
}

// resolveFrame returns a plain node tree: a copy of the wireframe frame with each
// SlotRef replaced by its socket's composited content. It recurses through the
// layout containers so a `slot` may sit at any depth.
func resolveFrame(nodes []ast.Node, fill map[string][]ast.Node) ([]ast.Node, error) {
	var out []ast.Node
	for _, n := range nodes {
		switch t := n.(type) {
		case ast.SlotRef:
			out = append(out, fill[t.Name]...) // an unfilled socket renders nothing
		case ast.Box:
			kids, err := resolveFrame(t.Children, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Box{Children: kids})
		case ast.Row:
			kids, err := resolveFrame(t.Children, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.Row{Children: kids})
		case ast.If:
			body, err := resolveFrame(t.Body, fill)
			if err != nil {
				return nil, err
			}
			out = append(out, ast.If{Cond: t.Cond, Body: body})
		case ast.For:
			body, err := resolveFrame(t.Body, fill)
			if err != nil {
				return nil, err
			}
			t.Body = body
			out = append(out, t)
		case ast.Slot:
			return nil, fmt.Errorf("a wireframe frame uses `slot <name>`, not a bare `slot`")
		default:
			out = append(out, n)
		}
	}
	return out, nil
}

// socketHint lists the available socket names for a "no such socket" error.
func socketHint(names []string) string {
	if len(names) == 0 {
		return " (the wireframe declares no sockets)"
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return " (sockets: " + strings.Join(sorted, ", ") + ")"
}
