package runtime

import (
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// An app declared `view Admin at "/admin"`. It compiled, `facet routes` printed
// `GET /admin  Admin  requires staff`, and every request to /admin got the
// runtime's generated console — because the built-ins are registered ahead of the
// view router. Nothing anywhere said so; the author found out by opening the page
// and moved the view to /manage.
//
// The report is what was missing, so this pins the report: the shadow is found
// against the same table Handler registers from, it is named at boot, and it is
// not raised for a route no built-in claims. The last assertion is the one that
// keeps the report honest — the built-in really does answer that path, so a
// warning here is never a false alarm.
func TestShadowedRouteIsReportedAtBoot(t *testing.T) {
	g, err := compile.String(`app Shop:
    entity Product:
        id: int
    view Admin at "/admin":
        box:
            text "my own console"
    view Manage at "/manage":
        box:
            text "not shadowed"
`)
	if err != nil {
		t.Fatal(err)
	}

	// newServer runs inside NewInMemory, and the logger takes os.Stderr as it is
	// built, so the capture has to be in place first.
	restore := fqCaptureStderr(t)
	srv, err := NewInMemory(g)
	logged := restore()
	if err != nil {
		t.Fatal(err)
	}

	shadows := ShadowedRoutes(g)
	if len(shadows) != 1 {
		t.Fatalf("ShadowedRoutes = %+v, want exactly the /admin one", shadows)
	}
	got := shadows[0]
	if got.Route != "/admin" || got.View != "Admin" || got.Builtin.Pattern != "/admin" {
		t.Errorf("shadow = %+v, want /admin / Admin / builtin /admin", got)
	}
	// The boot notice must name the view, the path and what is taking it — an
	// author who cannot tell which of their routes is dead is no better off.
	for _, want := range []string{"Admin", "/admin", "admin console"} {
		if !strings.Contains(logged, want) {
			t.Errorf("boot log does not mention %q:\n%s", want, logged)
		}
	}
	// A warning, not a refusal: an app that has been serving for months must not
	// stop booting because the runtime learned to notice this.
	if srv == nil {
		t.Fatal("a shadowed route must not prevent the server from starting")
	}
	if strings.Contains(logged, "/manage") {
		t.Errorf("a route no built-in claims must not be reported:\n%s", logged)
	}

	// And the report is true: the path really does reach the built-in, never the
	// view. Unauthenticated, the admin console refuses — the view would have
	// rendered its own text.
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	if page := string(httpGetBytes(t, ts.URL+"/admin")); strings.Contains(page, "my own console") {
		t.Errorf("/admin reached the app's view, so the shadow report would be wrong:\n%s", page)
	}
	if page := string(httpGetBytes(t, ts.URL+"/manage")); !strings.Contains(page, "not shadowed") {
		t.Errorf("/manage must reach the app's view:\n%s", page)
	}

	// The exported table is what cmd/ reports against, so it has to describe the
	// mux Handler actually built: subtree patterns claim what is under them.
	if b, ok := builtinClaiming("/api/orders"); !ok || b.Pattern != "/api/" {
		t.Errorf("a path under a subtree built-in must be claimed by it, got %+v ok=%v", b, ok)
	}
	if _, ok := builtinClaiming("/:slug"); ok {
		t.Error("a route that is dynamic from its first segment claims no fixed path and must not be reported")
	}
	if len(BuiltinRoutes()) != len(builtins) {
		t.Error("BuiltinRoutes must expose the whole table cmd/ has to report against")
	}
}
