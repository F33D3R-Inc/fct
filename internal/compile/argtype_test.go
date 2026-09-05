package compile

import (
	"strings"
	"testing"
)

// A `use` checked how many arguments it was handed and never what they were, so
// a text landing in an `int` parameter compiled clean and became a 0 at render
// time — toInt is total, so there was nothing to report and nothing to see but
// the wrong row. These are the boundaries of the check that closes that.
func TestAComponentArgumentIsCheckedAgainstItsParametersType(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{{
		name: "a text where a number was declared is refused",
		src: `app A:
    state q: text = "" @client
    component C(n: int):
        text "{n}"
    view V at "/":
        use C(q)
`,
		want: `component "C" parameter "n" is int, but argument 1 is text`,
	}, {
		name: "a row's text field where a number was declared is refused",
		src: `app A:
    entity Post:
        title: text
        views: int
    component C(n: int):
        text "{n}"
    view V at "/":
        for p in Post:
            use C(p.title)
`,
		want: `parameter "n" is int, but argument 1 is text`,
	}, {
		name: "the position is named, so a wrong argument in a long signature is findable",
		src: `app A:
    entity Post:
        title: text
        at: int
    component Byline(author: text, href: text, at: int):
        text "{author} {href} {at}"
    view V at "/":
        for p in Post:
            use Byline(p.title, p.title, p.title)
`,
		want: `parameter "at" is int, but argument 3 is text`,
	}, {
		name: "a text where a bool was declared is refused: truthy(\"false\") is true",
		src: `app A:
    state q: text = "" @client
    component C(on: bool):
        if on:
            text "yes"
    view V at "/":
        use C(q)
`,
		want: `parameter "on" is bool, but argument 1 is text`,
	}, {
		name: "a list where a scalar was declared is refused",
		src: `app A:
    state tags: [text] = [] @client
    component C(t: text):
        text "{t}"
    view V at "/":
        use C(tags)
`,
		want: `parameter "t" is text, but argument 1 is [text]`,
	}, {
		name: "a component's own parameter is typed, so a wrong hand-off inside a body is refused",
		src: `app A:
    component Inner(n: int):
        text "{n}"
    component Outer(label: text):
        use Inner(label)
    view V at "/":
        use Outer("x")
`,
		want: `component "Inner" parameter "n" is int, but argument 1 is text`,
	}, {
		name: "an `if` does not lose the row's type on the way in",
		src: `app A:
    entity Post:
        title: text
        live: bool
    component C(n: int):
        text "{n}"
    view V at "/":
        for p in Post:
            if p.live:
                use C(p.title)
`,
		want: `parameter "n" is int, but argument 1 is text`,
	}, {
		name: "a route parameter is text, so handing one to an int parameter is refused",
		src: `app A:
    component C(n: int):
        text "{n}"
    view V at "/tag/:tagname":
        use C(tagname)
`,
		want: `parameter "n" is int, but argument 1 is text`,
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

// The other half of the rule, and the more important half: everything the tree
// already does must keep compiling. Every case here is a shape the real library
// or one of the two real apps uses.
func TestWideningIntoTextStaysLegal(t *testing.T) {
	cases := []struct{ name, src string }{{
		// facets/layout/sectionheader.fct: "a subtitle is a sentence, sometimes a
		// number, and an int argument converts into a text parameter for free".
		name: "a count fills a text parameter",
		src: `app A:
    entity Category:
        name: text
    component Section(title: text, subtitle: text):
        text "{title} {subtitle}"
    view V at "/":
        use Section("Categories", count(Category))
`,
	}, {
		name: "a row's int field fills a text parameter",
		src: `app A:
    entity Product:
        name: text
        stock: int
    component Cell(cellText: text):
        text "{cellText}"
    view V at "/":
        for p in Product:
            use Cell(p.stock)
`,
	}, {
		name: "money() is a formatter, so its result is text",
		src: `app A:
    entity Product:
        price: money
    component Cell(cellText: text):
        text "{cellText}"
    view V at "/":
        for p in Product:
            use Cell(money(p.price))
`,
	}, {
		name: "a money amount fills an int parameter and the reverse",
		src: `app A:
    entity Product:
        price: money
        qty: int
    component C(a: int, b: money):
        text "{a} {b}"
    view V at "/":
        for p in Product:
            use C(p.price, p.qty)
`,
	}, {
		name: "an enum member is text",
		src: `app A:
    enum Status: draft, live
    state s: Status = Status.draft @client
    component C(t: text):
        text "{t}"
    view V at "/":
        use C(s)
`,
		// and the reverse: a text-typed field fills an enum parameter
	}, {
		name: "a relation is the referenced row's id, so it fills an int parameter",
		src: `app A:
    entity User:
        name: text
    entity Post:
        author: User
    component C(id: int):
        text "{id}"
    view V at "/":
        for p in Post:
            use C(p.author)
`,
	}, {
		// A parameter whose declared type the language does not have is refused
		// where it is declared (entityparam_test.go), not here — the check's job
		// is to catch a wrong argument.
		name: "concatenation is text",
		src: `app A:
    entity Post:
        slug: text
    component C(href: text):
        text "{href}"
    view V at "/":
        for p in Post:
            use C("/post/" + p.slug)
`,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := String(c.src); err != nil {
				t.Fatalf("expected this to compile, got: %v", err)
			}
		})
	}
}
