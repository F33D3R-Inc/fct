package compile

import "testing"

// richtext (markdown posts) and video are the post-content primitives f33d3r.com
// renders everywhere. They compile to their own node kinds, carrying interpolated
// segments like image/text.
func TestContentPrimitives(t *testing.T) {
	g := mustCompile(t, `app C:
    entity Post:
        id: int
        body: text
        media: text
    view Home at "/":
        box:
            for p in Post:
                box:
                    richtext "{p.body}"
                    video "{p.media}"
`)
	k := kindCounts(g.View)
	for _, want := range []string{"richtext", "video"} {
		if k[want] == 0 {
			t.Errorf("view should contain a %q node, kinds=%v", want, k)
		}
	}
}

// Search (contains in a where) + a dynamic limit (a @client page size for
// load-more / infinite scroll). The list refreshes when either the query or the
// page size changes.
func TestSearchAndDynamicLimit(t *testing.T) {
	g := mustCompile(t, `app S:
    state q: text = "" @client
    state shown: int = 20 @client
    entity Post:
        id: int
        body: text
    view Home at "/":
        box:
            input bind q placeholder "search"
            for p in Post where contains(lower(p.body), lower(q)) limit shown:
                text "{p.body}"
`)
	if len(g.DepGraph["q"]) == 0 {
		t.Errorf("list should refresh on search query q, deps=%v", g.DepGraph)
	}
	if len(g.DepGraph["shown"]) == 0 {
		t.Errorf("list should refresh on dynamic limit shown, deps=%v", g.DepGraph)
	}
}
