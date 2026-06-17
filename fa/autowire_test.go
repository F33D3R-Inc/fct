package fa

import (
	"strings"
	"testing"
)

const awLikeSrc = `facet LikeButton:
    what:
        post: Post
        count: int
        liked: bool

    looks:
        <button class="like{ if liked } on{ end }" data-action="post.like">
            if count:
                <span>{count}</span>
        </button>

    when post.like:
        replace LikeButton
`

// A singleton container (Feed → no holes in its id) plus a parametric item.
const awFeedSrc = `facet Feed:
    looks:
        <ul data-action="feed.add">
            slot:
        </ul>

    when feed.add:
        append Feed with FeedItem

    when feed.drop:
        remove FeedItem

facet FeedItem:
    what:
        post: Post
    looks:
        <li>{post.id}</li>
`

func awLikeData(count int, liked bool) map[string]any {
	return map[string]any{
		"Post":  map[string]any{"ID": "p1"},
		"Count": count,
		"Liked": liked,
	}
}

func awItemData(id string) map[string]any {
	return map[string]any{"Post": map[string]any{"ID": id}}
}

func TestAutoWireReplaceDerivesIDFromRender(t *testing.T) {
	c, err := Compile(awLikeSrc)
	if err != nil {
		t.Fatal(err)
	}

	count, liked := 3, false
	app := New(c.Manifest)
	app.AutoWire(c, "post.like", func(ctx Ctx) (any, error) {
		liked = !liked
		if liked {
			count++
		} else {
			count--
		}
		return awLikeData(count, liked), nil
	})

	events, err := app.Dispatch("post.like", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Op != "replace" {
		t.Errorf("op = %q, want replace", e.Op)
	}
	// Derived, not hand-built: LikeButton's id is LikeButton:post:{post.id}.
	if e.FacetID != "LikeButton:post:p1" {
		t.Errorf("facet-id = %q, want LikeButton:post:p1", e.FacetID)
	}
	if !strings.Contains(e.Fragment, "<span>4</span>") || !strings.Contains(e.Fragment, "like on") {
		t.Errorf("fragment did not reflect toggled state: %s", e.Fragment)
	}

	events, _ = app.Dispatch("post.like", nil, "")
	if !strings.Contains(events[0].Fragment, "<span>3</span>") || strings.Contains(events[0].Fragment, "like on") {
		t.Errorf("toggle-back wrong: %s", events[0].Fragment)
	}
}

func TestAutoWireAppendRendersItemIntoContainer(t *testing.T) {
	c, err := Compile(awFeedSrc)
	if err != nil {
		t.Fatal(err)
	}
	app := New(c.Manifest)
	app.AutoWire(c, "feed.add", func(ctx Ctx) (any, error) {
		return awItemData("x9"), nil
	})

	events, err := app.Dispatch("feed.add", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.Op != "append" {
		t.Errorf("op = %q, want append", e.Op)
	}
	if e.FacetID != "Feed" { // the singleton container, addressed by name
		t.Errorf("facet-id = %q, want Feed", e.FacetID)
	}
	if !strings.Contains(e.Fragment, ">x9</li>") || !strings.Contains(e.Fragment, `data-facet-id="FeedItem:post:x9"`) {
		t.Errorf("fragment = %q, want the rendered FeedItem", e.Fragment)
	}
}

func TestAutoWireRemoveTargetsDerivedID(t *testing.T) {
	c, err := Compile(awFeedSrc)
	if err != nil {
		t.Fatal(err)
	}
	app := New(c.Manifest)
	app.AutoWire(c, "feed.drop", func(ctx Ctx) (any, error) {
		return awItemData("x9"), nil
	})

	events, err := app.Dispatch("feed.drop", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.Op != "remove" || e.FacetID != "FeedItem:post:x9" {
		t.Errorf("remove event = %+v, want remove FeedItem:post:x9", e)
	}
	if e.Fragment != "" {
		t.Errorf("remove must carry no fragment, got %q", e.Fragment)
	}
}

func TestAutoWireStartupRejections(t *testing.T) {
	cases := []struct {
		name string
		src  string
		ev   string
	}{
		{"no when block", awLikeSrc, "post.nonexistent"},
		{
			"replace_all unsupported by client",
			"facet X:\n    looks:\n        <div data-action=\"x\">x</div>\n    when x.go:\n        replace_all\n",
			"x.go",
		},
		{
			"append without with",
			"facet Y:\n    looks:\n        <ul data-action=\"y\">y</ul>\n    when y.go:\n        append Y\n",
			"y.go",
		},
		{
			"with on replace",
			"facet Z:\n    what:\n        post: Post\n    looks:\n        <div data-action=\"z\">{post.id}</div>\n    when z.go:\n        replace Z with Z\n",
			"z.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compile(tc.src)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected AutoWire to panic: %s", tc.name)
				}
			}()
			New(c.Manifest).AutoWire(c, tc.ev, func(Ctx) (any, error) { return nil, nil })
		})
	}
}
