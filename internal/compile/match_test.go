package compile

import (
	"strings"
	"testing"
)

// `match` over an enum-typed entity field, exhaustively covered — the post-kind
// render switch. It lowers to a match region with one case per member.
func TestMatchEnumExhaustive(t *testing.T) {
	g := mustCompile(t, `app M:
    enum Kind: text, image, video
    entity Post:
        id: int
        kind: Kind
        body: text
        media: text
    view Home at "/":
        box:
            for p in Post:
                match p.kind:
                    case "text":
                        text "{p.body}"
                    case "image":
                        image "{p.media}"
                    case "video":
                        video "{p.media}"
`)
	k := kindCounts(g.View)
	if k["match"] == 0 {
		t.Errorf("expected a match node, kinds=%v", k)
	}
	if k["case"] != 3 {
		t.Errorf("expected 3 case nodes, got %d", k["case"])
	}
}

// An `else` makes a match exhaustive regardless of the subject's type, and the
// matched value's deps drive the region's refresh.
func TestMatchWithElse(t *testing.T) {
	g := mustCompile(t, `app M:
    state mode: text = "a" @client
    view Home at "/":
        box:
            match mode:
                case "a":
                    text "alpha"
                else:
                    text "other"
`)
	if kindCounts(g.View)["match"] == 0 {
		t.Error("expected a match node")
	}
	if len(g.DepGraph["mode"]) == 0 {
		t.Errorf("match region should refresh when its subject changes, deps=%v", g.DepGraph)
	}
}

func TestMatchErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			"non-exhaustive enum",
			`app M:
    enum Kind: text, image, video
    entity Post:
        id: int
        kind: Kind
    view H at "/":
        for p in Post:
            match p.kind:
                case "text":
                    text "t"
                case "image":
                    text "i"
`,
			"not exhaustive",
		},
		{
			"unknown enum member",
			`app M:
    enum Kind: text, image
    entity Post:
        id: int
        kind: Kind
    view H at "/":
        for p in Post:
            match p.kind:
                case "text":
                    text "t"
                case "audio":
                    text "a"
`,
			`has no member "audio"`,
		},
		{
			"open type needs else",
			`app M:
    state name: text = "" @client
    view H at "/":
        match name:
            case "ada":
                text "hi ada"
`,
			"must be exhaustive",
		},
		{
			"duplicate case",
			`app M:
    state name: text = "" @client
    view H at "/":
        match name:
            case "a":
                text "1"
            case "a":
                text "2"
            else:
                text "x"
`,
			"duplicate match case",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := String(c.src); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
