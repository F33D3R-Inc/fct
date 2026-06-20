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
// is fixed by the architecture: a `playground` baseplate mounts one or more
// `wireframe`s as screens; each wireframe exposes typed `socket`s; `ui` and
// `data` facets snap into sockets whose declared kind matches. The compiler
// composites every brick's content into its wireframe frame — one screen per
// mount — and merges all data and logic into one graph. So the layering is how
// the app is built; each mount is a surface placement runs over, and the runtime
// routes between screens by their guards.
func compose(facets []*ast.App) (*ast.App, error) {
	// 1. Partition the bricks by kind and enforce one baseplate.
	var playground *ast.App
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

	// 2. Index every wireframe and build one global socket table. Socket names are
	//    unique across the whole app, so a brick's `in <socket>` names exactly one
	//    region without having to say which wireframe.
	byWireframe := map[string]*ast.App{}
	socketOwner := map[string]*ast.App{} // socket name -> wireframe that declares it
	socketAccept := map[string]string{}  // socket name -> accepted kind
	var socketOrder []string
	for _, w := range wireframes {
		if _, dup := byWireframe[w.Name]; dup {
			return nil, fmt.Errorf("two wireframes are both named %q", w.Name)
		}
		byWireframe[w.Name] = w
		for _, s := range w.Sockets {
			if owner, dup := socketOwner[s.Name]; dup {
				return nil, fmt.Errorf("socket %q is declared by both wireframe %q and %q — socket names must be unique", s.Name, owner.Name, w.Name)
			}
			socketOwner[s.Name] = w
			socketAccept[s.Name] = s.Accept
			socketOrder = append(socketOrder, s.Name)
		}
	}

	// 3. Snap each brick into its socket, checking the studs line up. Content is
	//    accumulated per wireframe so each screen fills its own frame.
	fill := map[*ast.App]map[string][]ast.Node{} // wireframe -> socket -> content
	for _, b := range bricks {
		owner, ok := socketOwner[b.Into]
		if !ok {
			return nil, fmt.Errorf("%s facet %q snaps into socket %q, but no wireframe declares it%s",
				b.Kind, b.Name, b.Into, socketHint(socketOrder))
		}
		if socketAccept[b.Into] != b.Kind {
			return nil, fmt.Errorf("socket %q accepts `%s` facets, but %q is a `%s` facet — the bricks don't fit",
				b.Into, socketAccept[b.Into], b.Name, b.Kind)
		}
		if fill[owner] == nil {
			fill[owner] = map[string][]ast.Node{}
		}
		fill[owner][b.Into] = append(fill[owner][b.Into], b.Content...)
	}

	// 4. Flatten every brick's data and logic into one graph. The playground's
	//    theme is the base; wireframes and then bricks layer on top (later entries
	//    win per key), so skin composes the same way structure does.
	app := &ast.App{Name: playground.Name, Kind: "app", Line: playground.Line}
	app.Auth = playground.Auth
	app.Theme = append(app.Theme, playground.Theme...)
	knownPolicy := map[string]int{} // policy name -> param count, for guard checks
	for _, w := range wireframes {
		app.Theme = append(app.Theme, w.Theme...)
	}
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
		app.Services = append(app.Services, b.Services...)
		app.Theme = append(app.Theme, b.Theme...)
		for _, p := range b.Policies {
			knownPolicy[p.Name] = len(p.Params)
		}
	}

	// 5. Each playground mount becomes one screen: its wireframe's frame composited
	//    with the bricks snapped into it, served at the mount's route behind its
	//    guard. The runtime redirects a failed guard to the first enterable screen.
	if len(playground.Mounts) == 0 {
		return nil, fmt.Errorf("playground %q mounts no wireframe", playground.Name)
	}
	seenPath := map[string]string{}
	for _, m := range playground.Mounts {
		w, ok := byWireframe[m.Wireframe]
		if !ok {
			return nil, fmt.Errorf("playground %q mounts wireframe %q, which was not found (did you import it?)", playground.Name, m.Wireframe)
		}
		if m.Requires != "" {
			n, ok := knownPolicy[m.Requires]
			if !ok {
				return nil, fmt.Errorf("screen %q (at %q) requires policy %q, which is not defined in any data facet", m.Wireframe, m.Path, m.Requires)
			}
			if n != 0 {
				return nil, fmt.Errorf("screen guard %q must be a zero-argument policy", m.Requires)
			}
		}
		if prev, dup := seenPath[m.Path]; dup {
			return nil, fmt.Errorf("two screens mount at %q (%q and %q)", m.Path, prev, m.Wireframe)
		}
		seenPath[m.Path] = m.Wireframe
		root, err := resolveFrame(w.Frame, fill[w])
		if err != nil {
			return nil, err
		}
		app.Views = append(app.Views, &ast.View{
			Name:     m.Wireframe,
			Path:     m.Path,
			Requires: m.Requires,
			Screen:   true,
			Root:     root,
			Line:     w.Line,
		})
	}
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
