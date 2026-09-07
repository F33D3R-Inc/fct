package integration

import (
	"os"
	"strings"
	"testing"
)

// startFacetsApp starts one of the reference apps in the sibling `facets` repo
// (github.com/F33D3R-Inc/facets), where the library lives; the test is skipped
// when that checkout is not beside this one.
func startFacetsApp(t *testing.T, e *engine, name string) *app {
	t.Helper()
	path := "../../facets/" + name
	if _, err := os.Stat(path); err != nil {
		t.Skipf("facets/%s not present", name)
	}
	return startAppFile(t, e, path)
}

// The library's timeline app renders the f33d3r look on both sides: the shell,
// the rail glyphs, a post card with its engagement bar, and the profile header.
// This is the one test that compiles the whole social slice of the facets tree —
// every atom it imports, the look, the icons — and runs the shipped client over
// the result, so an atom that stops composing, or a class the client stops
// stamping, fails here rather than in a browser.
func TestLibraryHomeRendersOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startFacetsApp(t, e, "timeline.fct")

	if code, body := a.action("signup", "ada", "pw12345678"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if code, body := a.action("post", "hello from the library"); code != 200 {
		t.Fatalf("post: %d %s", code, body)
	}
	if code, body := a.action("postVideo", "a clip", "/m/clip.mp4"); code != 200 {
		t.Fatalf("postVideo: %d %s", code, body)
	}
	if code, body := a.action("quote", "look at #this", 1); code != 200 {
		t.Fatalf("quote: %d %s", code, body)
	}

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	for _, want := range []string{
		`class="fa-row x-l-app3"`, `class="fa-box x-l-app3-nav"`, `class="fa-box x-l-app3-main"`, `class="fa-box x-l-app3-aside"`,
		`data-fa-icon="home"`, `class="fa-box x-post"`, `class="fa-row x-actions"`, `class="x-like x-glyph-heart"`,
		`class="fa-box x-media x-aspect x-aspect-16x9"`, `class="fa-row x-pinned"`, `class="fa-link x-pill x-primary x-post-btn"`,
		`class="fa-box x-quote"`, "· now",
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("the authority's home page is missing %s", want)
		}
	}
	// The look and the icons travel as imported css: blocks into one stylesheet.
	if code, css := a.get("/facet.css"); code != 200 || !strings.Contains(css, ".x-l-app3") || !strings.Contains(css, `[data-fa-icon="heart"]`) {
		t.Errorf("the stylesheet lost the look or the glyphs (status %d)", code)
	}

	run, _ := runClientAgainst(t, a, page, nil)
	for _, want := range [][2]string{
		{"class", "fa-row x-l-app3"}, {"class", "fa-box x-post"}, {"class", "fa-row x-actions"},
		{"class", "x-like x-glyph-heart"}, {"data-fa-icon", "home"}, {"class", "fa-box x-media x-aspect x-aspect-16x9"},
		{"class", "fa-box x-quote"},
	} {
		if !hasAttr(run.Attrs, want[0], want[1]) {
			t.Errorf("the client's render lost %s=%q", want[0], want[1])
		}
	}
	// The shim reports text with its whitespace removed, and a richtext body as
	// the HTML it was given — which is how the autolink is visible here.
	for _, want := range []string{"hellofromthelibrary", "aclip", "Pinned", "Home", "Explore", `<ahref="/tag/this">#this</a>`, "·now"} {
		if !strings.Contains(run.Text, want) {
			t.Errorf("the client's render is missing %q: %q", want, run.Text)
		}
	}

	code, profile := a.get("/profile/ada")
	if code != 200 {
		t.Fatalf("GET /profile/ada: %d", code)
	}
	for _, want := range []string{`class="fa-image x-profile-banner"`, `class="fa-row x-profile-top"`, `class="fa-row x-profile-stats"`, `class="fa-row x-topbar"`, "3 posts"} {
		if !strings.Contains(mountedMarkup(profile), want) {
			t.Errorf("the profile page is missing %s", want)
		}
	}

	// The hashtag page: the quote's `#this` is a link, and the page it links to
	// lists the post.
	code, tag := a.get("/tag/this")
	if code != 200 {
		t.Fatalf("GET /tag/this: %d", code)
	}
	if !strings.Contains(serverText(tag), "lookat#this") || !strings.Contains(mountedMarkup(tag), "1 posts") {
		t.Errorf("the tag page should list the one tagged post: %q", serverText(tag))
	}
}
