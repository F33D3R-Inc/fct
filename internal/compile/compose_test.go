package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err == nil || !strings.Contains(err.Error(), "no such socket") {
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
