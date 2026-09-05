package integration

import (
	"strings"
	"testing"
)

// `route` is the path being rendered, and it exists so a nav inside a shared
// layout can mark itself active without every page threading `current` down by
// hand. The component below is the whole reason: it is used identically by both
// views and reaches a different conclusion in each, from nothing but where it is.
const routeApp = `app Nav:
    component NavLink(label: text, to: text):
        if route == to:
            box class "fa-active":
                link "{label}" -> "{to}"
        if route != to:
            link "{label}" -> "{to}"

    component Bar:
        row:
            use NavLink("Home", "/")
            use NavLink("Settings", "/settings")

    view Home at "/":
        box:
            use Bar()
            text "at {route}"

    view Settings at "/settings":
        box:
            use Bar()
            text "at {route}"
`

// The server knows the route it is rendering; the client must reach the same
// answer after hydration, or a nav flickers from one active item to another the
// instant the page comes alive.
//
// It reaches it by not computing it: `route` is bound once, server-side, and
// rides to the client in the same state payload a route parameter does. That is
// the whole design — there is no second implementation to disagree with.
func TestTheRouteBeingRenderedIsReadableAndAgreesOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, routeApp)

	for _, path := range []string{"/", "/settings"} {
		t.Run(path, func(t *testing.T) {
			code, page := a.get(path)
			if code != 200 {
				t.Fatalf("GET %s: %d", path, code)
			}

			markup := mountedMarkup(page)
			// The route reaches an interpolation like any other value (it arrives in
			// a tracked bind span, so this asks the rendered text, not the markup).
			if want := "at" + path; !strings.Contains(serverText(page), want) {
				t.Errorf("the page rendered at %s does not read %q:\n%s", path, want, markup)
			}

			// Exactly one nav item is active, and it is this page's. Both `if`s over
			// `route` had to evaluate, and to opposite answers.
			if n := strings.Count(markup, "fa-active"); n != 1 {
				t.Errorf("%d active nav items on %s, want exactly 1", n, path)
			}
			active := markup[strings.Index(markup, "fa-active"):]
			if href := active[:min(len(active), 120)]; !strings.Contains(href, `href="`+path+`"`) {
				t.Errorf("the active nav item on %s does not link to it: %s", path, href)
			}

			// The client renders the same page, character for character — which it
			// can only do if it read the same route.
			run, _ := runClient(t, page, nil)
			if want := serverText(page); run.Text != want {
				t.Errorf("client text %q != server text %q", run.Text, want)
			}
		})
	}
}
