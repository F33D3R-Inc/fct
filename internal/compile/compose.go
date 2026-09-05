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
// joinCSS concatenates stylesheet fragments (the playground's plus every facet's
// `css:` block) with a newline between them, so composed skin layers like structure.
func joinCSS(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}

func isLayered(facets []*ast.App) bool {
	for _, f := range facets {
		switch f.Kind {
		case "playground", "wireframe", "ui", "data":
			return true
		}
	}
	return false
}

// isPresentational reports whether a plain module contributes nothing but
// presentation — the vocabulary a layered build merges wholesale — and so can be
// pulled in alongside the bricks without a socket to live in.
//
// "Presentational" is defined by what the atom merge in compose actually carries:
// components, layouts, themes and a stylesheet. Requiring a *component* was too
// narrow and rejected modules whose whole contribution is one of the other three
// — a layout vocabulary that ships as CSS rules, a shared layout wrapper, a
// palette — even though a layered build merges all four identically. That is why
// a stylesheet could not be split out of a wireframe.
//
// The negative half must stay exhaustive over ast.App, because anything a module
// declares that this merge does not carry would be silently dropped rather than
// rejected. Records, webhooks and triggers were missing from it.
func isPresentational(f *ast.App) bool {
	contributes := len(f.Components) > 0 || len(f.Layouts) > 0 || f.CSS != "" ||
		len(f.Theme) > 0 || len(f.DarkTheme) > 0 || len(f.Themes) > 0
	return contributes &&
		len(f.Entities) == 0 && len(f.Records) == 0 && len(f.Enums) == 0 &&
		len(f.States) == 0 && len(f.Derives) == 0 && len(f.Policies) == 0 &&
		len(f.Actions) == 0 && len(f.Jobs) == 0 && len(f.Services) == 0 &&
		len(f.Webhooks) == 0 && len(f.Triggers) == 0 && len(f.Views) == 0 &&
		len(f.Mounts) == 0 && len(f.Sockets) == 0 && len(f.Frame) == 0 && !f.Auth
}

// compose snaps a set of typed bricks into one flat application graph. The shape
// is fixed by the architecture: a `playground` baseplate mounts one or more
// `wireframe`s as screens; each wireframe exposes typed `socket`s; `ui` and
// `data` facets snap into sockets whose declared kind matches. The compiler
// composites every brick's content into its wireframe frame — one screen per
// mount — and merges all data and logic into one graph. So the layering is how
// the app is built; each mount is a surface placement runs over, and the runtime
// routes between screens by their guards.
//
// A screen comes from one of two places, and those two are the whole answer to
// "who may declare a route in a layered build":
//
//   - a playground `mount`, which places a wireframe at a route. The baseplate
//     decides the app's shape.
//   - a brick `view`, which places one more screen of the wireframe that owns
//     the brick's socket, with that socket holding the view. The slice decides
//     the routes onto its own data.
//
// Without the second, the layered track could not express an app whose
// components link anywhere: a `data` facet could serve a timeline of PostCards
// but nothing could serve the `/post/:id` each card links to, and the link check
// in internal/ir rejected the build — correctly, since the route really was
// unserved. The gap was that there was no way to say otherwise.
func compose(facets []*ast.App) (*ast.App, error) {
	// 1. Partition the bricks by kind and enforce one baseplate. A plain module
	//    that is *purely presentational* (only components/layouts/theme/css) is a
	//    shareable atom set — it carries no data or logic to place, so it's pulled
	//    into the layered graph like a brick's components, letting one PostCard/
	//    Avatar or one stylesheet serve both a plain app and a layered build. A
	//    plain module with any data, logic, or views is still rejected — it would
	//    need a socket to live in.
	var playground *ast.App
	var wireframes, bricks, atoms []*ast.App
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
			if isPresentational(f) {
				atoms = append(atoms, f)
				continue
			}
			return nil, fmt.Errorf("plain `app` %q cannot be mixed into a layered build — make it a `ui`/`data` facet (or strip it to presentation: components, layouts, theme, css), or build from a playground", f.Name)
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
	app.DarkTheme = append(app.DarkTheme, playground.DarkTheme...)
	app.Themes = append(app.Themes, playground.Themes...)
	app.CSS = joinCSS(app.CSS, playground.CSS)
	knownPolicy := map[string]int{} // policy name -> param count, for guard checks
	for _, w := range wireframes {
		app.Theme = append(app.Theme, w.Theme...)
		app.DarkTheme = append(app.DarkTheme, w.DarkTheme...)
		app.Themes = append(app.Themes, w.Themes...)
		app.CSS = joinCSS(app.CSS, w.CSS)
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
		app.DarkTheme = append(app.DarkTheme, b.DarkTheme...)
		app.Themes = append(app.Themes, b.Themes...)
		app.CSS = joinCSS(app.CSS, b.CSS)
		for _, p := range b.Policies {
			knownPolicy[p.Name] = len(p.Params)
		}
	}
	// Shareable atom modules contribute only their components/layouts/theme — the
	// presentational vocabulary every brick can `use`.
	for _, a := range atoms {
		app.Components = append(app.Components, a.Components...)
		app.Layouts = append(app.Layouts, a.Layouts...)
		app.Theme = append(app.Theme, a.Theme...)
		app.DarkTheme = append(app.DarkTheme, a.DarkTheme...)
		app.Themes = append(app.Themes, a.Themes...)
		app.CSS = joinCSS(app.CSS, a.CSS)
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
		// The frame splice shares one traversal with the layout splice
		// (ast.SpliceLayout), so both answer "where may a slot appear" the same
		// way and neither can land a foreign tree in a repeating context.
		root, err := ast.SpliceFrame(w.Frame, fill[w])
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

	// 6. Each routed `view` a brick declares becomes one more screen — the same
	//    wireframe, with the brick's socket holding that view instead of the
	//    brick's `content`.
	//
	//    This is the answer to "who may declare a route in a layered build". It
	//    is the brick, because the brick is the vertical slice: `data Feed`
	//    owns the Tweet entity, the actions over it, and therefore every surface
	//    onto it — the timeline it puts in its socket and the single post at
	//    `/post/:id`. A component snapped in below (a PostCard linking to
	//    `/post/{id}`) is validated against those routes with every other link.
	//
	//    The alternative — the playground declaring the routes — was rejected:
	//    it makes the baseplate know every route of every brick it composes, so
	//    a brick stops being droppable (adding a data facet would mean editing
	//    the playground), and a `mount` takes a wireframe, so each detail route
	//    would need a whole wireframe of its own to hold content that belongs to
	//    a slice already present.
	//
	//    The shape mirrors the plain track exactly: `view X in Shell` wraps a
	//    routed view in a shared *layout*; a brick view is wrapped in the shared
	//    *wireframe*. Same chrome-plus-one-tree relation, one layer up.
	for _, b := range bricks {
		if len(b.Views) == 0 {
			continue
		}
		owner := socketOwner[b.Into] // step 3 already proved this resolves
		for _, v := range b.Views {
			if prev, dup := seenPath[v.Path]; dup {
				return nil, fmt.Errorf("two screens mount at %q (%q and %q)", v.Path, prev, v.Name)
			}
			seenPath[v.Path] = v.Name
			// The other sockets of this wireframe keep their fills — the screen
			// is the whole surface, not the view alone — so this is the frame's
			// fill map with one entry swapped. Copied rather than mutated: the
			// map is the mounted screen's too.
			swapped := make(map[string][]ast.Node, len(fill[owner])+1)
			for name, content := range fill[owner] {
				swapped[name] = content
			}
			swapped[b.Into] = v.Root
			root, err := ast.SpliceFrame(owner.Frame, swapped)
			if err != nil {
				return nil, err
			}
			app.Views = append(app.Views, &ast.View{
				Name:      v.Name,
				Path:      v.Path,
				Params:    v.Params,
				Requires:  v.Requires,
				Screen:    true,
				Root:      root,
				TitleSegs: v.TitleSegs,
				DescSegs:  v.DescSegs,
				Line:      v.Line,
			})
		}
	}
	// Composed marks this graph as the output of a layered build, so a route
	// diagnostic downstream can name the mechanism that declares one here.
	app.Composed = true
	return app, nil
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
