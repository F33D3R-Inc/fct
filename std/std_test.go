package std_test

import (
	"strings"
	"testing"

	"fct.dev/fa"
	"fct.dev/std"
)

// TestStdlibCompiles is the load-bearing test: every standard-library facet must
// parse, codegen, and parse as a Go template. If this passes, the whole catalog
// is valid FDL.
func TestStdlibCompiles(t *testing.T) {
	c, err := fa.Compile(std.Source())
	if err != nil {
		t.Fatalf("the standard library does not compile: %v", err)
	}

	names := std.Names()
	if len(names) < 20 {
		t.Errorf("expected a real catalog, got %d facets: %v", len(names), names)
	}
	t.Logf("standard library: %d facets — %s", len(names), strings.Join(names, ", "))

	// Spot-render a representative sample.
	cases := []struct {
		facet string
		data  map[string]any
		want  string
	}{
		{"Button", map[string]any{"Label": "Go", "Variant": "primary", "Action": "x"}, "fa-btn--primary"},
		{"Badge", map[string]any{"Label": "new", "Tone": "accent"}, "fa-badge--accent"},
		{"Avatar", map[string]any{"Src": "/a.png", "Alt": "me", "Size": "small"}, `src="/a.png"`},
		{"Toggle", map[string]any{"On": true, "Label": "Dark", "Action": "t"}, "fa-toggle--on"},
		{"ProgressBar", map[string]any{"Value": 60, "Label": "upload"}, "width:60%"},
		{"Card", map[string]any{"Title": "Hi"}, "fa-card__title"},
		{"Checkbox", map[string]any{"Name": "agree", "Label": "OK", "Checked": true}, "checked"},
		{"Spinner", map[string]any{}, "fa-spinner"},
	}
	for _, tc := range cases {
		h, err := c.Render(tc.facet, tc.data)
		if err != nil {
			t.Errorf("render %s: %v", tc.facet, err)
			continue
		}
		if !strings.Contains(string(h), tc.want) {
			t.Errorf("%s render missing %q:\n%s", tc.facet, tc.want, h)
		}
	}
}

// TestStdlibComposes: an app facet uses stdlib facets (composition + slot) and
// it all compiles and renders together.
func TestStdlibComposes(t *testing.T) {
	app := `
facet Demo:
    what:
        likes: int
    looks:
        <div>
            <Card title="Stats">
                <Badge label="hot" tone="danger" />
                <Count value="{likes}" label="likes" />
            </Card>
        </div>
`
	c, err := fa.Compile(std.Source() + app)
	if err != nil {
		t.Fatalf("compose with stdlib failed: %v", err)
	}
	h, err := c.Render("Demo", map[string]any{"Likes": 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fa-card", "fa-badge--danger", `aria-label="likes"`, ">42<"} {
		if !strings.Contains(string(h), want) {
			t.Errorf("composed render missing %q:\n%s", want, h)
		}
	}
}

// TestFeedComposition renders a PostCard — which composes PostHeader (→ Avatar,
// Badge), PostBody, Image, and PostActionBar — with real nested data, proving the
// feed catalog works end to end and gets a per-instance facet-id.
func TestFeedComposition(t *testing.T) {
	c, err := fa.Compile(std.Source())
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.Render("PostCard", map[string]any{
		"Post": map[string]any{
			"ID": "42", "Body": "hello world", "Time": "2h",
			"ReplyCount": 3, "RepostCount": 1, "LikeCount": 10,
			"MediaURL": "/m.png", "MediaAlt": "pic",
		},
		"Author":   map[string]any{"AvatarURL": "/a.png", "Handle": "ada", "DisplayName": "Ada", "Verified": true},
		"Liked":    true,
		"Reposted": false,
	})
	if err != nil {
		t.Fatalf("render PostCard: %v", err)
	}
	got := string(h)
	for _, want := range []string{
		`data-facet-id="PostCard:post:42"`, // per-instance id (surgical updates)
		"Ada", "@ada",                      // PostHeader → name/handle
		"fa-badge--accent",       // verified → Badge
		`src="/a.png"`,           // PostHeader → Avatar
		"hello world",            // PostBody
		`src="/m.png"`,           // media → Image
		"fa-post__action--liked", // liked state
		">10<",                   // like count
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PostCard missing %q\n---\n%s", want, got)
		}
	}
}

func TestStdlibCSS(t *testing.T) {
	if !strings.Contains(std.CSS, ".fa-btn") || !strings.Contains(std.CSS, ".fa-card") {
		t.Error("std.CSS missing expected classes")
	}
}
