package compile

import (
	"encoding/json"
	"strings"
	"testing"
)

// A `{expr}` in a position that does not interpolate is a compile error.
//
// It used to be silence, and the silence was expensive: `use Link("{p.name}",
// "/p/{p.id}")` shipped the literal braces to a rendered shop page from 27 call
// sites, and `style "width: {n}%"` is why a progress bar carried 51 hardcoded
// width classes. Both compiled clean. One case per refusing position, so a
// position that goes quiet again fails here.
func TestLiteralBracesAreRefused(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a component argument",
		src: `app A:
    entity Post:
        name: text
    component Lbl(label: text):
        text "{label}"
    view V at "/":
        for p in Post:
            use Lbl("{p.name}")
`,
		want: `argument 1 (label) of component "Lbl" does not interpolate`,
	}, {
		name: "the style modifier",
		src: `app A:
    state n: int = 0 @client
    view V at "/":
        box style "width: {n}%":
            text "hi"
`,
		want: "`style` does not interpolate",
	}, {
		name: "an option value",
		src: `app A:
    state pick: text = "" @client
    view V at "/":
        select bind pick:
            option "One" -> "{pick}"
`,
		want: "an option value does not interpolate",
	}, {
		name: "a tab value",
		src: `app A:
    state tab: text = "a" @client
    view V at "/":
        tabs bind tab:
            tab "A" -> "{tab}":
                text "a"
`,
		want: "a tab value does not interpolate",
	}, {
		name: "a case value",
		src: `app A:
    state mode: text = "a" @client
    view V at "/":
        match mode:
            case "{mode}":
                text "a"
`,
		want: "a `case` value does not interpolate",
	}, {
		name: "a check message",
		src: `app A:
    state count: int = 0
    action bump(n: int):
        check n > 0 "n was {n}"
        count = count + n
`,
		want: "a check message does not interpolate",
	}, {
		name: "a route path",
		src: `app A:
    entity Post:
        name: text
    view V at "/post/{Post}":
        text "hi"
`,
		want: `view "V"'s route does not interpolate`,
	}, {
		name: "a theme token",
		src: `app A:
    state accent: text = "#f00"
    theme:
        bg "{accent}"
    view V at "/":
        text "hi"
`,
		want: `theme token "bg" does not interpolate`,
	}, {
		name: "a service base URL",
		src: `app A:
    state host: text = "x"
    service S at "https://{host}":
        score(body: text)
    view V at "/":
        text "hi"
`,
		want: `service "S"'s base URL does not interpolate`,
	}, {
		name: "a webhook path",
		src: `app A:
    state seen: int = 0
    action ping:
        seen = seen + 1
    webhook "/hooks/{seen}" -> ping
    view V at "/":
        text "hi"
`,
		want: `webhook path "/hooks/{seen}" does not interpolate`,
	}, {
		name: "a state default",
		src: `app A:
    state who: text = "actor is {actor}"
    view V at "/":
        text "hi"
`,
		want: "a text literal does not interpolate",
	}, {
		// The funnel: every expression in the language is checked in one place, so
		// an argument, a `set` value and a `where` operand need no wiring of their own.
		name: "an action argument",
		src: `app A:
    state note: text = "" @client
    state log: text = ""
    action write(body: text):
        log = body
    view V at "/":
        button "go" -> write("note is {note}")
`,
		want: "a text literal does not interpolate",
	}, {
		name: "a where operand",
		src: `app A:
    entity Post:
        name: text
    state q: text = "" @client
    view V at "/":
        for p in Post where p.name == "{q}":
            text "{p.name}"
`,
		want: "a text literal does not interpolate",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil {
				t.Fatalf("expected a compile error containing %q, got none", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a compile error containing %q, got: %v", c.want, err)
			}
		})
	}
}

// The refusal is narrow on purpose, and this is the half that keeps it honest.
//
// FDL has no escape for a literal brace — `{` in text demands a `}` and the
// inside must parse, and `\{` is not a Go string escape — so a blanket refusal
// would make `{` unwritable in these positions with no way out. The rule is
// therefore scoped to braces that would have RENDERED something: they parse as an
// expression and read a name the scope defines. Everything else is text, and the
// f33d3r website depends on it — a JSON sample and a paragraph about FDL
// interpolation, both passed as component arguments.
func TestLiteralBracesThatNameNothingStillCompile(t *testing.T) {
	app, err := String(`app A:
    entity Post:
        name: text
    state n: int = 0 @client
    component Code(lang: text, body: text):
        text "{lang}"
        text "{body}"
    component Lbl(label: text, href: text):
        link "{label}" -> "{href}"
    view V at "/p/:id":
        use Code("json", "{\n  \"address\": \"post:1\"\n}")
        use Code("md", "` + "`class \\\"x-c-{tone}\\\"`" + ` interpolates; ` + "`style \\\"width: {pct}%\\\"`" + ` does not")
        box style "width: {pct}%":
            text "the braces are text: pct names nothing"
        for p in Post:
            use Lbl(p.name, "/p/" + p.id)
        text "n is {n}"
`)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	// The style survives with its braces intact — it is text, not a dropped value.
	var style string
	for _, nd := range app.Pages[0].View {
		if nd.Style != "" {
			style = nd.Style
		}
	}
	if style != "width: {pct}%" {
		t.Errorf("style = %q, want the literal braces preserved", style)
	}
}

// An interpolation may hold a string literal of its own.
//
// It could not, at any interpolating position, because the scan that found the
// end of a quoted value stopped at the first `"` without knowing it was inside a
// `{…}`. `class "x-avi-c{contains(name, "e")}"` therefore ended after
// `x-avi-c{contains(name, ` , the rest of the line was an unparsable tail, and the
// node reported as `unknown view node "box"` — which is why a library component
// hashed a name with `len()` and a ladder of `if` probes instead of one call.
// One case per position that ends a quoted value, so a scanner that forgets again
// fails here.
func TestInterpolationHoldsAStringLiteral(t *testing.T) {
	ir, err := String(`app A:
    state name: text = "ada" @client
    view V at "/":
        box class "x-avi-c{contains(name, "e")}":
            text "has e: {contains(name, "e")}"
            text "escaped, as it always did: {contains(name, \"e\")}"
            link "{upper(name)}" -> "/u/{contains(name, "e")}"
            input bind name placeholder "not {upper(name)}, is it"
    view U at "/u/:flag":
        text "{name}"
`)
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	box := ir.Pages[0].View[0]
	if len(box.ClassSegs) != 2 || box.ClassSegs[0].Lit != "x-avi-c" || box.ClassSegs[1].Bind == "" {
		t.Errorf("class = %+v, want the literal prefix then the interpolated call", box.ClassSegs)
	}
	// The two spellings of the nested string mean the same thing, so the two text
	// nodes lower to the same expression.
	a, b := box.Children[0].Segs, box.Children[1].Segs
	if len(a) != 2 || len(b) != 2 || a[1].Bind == "" || b[1].Bind == "" {
		t.Fatalf("text segs = %+v / %+v, want a literal and an interpolated call in each", a, b)
	}
	bound := map[string]string{}
	for _, bd := range ir.Bindings {
		j, _ := json.Marshal(bd.Expr)
		bound[bd.ID] = string(j)
	}
	if bound[a[1].Bind] != bound[b[1].Bind] {
		t.Errorf("a plain nested quote lowered to %s but the escaped spelling lowered to %s — they must be one expression",
			bound[a[1].Bind], bound[b[1].Bind])
	}
	// A `\"` in a class is a literal backslash (a class is never escape-decoded —
	// that is how `w-1\/2` survives), so it is refused with the spelling that works.
	_, err = String(`app A:
    state name: text = "ada" @client
    view V at "/":
        box class "x-{contains(name, \"e\")}":
            text "hi"
`)
	if err == nil || !strings.Contains(err.Error(), "plain quotes") {
		t.Errorf("class with an escaped nested quote: got %v, want the plain-quotes message", err)
	}
}
