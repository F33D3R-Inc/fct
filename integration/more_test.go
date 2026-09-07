package integration

import (
	"strings"
	"testing"
)

// The infinite-scroll list: `limit shown more loadMore`. Five rows, two per page.
const moreApp = `app Feed:
    entity Post:
        id: int
        body: text
    state shown: int = 2 @client
    action add(body: text):
        add Post { body: body }
    action loadMore:
        shown = shown + 2
    view Home at "/":
        box:
            for p in Post by id limit shown more loadMore:
                text "row:{p.body}"
`

func seedPosts(t *testing.T, a *app, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if code, body := a.action("add", "p"+string(rune('0'+i))); code != 200 {
			t.Fatalf("seeding post %d: %d %s", i, code, body)
		}
	}
}

func hasAttr(attrs []clientAttr, name, value string) bool {
	for _, at := range attrs {
		if at.Name == name && at.Value == value {
			return true
		}
	}
	return false
}

// The authority paints the "More" control exactly while its `limit` held rows
// back; the client paints the same control from the flag the bootstrap carried;
// and acting on it loads the next page from the authority. The control is a real
// button, so this drives it by click — the path a reader with no
// IntersectionObserver (and this shim) takes; the observer only automates it.
func TestInfiniteScrollLoadsTheNextPage(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, moreApp)
	seedPosts(t, a, 5)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	if !strings.Contains(markup, `data-fa-more="loadMore"`) {
		t.Fatalf("the authority cut the list at 2 of 5 rows but painted no More control:\n%s", markup)
	}
	if got := serverText(page); !strings.Contains(got, "row:p2") || strings.Contains(got, "row:p3") {
		t.Fatalf("first paint should hold rows 1-2 and no more, got %q", got)
	}

	before, after := runClientAgainst(t, a, page, []driveStep{{Sel: "[data-fa-more]", Do: "click"}})
	if !hasAttr(before.Attrs, "data-fa-more", "loadMore") {
		t.Errorf("the client's first render lost the More control: %v", before.Attrs)
	}
	if strings.Contains(before.Text, "row:p3") {
		t.Errorf("the client rendered past the limit before anything was loaded: %q", before.Text)
	}
	for _, want := range []string{"row:p1", "row:p2", "row:p3", "row:p4"} {
		if !strings.Contains(after.Text, want) {
			t.Errorf("after one More, %q is missing — the next page did not load. Got: %q", want, after.Text)
		}
	}
	if strings.Contains(after.Text, "row:p5") {
		t.Errorf("one More loads one page (2 rows), not everything: %q", after.Text)
	}
	if !hasAttr(after.Attrs, "data-fa-more", "loadMore") {
		t.Errorf("4 of 5 rows shown, so the More control must still be there: %v", after.Attrs)
	}
}

// When the last page lands the control goes away: the authority answers the
// region with `more: false`, and the client stops painting it — which is also
// what stops an observer-driven page from asking forever.
func TestInfiniteScrollStopsWhenExhausted(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, moreApp)
	seedPosts(t, a, 5)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	_, after := runClientAgainst(t, a, page, []driveStep{
		{Sel: "[data-fa-more]", Do: "click"},
		{Sel: "[data-fa-more]", Do: "click"},
	})
	for _, want := range []string{"row:p1", "row:p5"} {
		if !strings.Contains(after.Text, want) {
			t.Errorf("after two Mores every row should be on the page; %q is missing from %q", want, after.Text)
		}
	}
	if hasAttr(after.Attrs, "data-fa-more", "loadMore") {
		t.Errorf("every row is shown, yet the More control is still painted — an observer would load forever: %v", after.Attrs)
	}
}

// A list the authority renders whole paints no control at all.
func TestNoMoreControlWhenNothingIsHeldBack(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, moreApp)
	seedPosts(t, a, 2)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	if strings.Contains(mountedMarkup(page), "data-fa-more") {
		t.Fatalf("2 rows, limit 2: nothing held back, yet a More control was painted:\n%s", mountedMarkup(page))
	}
	run, _ := runClient(t, page, nil)
	if hasAttr(run.Attrs, "data-fa-more", "loadMore") {
		t.Errorf("the client painted a More control the authority did not: %v", run.Attrs)
	}
}

// video: poster and playback flags reach both renderers identically; autoplay
// implies muted (and playsinline) on both sides.
func TestVideoPosterAndFlagsOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, `app Clips:
    entity Clip:
        id: int
        src: text
        thumb: text
    action add(src: text, thumb: text):
        add Clip { src: src, thumb: thumb }
    view Home at "/":
        box:
            for c in Clip:
                video "{c.src}" poster "{c.thumb}" alt "clip {c.id}" autoplay loop
`)
	if code, body := a.action("add", "/m/a.mp4", "/m/a.jpg"); code != 200 {
		t.Fatalf("seeding a clip: %d %s", code, body)
	}
	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	for _, want := range []string{`poster="/m/a.jpg"`, ` autoplay`, ` playsinline`, ` loop`, ` muted`, `aria-label="clip 1"`} {
		if !strings.Contains(markup, want) {
			t.Errorf("server <video> is missing %s:\n%s", want, markup)
		}
	}
	run, _ := runClient(t, page, nil)
	for _, want := range [][2]string{{"poster", "/m/a.jpg"}, {"autoplay", ""}, {"playsinline", ""}, {"loop", ""}, {"muted", ""}, {"aria-label", "clip 1"}} {
		if !hasAttr(run.Attrs, want[0], want[1]) {
			t.Errorf("client <video> is missing %s=%q: %v", want[0], want[1], run.Attrs)
		}
	}
}
