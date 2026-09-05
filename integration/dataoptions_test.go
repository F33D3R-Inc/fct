package integration

import (
	"strings"
	"testing"
)

// A control's choices, drawn from data.
//
// `Options` was a fixed compile-time array, so every dropdown in the language
// offered exactly the choices its author had typed. A country picker, a category
// selector, a "reply to" list — every control whose choices are rows — was
// unwritable, and no amount of interpolation helped: interpolation fills a value
// in, and what was missing was the *repetition* that produces one choice per row.
//
// So a choice list learned the language's existing repeating header. This page
// writes both shapes at once — a fixed placeholder followed by a `for`, and a
// radio group that is nothing but a `for` — over an entity whose rows arrive
// only at runtime.
//
// It is checked end to end because the two halves that can silently disagree are
// exactly the two this exercises: the server's markup and the shipped client's
// DOM must offer the same choices, and the list must still be right after the
// collection behind it changes.
const dataOptionsApp = `app Catalog:
    entity Category:
        name: text
        slug: text
        rank: int

    state chosen: text = "" @client
    state tone: text = "" @client

    state tags: [text] = ["red", "green", "blue"] @client
    state hide: text = "" @client
    state picked: text = "" @client

    action addCategory(name: text, slug: text, rank: int):
        add Category { name: name, slug: slug, rank: rank }

    view Home at "/":
        box:
            select bind chosen:
                option "— any —" -> ""
                for c in Category by rank:
                    option "{c.name}" -> c.slug
            radio bind tone:
                for c in Category by rank:
                    option "{c.name}" -> c.slug
            input bind hide
            select bind picked:
                for tg in tags where tg != hide:
                    option "{tg}" -> tg
`

// seedCategories inserts the rows the page's choices are drawn from.
func seedCategories(t *testing.T, a *app) {
	t.Helper()

	for _, c := range []struct {
		name, slug string
		rank       int
	}{
		{"Books & Zines", "books", 1},
		{"Toys", "toys", 2},
	} {
		if code, body := a.action("addCategory", c.name, c.slug, c.rank); code != 200 {
			t.Fatalf("seeding %q: %d %s", c.name, code, body)
		}
	}
}

// selectOptions is the choice list a run's dropdown over `bind` is offering.
func selectOptions(run clientRun, bind string) string {
	for _, c := range run.Controls {
		if c.Tag == "SELECT" && c.Bind == bind {
			return c.Options
		}
	}
	return "<no select over " + bind + ">"
}

func TestAChoiceListDrawnFromDataRendersTheSameOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, dataOptionsApp)
	seedCategories(t, a)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)

	t.Run("the server renders the rows as choices", func(t *testing.T) {
		// The label interpolates the row and is escaped as markup; the value is the
		// row's own field, which no literal option could have named.
		for _, want := range []string{
			`<option value="" selected>— any —</option>`,
			`<option value="books">Books &amp; Zines</option>`,
			`<option value="toys">Toys</option>`,
			`<input type="radio" name="tone" value="books" data-fa-input="tone">`,
			`<input type="radio" name="tone" value="toys" data-fa-input="tone">`,
		} {
			if !strings.Contains(markup, want) {
				t.Errorf("the server did not render %s\n%s", want, markup)
			}
		}
		// A control whose choices come from data is a region — without the marker
		// the client has nothing to re-fill when the collection changes.
		if !strings.Contains(markup, `<select data-fa-region=`) {
			t.Error("the select is not marked as a region, so its options can never refresh")
		}
	})

	t.Run("the client builds the same choices", func(t *testing.T) {
		server := serverControls(markup)
		client, _ := runClient(t, page, nil)

		// Matched by identity rather than by position: serverControls reads the
		// markup one control kind at a time and the shim walks the DOM in document
		// order, so the two lists hold the same controls in different orders.
		if len(server) != len(client.Controls) {
			t.Fatalf("the server rendered %d controls and the client %d:\nserver %+v\nclient %+v",
				len(server), len(client.Controls), server, client.Controls)
		}
		key := func(c clientControl) string { return c.Tag + "/" + c.Bind + "/" + c.Value }
		byKey := map[string]clientControl{}
		for _, c := range client.Controls {
			c.Cls = "" // the enclosing element differs by design
			byKey[key(c)] = c
		}
		for _, want := range server {
			want.Cls = ""
			got, ok := byKey[key(want)]
			if !ok {
				t.Errorf("the client never rendered the control the server did: %+v", want)
				continue
			}
			if got != want {
				t.Errorf("control %s:\n  server %+v\n  client %+v", key(want), want, got)
			}
		}

		// Said again as the property, so a failure reads as what broke rather than
		// as a struct diff: the dropdown offers the placeholder plus one choice per
		// row, in the order the `by rank` asked for.
		if want := "*=— any —|books=Books & Zines|toys=Toys"; selectOptions(client, "chosen") != want {
			t.Errorf("the client's dropdown offers %q, want %q", selectOptions(client, "chosen"), want)
		}
	})
}

// The half that first paint cannot show: the collection changes and the choices
// follow it.
//
// This is the failure mode a data-driven dropdown has and a fixed one cannot: it
// paints correctly and then goes stale, which looks exactly like a dropdown that
// works. It holds only if the compiler wrote a dependency edge from the
// collection to the control, and the client re-filled the control from it.
func TestChoicesDrawnFromDataFollowTheirCollection(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, dataOptionsApp)
	seedCategories(t, a)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	// Selecting a row's value writes the cell, and the control re-renders from it
	// — a data-driven choice is still a two-way control.
	before, after := runClient(t, page, []driveStep{
		{Sel: `[data-fa-input="tone"][value="toys"]`},
	})
	if got := checkedValues(controlsByBind(before.Controls, "tone")); len(got) != 0 {
		t.Errorf("a radio was already selected before anything was clicked: %v", got)
	}
	if got := checkedValues(controlsByBind(after.Controls, "tone")); len(got) != 1 || got[0] != "toys" {
		t.Errorf("after selecting a row's value, checked = %v, want exactly [toys]", got)
	}

	// A new row, and a page served after it: the choices grew without a line of
	// the view changing.
	if code, body := a.action("addCategory", "Vinyl", "vinyl", 3); code != 200 {
		t.Fatalf("adding a category: %d %s", code, body)
	}
	code, page = a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	run, _ := runClient(t, page, nil)

	if got := selectOptions(run, "chosen"); !strings.Contains(got, "vinyl=Vinyl") {
		t.Errorf("the new row is not among the client's choices: %q", got)
	}
	if got := len(controlsByBind(run.Controls, "tone")); got != 3 {
		t.Errorf("the radio group offers %d choices, want 3 — the new row did not reach it", got)
	}
}

// The client re-fills a data-driven choice list when the data behind it moves.
//
// This is the edge that a `for` region has always had and a control never did,
// and it is the one that fails silently: without the dependency edge the compiler
// now writes from the collection to the control, and without the control being a
// region the client can re-fill, the dropdown paints once and then lies.
//
// It is exercised over a `[T]` client cell filtered by another client cell,
// because that is the whole loop with no authority in it: a keystroke changes the
// filter, the client re-resolves the rows itself, and the options must follow
// within the same frame.
func TestAChoiceListRefillsWhenItsRowsChangeOnTheClient(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, dataOptionsApp)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	before, after := runClient(t, page, []driveStep{
		{Sel: `[data-fa-input="hide"]`, Do: "type", Value: "green"},
	})
	if want := "red=red|green=green|blue=blue"; selectOptions(before, "picked") != want {
		t.Fatalf("first paint offers %q, want %q", selectOptions(before, "picked"), want)
	}
	if want := "red=red|blue=blue"; selectOptions(after, "picked") != want {
		t.Errorf("after filtering, the dropdown offers %q, want %q — its options did not follow the data",
			selectOptions(after, "picked"), want)
	}
}
