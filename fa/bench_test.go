package fa

import (
	"fmt"
	"testing"
)

const benchFacet = `
facet PostCard:
    what:
        post: Post
        viewer: Viewer
    looks:
        <article class="post" data-facet-id="x">
            <h3>{post.title}</h3>
            if post.likes > 100:
                <span class="hot">hot</span>
            <p>{post.body}</p>
            <button data-action="post.like" data-post-id="{post.id}">{post.likes} likes</button>
        </article>
`

func benchData() map[string]any {
	return map[string]any{
		"Post":   map[string]any{"ID": "p1", "Title": "Hello", "Body": "world", "Likes": 150},
		"Viewer": map[string]any{"ID": "v1"},
	}
}

// BenchmarkRender measures server-side render of one facet (the per-update cost).
func BenchmarkRender(b *testing.B) {
	c, err := Compile(benchFacet)
	if err != nil {
		b.Fatal(err)
	}
	data := benchData()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Render("PostCard", data); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDispatch measures the handler path (guard + handler + render).
func BenchmarkDispatch(b *testing.B) {
	c, _ := Compile(benchFacet)
	app := New([]byte(`{}`))
	app.On("post.like", func(ctx Ctx) ([]Event, error) {
		frag, _ := c.Render("PostCard", benchData())
		return []Event{{Op: "replace", FacetID: "PostCard:post:p1", Fragment: string(frag)}}, nil
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := app.Dispatch("post.like", map[string]string{"postId": "p1"}, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEmitChannel measures signed fan-out to N local subscribers.
func BenchmarkEmitChannel(b *testing.B) {
	h := newHub([]byte("bench-key-bench-key-bench-key-bk"), nil, nil, nil)
	const subs = 1000
	for i := 0; i < subs; i++ {
		c := &sseClient{id: newConnID(), channels: make(map[string]bool), send: make(chan []byte, 1<<16)}
		h.register(c)
		h.subscribe(c.id, "feed")
	}
	ev := Event{Op: "replace", FacetID: "X", Fragment: "<b>hi</b>"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.EmitChannel("feed", ev)
	}
	b.StopTimer()
	b.ReportMetric(float64(subs), "subs/op")
	_ = fmt.Sprint // keep fmt imported for readability of failures
}

// BenchmarkCompile measures cold compile of a facet set.
func BenchmarkCompile(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Compile(benchFacet); err != nil {
			b.Fatal(err)
		}
	}
}
