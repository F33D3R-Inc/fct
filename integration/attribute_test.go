package integration

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// One component, parameterized, using every node attribute that used to take a
// literal string. Before this existed, `placeholder "{hint}"` rendered the four
// characters `{hint}` and a form's `"Send {hint}"` lost the interpolation
// altogether — so a reusable field, tab strip or select could not be written.
//
// The value comes out of the database rather than out of this source, because
// that is where a hostile one comes from: `hint` is a row's `name`, written
// through a real action.
const attributeApp = `app Fields:
    state draft: text @client
    state pick: text @client
    state which: text @client
    state file: text @client
    state tagq: text @client

    entity Tag:
        id: int
        name: text

    action add(name: text):
        add Tag { name: name }

    action submit:
        draft = ""

    component Field(hint: text, glyph: text, c: cell text):
        box:
            input bind c placeholder "{hint}"
            typeahead bind tagq from Tag.name placeholder "{hint}"
            icon "{glyph}"
            upload bind file label "up {hint}"
            form "send {hint}" -> submit:
                text "body"
            select bind pick:
                option "opt {hint}" -> "v1"
            tabs bind which:
                tab "tab {hint}" -> "a":
                    text "one"
            link "link {hint}" -> "/"

    view Home at "/":
        box:
            for t in Tag by id:
                use Field(t.name, t.name, draft)
`

// Everything a value must not be able to do once it lands in an attribute the
// server writes by hand: close it, open an event handler, or open a tag.
const hostileValue = `" onmouseover="alert(1) ' > < & <script>x()</script>`

var (
	placeholderRE = regexp.MustCompile(`placeholder="([^"]*)"`)
	iconRE        = regexp.MustCompile(`data-fa-icon="([^"]*)"`)
)

// mountedMarkup is the page the server rendered, tags and all — the region the
// client replaces. The IR and state payloads below it are JSON inside a script
// element, where a bare quote means nothing, so including them would make an
// inertness check answer about the wrong thing.
func mountedMarkup(page string) string {
	const open = `<div id="fa-root" data-fa-mount>`

	start := strings.Index(page, open)
	if start < 0 {
		return ""
	}
	start += len(open)

	end := strings.Index(page[start:], `<script type="application/json" id="fa-ir">`)
	if end < 0 {
		end = len(page) - start
	}

	return page[start : start+end]
}

func attrValues(t *testing.T, re *regexp.Regexp, markup string) []string {
	t.Helper()

	var out []string
	for _, m := range re.FindAllStringSubmatch(markup, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}

	return out
}

// clientValues returns every value the client put in one place.
func clientValues(attrs []clientAttr, tag, name string) []string {
	var out []string
	for _, a := range attrs {
		if a.Tag == tag && a.Name == name {
			out = append(out, a.Value)
		}
	}
	return out
}

// A node attribute must interpolate like node text does, and the value must be
// inert once it gets there — on both sides, identically.
//
// The two halves are one test on purpose. Escaping that only the server does is
// worth nothing: the client re-renders the same page a few milliseconds later,
// and if it produced different characters the page would be safe for exactly as
// long as it took to hydrate.
func TestANodeAttributeInterpolatesAndCannotBreakOut(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, attributeApp)

	if code, body := a.action("add", hostileValue); code != 200 {
		t.Fatalf("add: %d %s", code, body)
	}

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	if markup == "" {
		t.Fatal("the server rendered no mounted markup")
	}

	t.Run("the attribute interpolates", func(t *testing.T) {
		// The failure this replaces: the interpolation reached the browser as its
		// own source text.
		for _, literal := range []string{"{hint}", "{glyph}"} {
			if strings.Contains(markup, literal) {
				t.Errorf("the page still contains the literal %s — the attribute did not interpolate", literal)
			}
		}

		// A form's submit label went through a flattener that kept only the
		// literal segments, so `"send {hint}"` reached the IR as `"send "`. A
		// present-but-truncated label is not caught by looking for `{hint}`.
		for _, want := range []string{"up ", "send ", "opt ", "tab ", "link "} {
			if !strings.Contains(markup, want+html.EscapeString(hostileValue)) {
				t.Errorf("no label reading %q followed by the interpolated value; "+
					"the interpolation was dropped rather than rendered", want)
			}
		}

		if got := attrValues(t, placeholderRE, markup); len(got) != 2 {
			t.Errorf("want two placeholders (input + typeahead), got %d: %q", len(got), got)
		} else {
			for _, v := range got {
				if v != hostileValue {
					t.Errorf("placeholder = %q, want the interpolated value %q", v, hostileValue)
				}
			}
		}

		if got := attrValues(t, iconRE, markup); len(got) != 1 || got[0] != hostileValue {
			t.Errorf("data-fa-icon = %q, want [%q]", got, hostileValue)
		}
	})

	t.Run("the value stays inside its attribute", func(t *testing.T) {
		// The value carries these verbatim. If any survives into the markup, it
		// left the attribute the renderer opened.
		for _, bad := range []string{`" onmouseover="`, `<script>`, `' >`} {
			if strings.Contains(markup, bad) {
				i := strings.Index(markup, bad)
				t.Errorf("the rendered page contains %q — an interpolated value "+
					"escaped its attribute:\n…%s…", bad, markup[max(i-80, 0):min(i+80, len(markup))])
			}
		}
		// The word survives, escaped, and that is the point: `onmouseover` is an
		// event handler only when a real quote ends the attribute before it. Here
		// the quote is an entity, so the browser is still inside the placeholder's
		// value and the whole thing is eight characters of text.
		if !strings.Contains(markup, `&#34; onmouseover=&#34;alert(1)`) {
			t.Error("the hostile value is not present in its escaped form; this test " +
				"is no longer looking at the value it thinks it is")
		}

		// Said once more as a property, over the values themselves rather than the
		// page: what the renderer wrote between the quotes carries no markup.
		for _, raw := range append(placeholderRE.FindAllStringSubmatch(markup, -1),
			iconRE.FindAllStringSubmatch(markup, -1)...) {
			// A raw `"` is already impossible — the regex captures up to the first
			// one — so what is left to check is that no tag can be opened either.
			for _, bad := range []string{"<", ">"} {
				if strings.Contains(raw[1], bad) {
					t.Errorf("attribute value %q still contains %q", raw[1], bad)
				}
			}
		}
	})

	t.Run("the client agrees character for character", func(t *testing.T) {
		attrs := renderClientAttrs(t, page)
		if len(attrs) == 0 {
			t.Fatal("the client rendered nothing to compare against")
		}

		// What the server escaped must decode to exactly what the client assigns
		// through the DOM. Anything else and hydration rewrites the page's safety.
		for _, c := range []struct {
			what        string
			server      []string
			client      []string
			wantPerSide int
		}{
			{"placeholder", attrValues(t, placeholderRE, markup), clientValues(attrs, "INPUT", "placeholder"), 2},
			{"icon glyph", attrValues(t, iconRE, markup), clientValues(attrs, "SPAN", "data-fa-icon"), 1},
		} {
			if len(c.server) != c.wantPerSide || len(c.client) != c.wantPerSide {
				t.Errorf("%s: server produced %d, client produced %d, want %d each",
					c.what, len(c.server), len(c.client), c.wantPerSide)
				continue
			}
			for i := range c.server {
				if c.server[i] != c.client[i] {
					t.Errorf("%s #%d: server %q, client %q", c.what, i, c.server[i], c.client[i])
				}
			}
		}

		// The labels are text content, not attributes, and the client sets them
		// through textContent — the same rule, the other context.
		for _, want := range []string{
			"up " + hostileValue,
			"send " + hostileValue,
			"opt " + hostileValue,
			"tab " + hostileValue,
			"link " + hostileValue,
		} {
			found := false
			for _, a := range attrs {
				if a.Name == "text" && a.Value == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the client rendered no control labelled %q", want)
			}
		}
	})
}
