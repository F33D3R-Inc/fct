package fa

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file brings `when <event>:` blocks to life on the server.
//
// ADR-0005 says `when <event>` IS the handler, but until now the block was dead
// metadata: parsed, compiled into the manifest, and read by nobody at runtime —
// every app still hand-wrote `app.On`, hand-built the "Facet:key:val" facet-id
// string, and hand-assembled the Event. AutoWire reads the compiled `when` block
// and does all of that, so a handler is reduced to its one irreducible job:
// mutate server state and return the data to render. The op and the target come
// from the facet; the facet-id is read back out of the render itself, so it can
// never drift from the id the template actually emits.
//
// The supported ops mirror exactly what the client runtime (fa-runtime.js)
// applies: replace (outerHTML), append/prepend (insertAdjacentHTML into a
// container), and remove. replace_all is parsed by the language but the client
// has no case for it, so AutoWire rejects it at startup rather than emit a frame
// that silently does nothing.

// mutationRule is one line of a `when` block as seen at runtime: `<op> <target>
// [with <source>]`.
type mutationRule struct {
	op     string // replace | append | prepend | remove
	target string // facet whose node is the swap target (or container, for append)
	with   string // `with <Source>` — the item facet for append/prepend
}

// parseWhens indexes every facet's `when` blocks from the manifest by event
// name, so a client event type maps straight to the mutations it should drive.
func parseWhens(manifest []byte) (map[string][]mutationRule, error) {
	var m struct {
		Facets []struct {
			When []struct {
				Events    []string `json:"events"`
				Mutations []struct {
					Op     string `json:"op"`
					Target string `json:"target"`
					With   string `json:"with"`
				} `json:"mutations"`
			} `json:"when"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("fa: manifest: %w", err)
	}
	out := map[string][]mutationRule{}
	for _, f := range m.Facets {
		for _, w := range f.When {
			var muts []mutationRule
			for _, mu := range w.Mutations {
				muts = append(muts, mutationRule{op: mu.Op, target: mu.Target, with: mu.With})
			}
			for _, ev := range w.Events {
				out[ev] = append(out[ev], muts...)
			}
		}
	}
	return out, nil
}

// parseFacetIDPatterns reads each facet's data-facet-id pattern (e.g.
// "LikeButton:post:{post.id}", or just "Feed" for a singleton) from the manifest.
func parseFacetIDPatterns(manifest []byte) (map[string]string, error) {
	var m struct {
		Facets []struct {
			Name    string `json:"name"`
			FacetID string `json:"facet_id"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("fa: manifest: %w", err)
	}
	out := make(map[string]string, len(m.Facets))
	for _, f := range m.Facets {
		out[f.Name] = f.FacetID
	}
	return out, nil
}

// has reports whether a facet of this name was compiled.
func (c *Compiled) has(facet string) bool { return c.root.Lookup(facet) != nil }

// idOf renders a facet with data and returns the data-facet-id that render
// emits — the single source of truth for a parametric id like
// "Item:post:{post.id}" → "Item:post:p1". Used where the target is the very
// facet being rendered (replace) or where its concrete id is needed (remove).
func (c *Compiled) idOf(facet string, data any) (string, error) {
	frag, err := c.Render(facet, data)
	if err != nil {
		return "", err
	}
	id := facetIDOf(string(frag))
	if id == "" {
		return "", fmt.Errorf("rendered %q has no data-facet-id to target", facet)
	}
	return id, nil
}

// AutoWire registers a server handler for the client event by reading the
// `when <event>:` block compiled into c. The reducer does the only work the
// language can't express declaratively — mutate server state and return the data
// to render — and the runtime builds the SSE events from the facet's declared
// mutations. No hand-built facet-id strings, no Event envelopes, and the entire
// flow is server-rendered (the client never supplies the fragment, so there is
// no injection surface).
//
// It is additive: app.On still works for anything imperative. AutoWire panics at
// startup on a misconfigured `when` block (unknown facet, unsupported op, or a
// `with` clause where it makes no sense) so the mistake surfaces at boot, not on
// the first click.
func (a *App) AutoWire(c *Compiled, event string, reducer func(Ctx) (any, error)) *App {
	muts := c.whens[event]
	if len(muts) == 0 {
		panic(fmt.Sprintf("fa: AutoWire(%q): no `when %s:` block found in any compiled facet", event, event))
	}
	for _, m := range muts {
		bad := func(msg string) { panic(fmt.Sprintf("fa: AutoWire(%q): %s", event, msg)) }
		if !c.has(m.target) {
			bad(fmt.Sprintf("target %q is not a compiled facet", m.target))
		}
		switch m.op {
		case "replace", "remove":
			if m.with != "" {
				bad(fmt.Sprintf("`with` is only valid on append/prepend, not %s", m.op))
			}
		case "append", "prepend":
			if m.with == "" {
				bad(m.op + " needs `with <Facet>` (the item to add)")
			}
			if !c.has(m.with) {
				bad(fmt.Sprintf("`with %s` is not a compiled facet", m.with))
			}
			if strings.ContainsRune(c.idPatterns[m.target], '{') {
				bad(fmt.Sprintf("%s into the parametric container %q isn't supported (its id has holes); use app.On", m.op, m.target))
			}
		default:
			bad(fmt.Sprintf("op %q is not applied by the client runtime; use app.On", m.op))
		}
	}
	a.On(event, func(ctx Ctx) ([]Event, error) {
		data, err := reducer(ctx)
		if err != nil {
			return nil, err
		}
		events := make([]Event, 0, len(muts))
		for _, m := range muts {
			ev, err := c.eventFor(m, data)
			if err != nil {
				return nil, fmt.Errorf("fa: AutoWire(%q): %w", event, err)
			}
			events = append(events, ev)
		}
		return events, nil
	})
	return a
}

// eventFor builds the SSE Event for one mutation against the reducer's data.
func (c *Compiled) eventFor(m mutationRule, data any) (Event, error) {
	switch m.op {
	case "replace":
		// The new node replaces the old by outerHTML; the facet-id it carries IS
		// the target, so render once and read the id straight back out.
		frag, err := c.Render(m.target, data)
		if err != nil {
			return Event{}, err
		}
		id := facetIDOf(string(frag))
		if id == "" {
			return Event{}, fmt.Errorf("rendered %q has no data-facet-id to target", m.target)
		}
		return Event{Op: "replace", FacetID: id, Fragment: string(frag)}, nil
	case "append", "prepend":
		// Insert the rendered item into the container node, addressed by the
		// container's (hole-free, checked at startup) facet-id.
		frag, err := c.Render(m.with, data)
		if err != nil {
			return Event{}, err
		}
		return Event{Op: m.op, FacetID: c.idPatterns[m.target], Fragment: string(frag)}, nil
	case "remove":
		id, err := c.idOf(m.target, data)
		if err != nil {
			return Event{}, err
		}
		return Event{Op: "remove", FacetID: id}, nil
	default:
		return Event{}, fmt.Errorf("op %q unsupported", m.op) // unreachable; vetted at startup
	}
}

// facetIDOf reads the data-facet-id of a rendered fragment's root element — the
// first one in document order. This is the same id the client keys swaps on, so
// deriving the Event target from the render keeps the two in lockstep.
func facetIDOf(fragment string) string {
	const key = `data-facet-id="`
	i := strings.Index(fragment, key)
	if i < 0 {
		return ""
	}
	rest := fragment[i+len(key):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}
