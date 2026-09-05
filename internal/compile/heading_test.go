package compile

import "strings"

import "testing"

// A heading is a text leaf that carries a document level. The level is an
// expression — a component's header must render at the depth its CALLER chose,
// since the same component sits at different depths on different pages — so
// these cases are about what the compiler can prove about that expression, not
// about what it evaluates to.

func headingApp(body string) string {
	return "app H:\n    view Home at \"/\":\n        box:\n" + body + "\n"
}

// The level reaches the IR as an expression, and the node is its own kind: a
// heading is not a styled span.
func TestHeadingLowersToItsOwnKindWithALevel(t *testing.T) {
	g, err := String(headingApp(`            heading 2 "Title"`))
	if err != nil {
		t.Fatal(err)
	}
	n := g.Pages[0].View[0].Children[0]
	if n.Kind != "heading" {
		t.Fatalf("node kind = %q, want \"heading\"", n.Kind)
	}
	if n.Level == nil {
		t.Fatal("heading has no level expression")
	}
	if n.Level.Kind != "lit" {
		t.Fatalf("level expr kind = %q, want a literal", n.Level.Kind)
	}
}

// A component takes the level as a parameter, which is the whole reason the
// level is an expression rather than a keyword.
func TestAComponentRendersAtTheLevelItsCallerChose(t *testing.T) {
	src := `app H:
    component SectionHeader(title: text, level: int):
        heading level "{title}"
    view Home at "/":
        box:
            use SectionHeader("Replies", 3)
`
	if _, err := String(src); err != nil {
		t.Fatalf("a caller-chosen level must compile: %v", err)
	}
}

// What the compiler CAN prove. Each of these is a level that is certainly wrong,
// and the certainty is what makes it an error rather than a guess.
func TestTheCompilerRefusesTheLevelsItCanProveWrong(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"zero", `            heading 0 "T"`, "1 and 6"},
		{"seven", `            heading 7 "T"`, "1 and 6"},
		{"negative", `            heading -1 "T"`, "1 and 6"},
		{"text literal", `            heading "2" "T"`, "needs a level"},
		{"no level", `            heading "T"`, "needs a level"},
		{"no text", `            heading 2`, "needs text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(headingApp(c.body))
			if err == nil {
				t.Fatalf("%s compiled; want an error mentioning %q", c.body, c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// A text-typed level is refused where the type is known, because toInt("two")
// is 0 with no diagnostic anywhere — the same asymmetry internal/ir/types.go
// states for every other declared position.
func TestATextLevelIsRefused(t *testing.T) {
	src := `app H:
    component Bad(title: text, level: text):
        heading level "{title}"
    view Home at "/":
        box:
            use Bad("x", "y")
`
	err := errOf(String(src))
	if err == nil || !strings.Contains(err.Error(), "level") {
		t.Fatalf("a text level must be refused, got %v", err)
	}
}

// A level that reads state directly is refused, and the message says why: a
// heading is a leaf with no region of its own, so a level that could change
// under the page has nothing to re-render it. Passing it through a component
// parameter (or a row) puts it inside a region that does.
func TestALevelMayNotReadStateDirectly(t *testing.T) {
	src := `app H:
    state depth: int = 2 @client
    view Home at "/":
        box:
            heading depth "T"
`
	err := errOf(String(src))
	if err == nil || !strings.Contains(err.Error(), "re-render") {
		t.Fatalf("a state-reading level must be refused with the re-render reason, got %v", err)
	}
}

func errOf[T any](_ T, err error) error { return err }
