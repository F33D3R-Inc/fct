package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

const entityParamApp = `app E:
    enum Kind: text, video
    entity User:
        id: int
        name: text
    entity Post:
        id: int
        body: text
        kind: Kind
        by: User
        created: int
    component Card(t: Post):
        box:
            text "{t.body} · {ago(t.created)}"
            match t.kind:
                case "text":
                    text "t"
                case "video":
                    text "v"
            use Inner(t)
    component Inner(p: Post):
        text "{compact(len(p.body))}"
    view Home at "/":
        box:
            for p in Post:
                use Card(p)
`

// A component may take a row: `component Card(t: Post)`, called with the `for`
// variable. Inside, `t.field` is typed as a row of Post — so `match t.kind` finds
// the enum and needs no else — and the row passes through to a nested component.
func TestComponentTakesAnEntityRow(t *testing.T) {
	g := mustCompile(t, entityParamApp)
	var card *ir.Component
	for i := range g.Components {
		if g.Components[i].Name == "Card" {
			card = &g.Components[i]
		}
	}
	if card == nil {
		t.Fatal("Card did not compile")
	}
	if len(card.Params) != 1 || card.Params[0].Type != "Post" {
		t.Errorf("Card's parameter should be typed Post, got %+v", card.Params)
	}
}

func TestEntityParameterMustReceiveARow(t *testing.T) {
	base := func(useLine string) string {
		return strings.Replace(entityParamApp, "                use Card(p)", "                use Card(p)\n                "+useLine, 1)
	}
	for _, c := range []struct{ use, want string }{
		// The three ways to hand an id where a row is due, each named in the message.
		{"use Card(p.id)", "is a Post row, but argument 1 (`p.id`, an int) is not one"},
		{"use Card(5)", "argument 1 (the literal 5) is not one"},
		{"use Card(p.by)", "is a Post row, but argument 1"}, // a relation field is an id, and of the wrong entity besides
		{`use Card("x")`, "parameter \"t\" is Post, but argument 1 is text"},
	} {
		_, err := String(base(c.use))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.use, c.want, err)
		}
	}
}

// A relation field is a row of ANOTHER entity's id — still not a row.
func TestRelationFieldIsNotARow(t *testing.T) {
	src := strings.Replace(entityParamApp, "    component Inner(p: Post):", "    component Who(u: User):\n        text \"{u.name}\"\n    component Inner(p: Post):", 1)
	src = strings.Replace(src, "                use Card(p)", "                use Card(p)\n                use Who(p.by)", 1)
	_, err := String(src)
	if err == nil || !strings.Contains(err.Error(), "is a User row, but argument 1 (`p.by`, a User) is not one") {
		t.Errorf("want the row diagnostic for a relation field, got %v", err)
	}
}

// `x.field` on a row is checked against the entity — in a component body, a
// `for` body, a `where` filter, and a filtered aggregate's predicate alike.
func TestUnknownFieldOnARowIsAnError(t *testing.T) {
	for _, c := range []struct{ name, old, new, want string }{
		{"component body", `text "{t.body} · {ago(t.created)}"`, `text "{t.bdoy}"`, `entity "Post" has no field "bdoy" (in ` + "`t.bdoy`" + `)`},
		{"for body", "                use Card(p)", "                text \"{p.nope}\"", `has no field "nope" (in ` + "`p.nope`" + `)`},
		{"where", "for p in Post:", "for p in Post where p.autor == actor:", `has no field "autor"`},
		{"filtered aggregate", "                use Card(p)", "                text \"{count(x in Post where x.bod == p.body)}\"", `has no field "bod"`},
		{"if condition", "                use Card(p)", "                if p.pinned:\n                    text \"pinned\"", `has no field "pinned"`},
	} {
		src := strings.Replace(entityParamApp, c.old, c.new, 1)
		_, err := String(src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	// `id` is implicit on every entity and never an error.
	if _, err := String(strings.Replace(entityParamApp, "                use Card(p)", "                text \"{p.id}\"", 1)); err != nil {
		t.Errorf("p.id should be accepted: %v", err)
	}
}

func TestComponentParameterTypeMustExist(t *testing.T) {
	_, err := String(strings.Replace(entityParamApp, "component Card(t: Post):", "component Card(t: Psot):", 1))
	if err == nil || !strings.Contains(err.Error(), `parameter "t" has unknown type "Psot"`) {
		t.Errorf("want the unknown-type diagnostic, got %v", err)
	}
}

// The formatting builtins type as text and are pure, so a view may use them.
func TestFormattingBuiltinsAreViewSafe(t *testing.T) {
	mustCompile(t, `app F:
    entity Post:
        id: int
        body: text
        created: int
        likes: int
    view Home at "/":
        box:
            for p in Post:
                text "{ago(p.created)} · {compact(p.likes)} · {commas(p.likes)} · {take(p.body, 80)}"
`)
	for _, c := range []struct{ src, want string }{
		{`text "{compact()}"`, "compact"},
		{`text "{take(p.body)}"`, "take"},
	} {
		_, err := String(`app F:
    entity Post:
        id: int
        body: text
    view Home at "/":
        box:
            for p in Post:
                ` + c.src + `
`)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an arity error naming %s, got %v", c.src, c.want, err)
		}
	}
}

// `Post(id)` with no field is the row: it may be handed to an entity parameter
// (`use QuoteCard(Tweet(t.quoted))`), it may not be rendered as text, and a
// lookup that does name a field must name one the entity has.
func TestBareLookupIsARow(t *testing.T) {
	mustCompile(t, strings.Replace(entityParamApp, "                use Card(p)", "                use Card(Post(p.id))", 1))
	for _, c := range []struct{ name, old, new, want string }{
		{"a row is not text", "                use Card(p)", "                text \"{Post(p.id)}\"", "`Post(…)` is a whole row and cannot be rendered as text"},
		{"a row variable is not text either", "                use Card(p)", "                text \"{p}\"", "`p` is a whole row and cannot be rendered as text"},
		{"lookup field must exist", "                use Card(p)", "                text \"{Post(p.id).bdoy}\"", `entity "Post" has no field "bdoy" (in ` + "`Post(…).bdoy`" + `)`},
		{"lookup of another entity is not this row", "                use Card(p)", "                use Card(User(p.by))", "is a Post row, but argument 1"},
	} {
		_, err := String(strings.Replace(entityParamApp, c.old, c.new, 1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	// `.id` on a lookup is always fine; a lookup inside a component slot too.
	mustCompile(t, strings.Replace(entityParamApp, "                use Card(p)", "                text \"{Post(p.id).id}\"", 1))
}

// A sealed field reaches a component through a row parameter: `body: text @e2e`
// rendered by `text "{m.body}"` inside `component Bubble(m: Message)` is the same
// standalone read the dataflow check allows on a `for` variable.
func TestSealedFieldThroughRowParameter(t *testing.T) {
	g := mustCompile(t, `app S:
    entity Message:
        id: int
        body: text @e2e
    component Bubble(m: Message):
        text "{m.body}"
    view Home at "/":
        box:
            for m in Message:
                use Bubble(m)
`)
	var sealed bool
	for _, c := range g.Components {
		walkNodes(c.View, func(n *ir.Node) {
			for _, s := range n.Segs {
				if s.E2E {
					sealed = true
				}
			}
		})
	}
	if !sealed {
		t.Error("the bubble's body should be lowered as a sealed read")
	}
	_, err := String(`app S:
    entity Message:
        id: int
        body: text @e2e
    component Bubble(m: Message):
        text "{m.body + "!"}"
    view Home at "/":
        box:
            for m in Message:
                use Bubble(m)
`)
	// A literal beside the sealed read is fine (the placeholder is its own span);
	// an expression over it is not — the whole ciphertext opens at once.
	if err == nil || !strings.Contains(err.Error(), "stand alone") {
		t.Errorf("a sealed read must stand alone even through a row parameter, got %v", err)
	}
}
