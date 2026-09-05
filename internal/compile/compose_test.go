package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"facet/internal/ir"
)

// writeProject drops a set of .fct files into a fresh dir and returns the path of
// the named entry file, so a test can compile a whole layered build from disk.
func writeProject(t *testing.T, files map[string]string, entry string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, entry)
}

// a minimal but complete layered app: playground → wireframe(nav:ui, feed:data,
// aside:ui) → Nav(ui), Feed(data), Trends(ui).
func layeredFiles() map[string]string {
	return map[string]string{
		"playground.fct": `import "wireframe.fct"

playground X:
    auth
    theme:
        accent "#1d9bf0"
        card-border "#333"
    mount Shell
`,
		"wireframe.fct": `import "nav.fct"
import "feed.fct"
import "aside.fct"

wireframe Shell:
    socket nav: ui
    socket feed: data
    socket aside: ui
    frame:
        row:
            box:
                slot nav
            box:
                slot feed
            box:
                slot aside
`,
		"nav.fct": `ui Nav in nav:
    content:
        text "F33D3R"
        link "Home" -> "/"
`,
		"feed.fct": `data Feed in feed:
    entity Tweet:
        id: int
        author: text
        body: text
        created: int
    state draft: text = "" @client
    derive tweetCount: int = count(Tweet)
    policy member:
        actor != "guest"
    action tweet(body: text):
        requires member
        add Tweet { author: actor, body: body, created: now() }
    content:
        text "Home · {tweetCount} tweets"
        for t in Tweet by created desc limit 50:
            box:
                text "{t.author}: {t.body}"
`,
		"aside.fct": `ui Trends in aside:
    content:
        text "What's happening"
`,
	}
}

func TestLayeredComposeSucceeds(t *testing.T) {
	entry := writeProject(t, layeredFiles(), "playground.fct")
	ir, err := File(entry)
	if err != nil {
		t.Fatalf("compose failed: %v", err)
	}
	if ir.App != "X" {
		t.Errorf("composed app name = %q, want X", ir.App)
	}
	// The data facet's entity must have merged into the one graph.
	var hasTweet bool
	for _, e := range ir.Entities {
		if e.Name == "Tweet" {
			hasTweet = true
		}
	}
	if !hasTweet {
		t.Error("entity Tweet from the data facet did not merge into the graph")
	}
	// Exactly one composited surface, served at "/".
	if len(ir.Pages) != 1 || ir.Pages[0].Path != "/" {
		t.Fatalf("want one page at /, got %d pages", len(ir.Pages))
	}
	// The action and its `member` policy gate survived composition.
	var hasTweetAction bool
	for _, a := range ir.Actions {
		if a.Name == "tweet" {
			hasTweetAction = true
			if len(a.Requires) == 0 {
				t.Error("tweet action lost its `requires member` gate through composition")
			}
		}
	}
	if !hasTweetAction {
		t.Error("action tweet from the data facet did not merge")
	}
}

func TestSnapKindMismatch(t *testing.T) {
	files := layeredFiles()
	// Make the feed socket demand a ui facet; Feed is a data facet → no fit.
	files["wireframe.fct"] = strings.Replace(files["wireframe.fct"], "socket feed: data", "socket feed: ui", 1)
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "don't fit") {
		t.Fatalf("want a kind-mismatch error, got %v", err)
	}
}

func TestSnapUnknownSocket(t *testing.T) {
	files := layeredFiles()
	files["aside.fct"] = `ui Trends in sidebar:
    content:
        text "x"
`
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "no wireframe declares it") {
		t.Fatalf("want an unknown-socket error, got %v", err)
	}
}

func TestPlaygroundMountMissing(t *testing.T) {
	files := layeredFiles()
	files["playground.fct"] = strings.Replace(files["playground.fct"], "mount Shell", "mount Nope", 1)
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a missing-wireframe error, got %v", err)
	}
}

func TestUIFacetMayNotDeclareEntity(t *testing.T) {
	files := layeredFiles()
	files["nav.fct"] = `ui Nav in nav:
    entity Secret:
        id: int
    content:
        text "x"
`
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "may not declare an entity") {
		t.Fatalf("want a ui-entity error, got %v", err)
	}
}

func TestPlainAppStillFlatMerges(t *testing.T) {
	// No typed bricks: the original flat-merge path must be untouched.
	files := map[string]string{
		"app.fct": `import "posts.fct"

app Blog:
    view Home at "/":
        box:
            text "hi"
`,
		"posts.fct": `app Posts:
    entity Post:
        id: int
        body: text
`,
	}
	entry := writeProject(t, files, "app.fct")
	ir, err := File(entry)
	if err != nil {
		t.Fatalf("plain app compile failed: %v", err)
	}
	if ir.App != "Blog" {
		t.Errorf("app name = %q, want Blog", ir.App)
	}
	var hasPost bool
	for _, e := range ir.Entities {
		if e.Name == "Post" {
			hasPost = true
		}
	}
	if !hasPost {
		t.Error("imported entity Post did not flat-merge")
	}
}

// a two-screen layered app: a guest-only login screen and a member-only home,
// each its own wireframe, guarded by policies a data facet defines.
func screensFiles() map[string]string {
	return map[string]string{
		"playground.fct": `import "login.fct"
import "home.fct"

playground X:
    auth
    mount Auth at "/login" requires guest
    mount Home at "/" requires member
`,
		"login.fct": `import "loginform.fct"

wireframe Auth:
    socket form: ui
    frame:
        box:
            slot form
`,
		"loginform.fct": `ui LoginForm in form:
    state username: text = "" @client
    content:
        text "Sign in"
        button "log in" -> login(username, username)
`,
		"home.fct": `import "feed.fct"

wireframe Home:
    socket main: data
    frame:
        box:
            slot main
`,
		"feed.fct": `data Feed in main:
    entity Tweet:
        id: int
        body: text
    policy guest:
        actor == "guest"
    policy member:
        actor != "guest"
    content:
        text "home feed"
`,
	}
}

func TestScreensCompose(t *testing.T) {
	entry := writeProject(t, screensFiles(), "playground.fct")
	ir, err := File(entry)
	if err != nil {
		t.Fatalf("screens compose failed: %v", err)
	}
	if len(ir.Pages) != 2 {
		t.Fatalf("want 2 screens, got %d", len(ir.Pages))
	}
	want := map[string]string{"/login": "guest", "/": "member"}
	for _, p := range ir.Pages {
		guard, ok := want[p.Path]
		if !ok {
			t.Errorf("unexpected screen path %q", p.Path)
			continue
		}
		if p.Requires != guard {
			t.Errorf("screen %q guard = %q, want %q", p.Path, p.Requires, guard)
		}
		if !p.Screen {
			t.Errorf("screen %q not marked as a screen", p.Path)
		}
		delete(want, p.Path)
	}
	if len(want) != 0 {
		t.Errorf("missing screens: %v", want)
	}
}

func TestScreenGuardUnknownPolicy(t *testing.T) {
	files := screensFiles()
	files["playground.fct"] = strings.Replace(files["playground.fct"], "requires member", "requires nobody", 1)
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "not defined in any data facet") {
		t.Fatalf("want an unknown-guard-policy error, got %v", err)
	}
}

func TestSocketNamesUniqueAcrossWireframes(t *testing.T) {
	files := screensFiles()
	// Make both wireframes declare a socket named "main" → collision.
	files["login.fct"] = strings.Replace(files["login.fct"], "socket form: ui", "socket main: ui", 1)
	files["loginform.fct"] = strings.Replace(files["loginform.fct"], "in form", "in main", 1)
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "must be unique") {
		t.Fatalf("want a duplicate-socket error, got %v", err)
	}
}

func TestLayeredComponentOnlyAtomMerges(t *testing.T) {
	files := layeredFiles()
	// A shareable atom: a plain, component-only module — no data/logic/views. The
	// data brick imports it and `use`s its component, the same file a plain app
	// would import.
	files["card.fct"] = `app Atoms:
    component Card(label: text):
        box:
            text "{label}"
`
	files["feed.fct"] = `import "card.fct"
data Feed in feed:
    entity Tweet:
        id: int
        author: text
        body: text
    content:
        for t in Tweet limit 50:
            use Card(t.body)
`
	entry := writeProject(t, files, "playground.fct")
	ir, err := File(entry)
	if err != nil {
		t.Fatalf("a component-only atom should merge into a layered build, got: %v", err)
	}
	var hasCard bool
	for _, c := range ir.Components {
		if c.Name == "Card" {
			hasCard = true
		}
	}
	if !hasCard {
		t.Error("the atom's Card component did not merge into the layered graph")
	}
}

func TestLayeredErrorOnPlainAppMixedIn(t *testing.T) {
	files := layeredFiles()
	// Replace a ui brick with a plain app — it can't be mixed into a layered build.
	files["nav.fct"] = `app Stray:
    view X at "/x":
        box:
            text "x"
`
	entry := writeProject(t, files, "playground.fct")
	_, err := File(entry)
	if err == nil || !strings.Contains(err.Error(), "cannot be mixed") {
		t.Fatalf("want a mixed-app error, got %v", err)
	}
}

// TestLayeredBrickViewServesARoute is the composition-model check: a brick may
// declare a routed `view`, that view becomes a screen of the wireframe owning
// its socket (so the other sockets' content is still around it), and a link into
// that route — written inside a shared component, several files down — validates
// against it.
//
// Before this the layered track had no way to declare a route at all, so a
// component that linked anywhere could not be composed into a playground build.
func TestLayeredBrickViewServesARoute(t *testing.T) {
	files := layeredFiles()
	files["feed.fct"] = strings.Replace(files["feed.fct"],
		`    content:
        text "Home · {tweetCount} tweets"`,
		`    component Row(id: int, body: text):
        link "{body}" -> "/post/{id}"
    view Post at "/post/:id":
        for t in Tweet where t.id == id by id desc limit 1:
            text "{t.body}"
    content:
        text "Home · {tweetCount} tweets"
        for t in Tweet by id desc limit 5:
            use Row(t.id, t.body)`, 1)

	built, err := File(writeProject(t, files, "playground.fct"))
	if err != nil {
		t.Fatalf("a brick view should serve the route its component links to, got: %v", err)
	}

	var page *ir.Page
	for i := range built.Pages {
		if built.Pages[i].Path == "/post/:id" {
			page = &built.Pages[i]
		}
	}
	if page == nil {
		t.Fatal("the brick's view did not become a routed screen")
	}
	if !page.Screen || len(page.Params) != 1 || page.Params[0] != "id" {
		t.Errorf("want a composed screen with the `id` route parameter bound, got screen=%v params=%v", page.Screen, page.Params)
	}
	// The view takes its own socket's place; every other socket keeps its
	// content, because a screen is the whole surface and not the view alone.
	if !rendersLiteral(page.View, "F33D3R") || !rendersLiteral(page.View, "What's happening") {
		t.Error("the brick view's screen lost the wireframe's other sockets")
	}
	if rendersLiteral(page.View, "Home · ") {
		t.Error("the brick view's screen still shows the socket's default `content`")
	}
}

// TestLayeredUnservedRouteIsStillAnError pins the half of the guarantee the
// model must not weaken: declaring routes is now possible, so a link to one
// nobody declared is a hard failure, and the message names what wanted it.
func TestLayeredUnservedRouteIsStillAnError(t *testing.T) {
	files := layeredFiles()
	files["nav.fct"] = `ui Nav in nav:
    component Crumb():
        link "gone" -> "/nowhere"
    content:
        use Crumb()
`
	_, err := File(writeProject(t, files, "playground.fct"))
	if err == nil {
		t.Fatal("a link to a route nothing serves must not compile")
	}
	for _, want := range []string{"no view serves that route", `component "Crumb"`, "`ui`/`data` facet"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// rendersLiteral reports whether any node in the tree renders the given literal
// text, so a test can ask what a composed screen actually shows.
func rendersLiteral(nodes []ir.Node, lit string) bool {
	for _, n := range nodes {
		for _, s := range n.Segs {
			if strings.Contains(s.Lit, lit) {
				return true
			}
		}
		if rendersLiteral(n.Children, lit) {
			return true
		}
	}
	return false
}
