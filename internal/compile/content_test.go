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
