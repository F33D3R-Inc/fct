package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// walkNodes visits every node of a view tree in pre-order.
func walkNodes(nodes []ir.Node, visit func(*ir.Node)) {
	for i := range nodes {
		visit(&nodes[i])
		walkNodes(nodes[i].Children, visit)
	}
}

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

// `video` carries a poster and playback flags. `autoplay` implies `muted` in the
// IR — no browser autoplays with sound, so the compiler folds it once rather than
// leaving two renderers to each remember.
func TestVideoPosterAndFlags(t *testing.T) {
	g := mustCompile(t, `app V:
    entity Clip:
        id: int
        src: text
        thumb: text
    view Home at "/":
        box:
            for c in Clip:
                video "{c.src}" poster "{c.thumb}" alt "clip {c.id}" autoplay loop
`)
	var v *ir.Node
	walkNodes(g.View, func(n *ir.Node) {
		if n.Kind == "video" {
			v = n
		}
	})
	if v == nil {
		t.Fatal("no video node")
	}
	if len(v.Poster) == 0 || v.Poster[0].Expr == nil {
		t.Errorf("poster should be an interpolated segment list, got %+v", v.Poster)
	}
	if !v.Autoplay || !v.Loop {
		t.Errorf("autoplay/loop flags lost: %+v", v)
	}
	if !v.Muted {
		t.Error("autoplay must imply muted — a player that never starts is what an unmuted autoplay is")
	}
}

func TestVideoFlagsAreValidated(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{`video "{c.src}" fullscreen`, "unknown flag"},
		{`video "{c.src}" alt "x" poster "{c.thumb}"`, "ordered"},
		{`video poster "{c.thumb}"`, "needs a source"},
	} {
		_, err := String(`app V:
    entity Clip:
        id: int
        src: text
        thumb: text
    view Home at "/":
        box:
            for c in Clip:
                ` + c.src + `
`)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.src, c.want, err)
		}
	}
}

// `for … limit shown more loadMore:` is the infinite-scroll list: the node names
// the action, the action is a validated zero-argument action, and the list still
// refreshes on the page-size cell (that is what grows when the action runs).
func TestInfiniteScrollMore(t *testing.T) {
	g := mustCompile(t, `app F:
    state shown: int = 20 @client
    entity Post:
        id: int
        body: text
    action loadMore:
        shown = shown + 20
    view Home at "/":
        box:
            for p in Post by id desc limit shown more loadMore:
                text "{p.body}"
`)
	var l *ir.Node
	walkNodes(g.View, func(n *ir.Node) {
		if n.Kind == "list" {
			l = n
		}
	})
	if l == nil {
		t.Fatal("no list node")
	}
	if l.More != "loadMore" {
		t.Errorf("list.More = %q, want loadMore", l.More)
	}
	if l.Limit == nil {
		t.Error("limit lost")
	}
	if !contains(g.DepGraph["shown"], l.ID) {
		t.Errorf("the list must refresh when the page size grows; deps of shown = %v", g.DepGraph["shown"])
	}
	for i := range g.Actions {
		if g.Actions[i].Name == "loadMore" && g.Actions[i].Placement != ir.Client {
			t.Errorf("loadMore writes only @client state and should run on the client, got %s", g.Actions[i].Placement)
		}
	}
}

func TestMoreIsValidated(t *testing.T) {
	base := func(forLine string) string {
		return `app F:
    state shown: int = 20 @client
    state pick: text = "" @client
    entity Post:
        id: int
        body: text
    action loadMore:
        shown = shown + 20
    action open(id: int):
        shown = id
    view Home at "/":
        box:
            ` + forLine + `
`
	}
	for _, c := range []struct{ src, want string }{
		{`for p in Post more loadMore:
                text "{p.body}"`, "needs a `limit`"},
		{`for p in Post limit shown more nope:
                text "{p.body}"`, "unknown action \"nope\""},
		{`for p in Post limit shown more open:
                text "{p.body}"`, "zero-argument"},
		{`for p in Post limit shown more 3:
                text "{p.body}"`, "names the action"},
		{`select bind pick:
                for p in Post limit shown more loadMore:
                    option "{p.body}" -> p.body`, "choice list"},
	} {
		_, err := String(base(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want error containing %q, got %v", c.src, c.want, err)
		}
	}
}
