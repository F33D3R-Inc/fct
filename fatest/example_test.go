package fatest_test

import (
	"strings"
	"testing"

	"fct.dev/fa"
	"fct.dev/fatest"
)

const src = `
facet LikeButton:
    what:
        post: Post
        count: int
        liked: bool
    looks:
        <button class="like{ if liked } active{ end }" data-action="post.like" data-post-id="{post.id}">{count}</button>
    when post.like_toggled:
        replace LikeButton with event.payload
`

// Testing what a facet renders — no server, no browser.
func TestFacetRender(t *testing.T) {
	html := fatest.Render(t, src, "LikeButton", map[string]any{
		"Post": map[string]any{"ID": "1"}, "Count": 5, "Liked": true,
	})
	if !strings.Contains(html, "active") || !strings.Contains(html, ">5<") {
		t.Errorf("unexpected render: %s", html)
	}
}

// Testing an event handler — dispatch an action, assert on what it pushes.
func TestLikeHandler(t *testing.T) {
	c, err := fa.Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	app := fa.New(c.Manifest)
	post := struct {
		ID    string
		Count int
		Liked bool
	}{ID: "1", Count: 5}

	app.On("post.like", func(ctx fa.Ctx) ([]fa.Event, error) {
		post.Liked = !post.Liked
		if post.Liked {
			post.Count++
		} else {
			post.Count--
		}
		frag, _ := c.Render("LikeButton", map[string]any{
			"Post": map[string]any{"ID": post.ID}, "Count": post.Count, "Liked": post.Liked,
		})
		return []fa.Event{{Op: "replace", FacetID: "LikeButton:post:" + post.ID, Fragment: string(frag)}}, nil
	})

	events := fatest.Dispatch(t, app, "post.like", map[string]string{"postId": "1"})
	fatest.AssertFragment(t, events, "post:1", "active", ">6<")
}
