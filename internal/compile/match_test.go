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

// A `case` may be written as a number.
//
// It could not — `case 1:` was `expected a quoted string, got "1"` — so a match on
// a month or a count had to coerce its subject (`match "" + month(at):`) to reach
// cases that were already being compared as text. Both renderers stringify the
// subject and compare it to the case's value, so `case 1:` and `case "1":` are the
// same case, and the number carries no new claim about coverage: an int has no
// finite member list, so an int-subject match is open-typed and must still have an
// `else`, exactly as before.
func TestMatchNumericCase(t *testing.T) {
	g := mustCompile(t, `app M:
    state n: int = 1 @client
    view Home at "/":
        match n:
            case 1:
                text "one"
            case 2:
                text "two"
            else:
                text "many"
`)
	m := g.Pages[0].View[0]
	if m.Kind != "match" || len(m.Children) != 3 {
		t.Fatalf("match = %+v", m)
	}
	if m.Children[0].Value != "1" || m.Children[1].Value != "2" {
		t.Errorf("case values = %q, %q — want the decimal text the subject stringifies to",
			m.Children[0].Value, m.Children[1].Value)
	}
	// An int cannot be covered by cases, so the `else` is still required...
	if _, err := String(`app M:
    state n: int = 1 @client
    view H at "/":
        match n:
            case 1:
                text "one"
`); err == nil || !strings.Contains(err.Error(), "must be exhaustive") {
		t.Errorf("numeric match without else: got %v, want the exhaustiveness refusal", err)
	}
	// ...and an enum still proves coverage over its own members, numbers included.
	if _, err := String(`app M:
    enum Kind: text, image
    entity Post:
        id: int
        kind: Kind
    view H at "/":
        for p in Post:
            match p.kind:
                case "text":
                    text "t"
                case 1:
                    text "one"
`); err == nil || !strings.Contains(err.Error(), `has no member "1"`) {
		t.Errorf("numeric case on an enum: got %v, want the member refusal", err)
	}
}
