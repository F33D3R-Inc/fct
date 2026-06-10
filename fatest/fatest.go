// Package fatest is a testing harness for Facet Architecture apps — the analogue
// of net/http/httptest. It lets you unit-test what a facet renders and what an
// event handler does, with no live server or browser.
//
//	html := fatest.Render(t, src, "LikeButton", map[string]any{...})
//
//	app := fa.New(manifest); app.On("post.like", h)
//	events := fatest.Dispatch(t, app, "post.like", map[string]string{"postId": "1"})
//	fatest.AssertFragment(t, events, "post:1", "active")
package fatest

import (
	"strings"
	"testing"

	"github.com/F33D3R-Inc/fct/fa"
)

// Render compiles FDL source and renders a (public) facet, failing the test on
// any error. Use it to assert on a facet's HTML.
func Render(t testing.TB, src, facet string, data map[string]any) string {
	t.Helper()
	c, err := fa.Compile(src)
	if err != nil {
		t.Fatalf("fatest: compile: %v", err)
	}
	h, err := c.Render(facet, data)
	if err != nil {
		t.Fatalf("fatest: render %q: %v", facet, err)
	}
	return string(h)
}

// RenderFor renders a who:-protected facet for a viewer, enforcing its policies.
func RenderFor(t testing.TB, c *fa.Compiled, v fa.View, facet string, data map[string]any) (string, error) {
	t.Helper()
	h, err := c.RenderFor(v, facet, data)
	return string(h), err
}

// Dispatch runs an event through its handler (and guard) and returns the events
// the handler produced, failing the test on error.
func Dispatch(t testing.TB, app *fa.App, eventType string, payload map[string]string) []fa.Event {
	t.Helper()
	return DispatchAs(t, app, eventType, payload, "")
}

// DispatchAs is Dispatch with an explicit actor identity (for guard/authz tests).
func DispatchAs(t testing.TB, app *fa.App, eventType string, payload map[string]string, identity string) []fa.Event {
	t.Helper()
	events, err := app.Dispatch(eventType, payload, identity)
	if err != nil {
		t.Fatalf("fatest: dispatch %q: %v", eventType, err)
	}
	return events
}

// AssertFragment fails the test unless some pushed event targets a facet-id
// containing idPart and whose fragment contains each want substring.
func AssertFragment(t testing.TB, events []fa.Event, idPart string, wants ...string) {
	t.Helper()
	for _, e := range events {
		if !strings.Contains(e.FacetID, idPart) {
			continue
		}
		ok := true
		for _, w := range wants {
			if !strings.Contains(e.Fragment, w) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
	}
	t.Fatalf("no event targeting %q with fragments %v; got %+v", idPart, wants, events)
}
