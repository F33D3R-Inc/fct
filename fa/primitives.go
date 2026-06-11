// Per-primitive runtime semantics (round 1). The compiler records each
// primitive's declarative extras (order/throttle/window/ttl/states) in the
// manifest; this file gives them behavior on the server:
//
//	feed      → SortFeed orders items by the declared `order:` before render
//	stream    → `throttle:` is enforced in the hub (trailing-edge coalescing);
//	            `window:` is enforced by the client runtime (DOM trim)
//	pipe      → `throttle:` as stream
//	lifecycle → Lifecycle validates `states:` transitions (fail closed)
//	signal    → Signal relays an ephemeral payload to channel subscribers;
//	            nothing is stored, and the client expires it after `ttl:`
//	vault     → decrypted and rendered ONLY in the client runtime (by design,
//	            there is nothing for the server to do here — see README)
//	media     → `source:` is interpreted by the client runtime (player mount)
package fa

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/F33D3R-Inc/fct/internal/codegen"
)

// facetMeta is the runtime view of one manifest entry: the per-kind declarative
// extras with durations/counts parsed into typed values.
type facetMeta struct {
	name     string
	kind     string
	order    string        // feed: "field" or "field asc|desc"
	throttle time.Duration // stream | pipe
	window   int           // stream
	ttl      time.Duration // signal
	states   []string      // lifecycle
}

// parseFacetMeta extracts per-facet runtime rules from manifest.json bytes,
// keyed by facet name. Durations use Go syntax (200ms, 5s); a malformed value is
// an error so a bad declaration fails at startup, not silently at runtime.
func parseFacetMeta(manifest []byte) (map[string]facetMeta, error) {
	var m struct {
		Facets []struct {
			Name     string   `json:"name"`
			Kind     string   `json:"kind"`
			Order    string   `json:"order"`
			Throttle string   `json:"throttle"`
			Window   string   `json:"window"`
			TTL      string   `json:"ttl"`
			States   []string `json:"states"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, fmt.Errorf("fa: manifest: %w", err)
	}
	rules := make(map[string]facetMeta, len(m.Facets))
	for _, f := range m.Facets {
		fm := facetMeta{name: f.Name, kind: f.Kind, order: f.Order, states: f.States}
		var err error
		if f.Throttle != "" {
			if fm.throttle, err = time.ParseDuration(f.Throttle); err != nil || fm.throttle <= 0 {
				return nil, fmt.Errorf("fa: %s %s: invalid throttle %q (use a Go duration like 200ms)", f.Kind, f.Name, f.Throttle)
			}
		}
		if f.Window != "" {
			if fm.window, err = strconv.Atoi(f.Window); err != nil || fm.window <= 0 {
				return nil, fmt.Errorf("fa: %s %s: invalid window %q (use a positive count)", f.Kind, f.Name, f.Window)
			}
		}
		if f.TTL != "" {
			if fm.ttl, err = time.ParseDuration(f.TTL); err != nil || fm.ttl <= 0 {
				return nil, fmt.Errorf("fa: %s %s: invalid ttl %q (use a Go duration like 5s)", f.Kind, f.Name, f.TTL)
			}
		}
		rules[f.Name] = fm
	}
	return rules, nil
}

// facetName extracts the facet name from a facet-id instance: "LikeButton:post:42"
// → "LikeButton"; a singleton id ("LiveChat") is already the name.
func facetName(facetID string) string {
	if i := strings.IndexByte(facetID, ':'); i >= 0 {
		return facetID[:i]
	}
	return facetID
}

// ── feed: ordering ──────────────────────────────────────────────────────────

// SortFeed sorts items — a slice (or pointer to slice) of structs, struct
// pointers, or maps — by the feed's declared `order:` field, in place. A bare
// field sorts DESCENDING (a feed is a ranked list: best/newest first); append
// `asc` for ascending. Fails closed: an item without the field, or a
// non-comparable field type, is an error and the slice is left untouched.
func (c *Compiled) SortFeed(facet string, items any) error {
	meta, ok := c.meta[facet]
	if !ok {
		return fmt.Errorf("fa: no facet %q compiled", facet)
	}
	if meta.kind != "feed" {
		return fmt.Errorf("fa: %s is a %s, not a feed", facet, meta.kind)
	}
	if meta.order == "" {
		return nil // no order: declared — the app's order stands
	}
	field, desc := meta.order, true
	if parts := strings.Fields(meta.order); len(parts) == 2 {
		field = parts[0]
		switch parts[1] {
		case "asc":
			desc = false
		case "desc":
		default:
			return fmt.Errorf("fa: feed %s: invalid order direction %q (use asc or desc)", facet, parts[1])
		}
	} else if len(parts) != 1 {
		return fmt.Errorf("fa: feed %s: invalid order %q (use `field` or `field asc|desc`)", facet, meta.order)
	}

	v := reflect.ValueOf(items)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Slice {
		return fmt.Errorf("fa: SortFeed: items must be a slice, got %T", items)
	}
	n := v.Len()
	keys := make([]reflect.Value, n)
	for i := 0; i < n; i++ {
		k, err := fieldValue(v.Index(i), field)
		if err != nil {
			return fmt.Errorf("fa: feed %s, item %d: %w", facet, i, err)
		}
		keys[i] = k
	}
	// Validate comparability across all keys before touching the slice.
	for i := 1; i < n; i++ {
		if _, err := compareValues(keys[0], keys[i]); err != nil {
			return fmt.Errorf("fa: feed %s: %w", facet, err)
		}
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		c, _ := compareValues(keys[idx[a]], keys[idx[b]])
		if desc {
			return c > 0
		}
		return c < 0
	})
	sorted := reflect.MakeSlice(v.Type(), n, n)
	for to, from := range idx {
		sorted.Index(to).Set(v.Index(from))
	}
	reflect.Copy(v, sorted)
	return nil
}

// fieldValue resolves an FDL field name on one feed item: a struct field (via
// the same snake_case→Go mapping codegen uses, so `avatar_url` finds AvatarURL)
// or a map key (raw FDL name first, Go name second).
func fieldValue(item reflect.Value, field string) (reflect.Value, error) {
	for item.Kind() == reflect.Pointer || item.Kind() == reflect.Interface {
		if item.IsNil() {
			return reflect.Value{}, fmt.Errorf("nil item")
		}
		item = item.Elem()
	}
	switch item.Kind() {
	case reflect.Struct:
		f := item.FieldByName(codegen.GoName(field))
		if !f.IsValid() {
			return reflect.Value{}, fmt.Errorf("no field %q (looked for struct field %s)", field, codegen.GoName(field))
		}
		return f, nil
	case reflect.Map:
		for _, key := range []string{field, codegen.GoName(field)} {
			f := item.MapIndex(reflect.ValueOf(key))
			if f.IsValid() {
				return f, nil
			}
		}
		return reflect.Value{}, fmt.Errorf("no key %q in item map", field)
	default:
		return reflect.Value{}, fmt.Errorf("item is %s; need a struct or map", item.Kind())
	}
}

// compareValues compares two order keys: numbers numerically, strings
// lexically, time.Time chronologically. Returns <0, 0, >0; an error if the
// values are not comparable.
func compareValues(a, b reflect.Value) (int, error) {
	for a.Kind() == reflect.Interface {
		a = a.Elem()
	}
	for b.Kind() == reflect.Interface {
		b = b.Elem()
	}
	if at, aok := a.Interface().(time.Time); aok {
		bt, bok := b.Interface().(time.Time)
		if !bok {
			return 0, fmt.Errorf("cannot compare time.Time with %s", b.Type())
		}
		return at.Compare(bt), nil
	}
	if x, ok := toFloat(a.Interface()); ok {
		y, yok := toFloat(b.Interface())
		if !yok {
			return 0, fmt.Errorf("cannot compare number with %s", b.Type())
		}
		switch {
		case x < y:
			return -1, nil
		case x > y:
			return 1, nil
		}
		return 0, nil
	}
	if a.Kind() == reflect.String && b.Kind() == reflect.String {
		return strings.Compare(a.String(), b.String()), nil
	}
	return 0, fmt.Errorf("order field type %s is not comparable (need number, string, or time.Time)", a.Type())
}

// ── lifecycle: state machine ────────────────────────────────────────────────

// Lifecycle is the validated state machine a `lifecycle` primitive declares via
// `states:`. Transitions move FORWARD one declared state at a time (placed →
// paid → shipped); anything else — an undeclared state, a skip, a move backward
// — is rejected. An app needing branch transitions (e.g. cancellation) models
// them in its own logic on top of Valid.
type Lifecycle struct {
	facet  string
	states []string
}

// Lifecycle returns the state machine for a lifecycle primitive. It is an error
// if the facet is not a lifecycle or declares no states.
func (c *Compiled) Lifecycle(facet string) (*Lifecycle, error) {
	meta, ok := c.meta[facet]
	if !ok {
		return nil, fmt.Errorf("fa: no facet %q compiled", facet)
	}
	if meta.kind != "lifecycle" {
		return nil, fmt.Errorf("fa: %s is a %s, not a lifecycle", facet, meta.kind)
	}
	if len(meta.states) == 0 {
		return nil, fmt.Errorf("fa: lifecycle %s declares no states", facet)
	}
	return &Lifecycle{facet: facet, states: meta.states}, nil
}

// States returns the declared states in order.
func (l *Lifecycle) States() []string { return append([]string(nil), l.states...) }

// Initial returns the first declared state.
func (l *Lifecycle) Initial() string { return l.states[0] }

// Valid reports whether state is one of the declared states.
func (l *Lifecycle) Valid(state string) bool { return l.index(state) >= 0 }

// Next returns the state after current. It is an error if current is not a
// declared state or is terminal (the last state).
func (l *Lifecycle) Next(current string) (string, error) {
	i := l.index(current)
	if i < 0 {
		return "", fmt.Errorf("fa: lifecycle %s: unknown state %q (states: %s)", l.facet, current, strings.Join(l.states, ", "))
	}
	if i == len(l.states)-1 {
		return "", fmt.Errorf("fa: lifecycle %s: %q is terminal", l.facet, current)
	}
	return l.states[i+1], nil
}

// CanTransition reports whether from → to is a legal step (forward by exactly
// one declared state).
func (l *Lifecycle) CanTransition(from, to string) bool {
	i := l.index(from)
	return i >= 0 && i+1 < len(l.states) && l.states[i+1] == to
}

func (l *Lifecycle) index(state string) int {
	for i, s := range l.states {
		if s == state {
			return i
		}
	}
	return -1
}

// ── signal: ephemeral relay ─────────────────────────────────────────────────

// Signal relays an ephemeral payload to a channel's subscribers on behalf of a
// declared `signal` primitive. The payload is pushed as a signed `signal` event
// and NEVER stored — there is no history, no replay; a subscriber who wasn't
// connected never sees it. The client runtime applies it to elements marked
// data-fa-signal and expires it after the signal's declared `ttl:`. Fails
// closed: facetID must belong to a compiled `signal` primitive.
func (a *App) Signal(channel, facetID string, payload map[string]string) error {
	meta, ok := a.rules[facetName(facetID)]
	if !ok {
		return fmt.Errorf("fa: no primitive %q in the manifest", facetName(facetID))
	}
	if meta.kind != "signal" {
		return fmt.Errorf("fa: %s is a %s, not a signal", meta.name, meta.kind)
	}
	js, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	a.hub.EmitChannel(channel, Event{Op: "signal", FacetID: facetID, Fragment: string(js)})
	return nil
}

// Signal relays an ephemeral signal payload from a handler. See App.Signal.
func (c Ctx) Signal(channel, facetID string, payload map[string]string) error {
	return c.app.Signal(channel, facetID, payload)
}
