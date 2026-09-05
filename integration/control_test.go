package integration

import (
	"html"
	"regexp"
	"strings"
	"testing"
)

// The controls, end to end.
//
// A control bound to a cell is the only thing in the language that may write a
// `@client` cell. Before these four existed, the only one was `input`, so a
// menu, a disclosure, a modal and a settings row were all unwritable — not for
// want of a way to open them, but for want of a control that could flip a bool.
// The fix is more controls, inside the existing rule, rather than a statement
// that assigns state around it.
//
// Every control on this page reaches it through a *reference parameter*. That is
// deliberate: the substitution in internal/ir/component.go is where a new node
// kind is easiest to forget, and forgetting it does not fail — the parameter's
// name simply is not replaced, and the control binds a cell that does not exist
// on the page. So none of these is written against a page cell directly.
const controlApp = `app Controls:
    state menuOpen: bool @client
    state notify: bool = true @client
    state plan: text = "pro" @client
    state note: text = "hello" @client
    state pass: text = "hunter2" @client
    state fresh: text = "" @client

    component MenuToggle(caption: text, open: cell bool):
        toggle bind open label "{caption}"

    component CheckboxField(caption: text, v: cell bool):
        checkbox bind v label "{caption}"

    component PlanChoice(v: cell text):
        radio bind v:
            option "Free" -> "free"
            option "Pro" -> "pro"

    component NoteField(hint: text, v: cell text):
        textarea bind v placeholder "{hint}"

    component PasswordField(hint: text, v: cell text):
        password bind v placeholder "{hint}"

    component NewPasswordField(hint: text, v: cell text):
        newpassword bind v placeholder "{hint}"

    view Home at "/":
        box:
            use MenuToggle("Account", menuOpen)
            use CheckboxField("Email me", notify)
            use PlanChoice(plan)
            use PasswordField("Password", pass)
            use NewPasswordField("Choose one", fresh)
            use NoteField("why?", note)
            overlay bind menuOpen:
                text "the menu is open"
`

// The same page with the menu already open, so the *server's* rendering of an
// open menu is observed rather than assumed. A dropdown that only opens after
// hydration is a dropdown that does not work.
var openMenuApp = strings.Replace(controlApp,
	"state menuOpen: bool @client", "state menuOpen: bool = true @client", 1)

var (
	inputTagRE  = regexp.MustCompile(`(?:<label class="([^"]*)">)?<input ([^>]*)>`)
	textareaRE  = regexp.MustCompile(`<textarea ([^>]*)>([^<]*)</textarea>`)
	selectTagRE = regexp.MustCompile(`(?s)<select ([^>]*)>(.*?)</select>`)
	optionTagRE = regexp.MustCompile(`<option value="([^"]*)"( selected)?>([^<]*)</option>`)
	oneAttrRE   = regexp.MustCompile(`([-\w]+)(?:="([^"]*)")?`)
)

// serverOptions reads a rendered <select>'s choices back in the shape the shim
// reports the client's: `value=label` per choice, a `*` on the selected one.
// The markup is escaped and the client's textContent is not, so the two are
// compared unescaped — the question is which characters ended up on the page,
// not which encoding each side had to use to get them there.
func serverOptions(inner string) string {
	var out []string
	for _, m := range optionTagRE.FindAllStringSubmatch(inner, -1) {
		sel := ""
		if m[2] != "" {
			sel = "*"
		}
		out = append(out, html.UnescapeString(m[1])+sel+"="+html.UnescapeString(m[3]))
	}
	return strings.Join(out, "|")
}

// serverControls reads back the controls the server wrote as markup, in the same
// shape the client reports the ones it built as DOM.
//
// It reads by element type — every <input>, then every <textarea>, then every
// <select> — while the client reports in DOM order, so the page under test keeps
// its controls grouped in that order and the two lists line up positionally. The two sides describe a
// control with different machinery — an HTML attribute on one, a DOM property on
// the other — and the whole question this file asks is whether they describe the
// same control.
func serverControls(markup string) []clientControl {
	var out []clientControl

	for _, m := range inputTagRE.FindAllStringSubmatch(markup, -1) {
		attrs := map[string]string{}
		for _, a := range oneAttrRE.FindAllStringSubmatch(m[2], -1) {
			attrs[a[1]] = a[2]
		}
		if _, ok := attrs["data-fa-input"]; !ok {
			continue
		}
		_, checked := attrs["checked"]
		out = append(out, clientControl{
			Tag: "INPUT", Bind: attrs["data-fa-input"], Type: attrs["type"],
			Name: attrs["name"], Value: attrs["value"], Checked: checked,
			Role: attrs["role"], Cls: m[1], Autocomplete: attrs["autocomplete"],
		})
	}

	for _, m := range textareaRE.FindAllStringSubmatch(markup, -1) {
		attrs := map[string]string{}
		for _, a := range oneAttrRE.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = a[2]
		}
		out = append(out, clientControl{
			Tag: "TEXTAREA", Bind: attrs["data-fa-input"], Value: m[2],
		})
	}

	for _, m := range selectTagRE.FindAllStringSubmatch(markup, -1) {
		attrs := map[string]string{}
		for _, a := range oneAttrRE.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = a[2]
		}
		out = append(out, clientControl{
			Tag: "SELECT", Bind: attrs["data-fa-input"], Options: serverOptions(m[2]),
		})
	}

	return out
}

// controlsByBind indexes a run's controls by the cell they write, keeping order
// within a group (a radio group is several controls over one cell).
func controlsByBind(cs []clientControl, bind string) []clientControl {
	var out []clientControl
	for _, c := range cs {
		if c.Bind == bind {
			out = append(out, c)
		}
	}
	return out
}

func checkedValues(cs []clientControl) []string {
	var out []string
	for _, c := range cs {
		if c.Checked {
			out = append(out, c.Value)
		}
	}
	return out
}

// The four controls must render the same control on both sides — not merely a
// similar-looking one.
//
// The server writes `checked` as an HTML attribute and the client assigns the
// `checked` DOM property; the server writes a radio's `name` and `value` as
// markup and the client sets them through setAttribute. Nothing makes those
// agree except both renderers being written to the same rule, which is why this
// compares them rather than trusting it.
func TestEveryControlRendersIdenticallyOnBothSides(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, controlApp)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	markup := mountedMarkup(page)
	if markup == "" {
		t.Fatal("the server rendered no mounted markup")
	}

	server := serverControls(markup)
	client, _ := runClient(t, page, nil)

	if len(server) != len(client.Controls) {
		t.Fatalf("the server rendered %d controls and the client %d:\nserver %+v\nclient %+v",
			len(server), len(client.Controls), server, client.Controls)
	}
	for i, want := range server {
		got := client.Controls[i]
		// A textarea's enclosing element differs by design (the server's is the
		// `box` it sits in, and the shim reports the DOM parent), so the class is
		// compared only where both sides put the control inside a label of their
		// own making.
		if got.Type == "" {
			got.Cls, want.Cls = "", ""
		}
		if got != want {
			t.Errorf("control %d:\n  server %+v\n  client %+v", i, want, got)
		}
	}

	t.Run("each control is the element it should be", func(t *testing.T) {
		// A toggle is a checkbox with a role and a class, not a node kind of its
		// own — so what distinguishes it must actually be rendered.
		for _, want := range []string{
			`<label class="fa-toggle"><input type="checkbox" data-fa-input="menuOpen" role="switch">`,
			`<label class="fa-checkbox"><input type="checkbox" data-fa-input="notify" checked>`,
			`<div class="fa-radio" role="radiogroup">`,
			`<input type="radio" name="plan" value="free" data-fa-input="plan">`,
			`<input type="radio" name="plan" value="pro" data-fa-input="plan" checked>`,
			`<textarea class="fa-textarea" data-fa-input="note" placeholder="why?">hello</textarea>`,
			// A password box is masked by the browser because the server said
			// `type="password"` on first paint — before any script runs, which is
			// exactly when the old CSS mask was not yet applied. The token beside
			// it is the whole of what separates the two keywords.
			`<input type="password" autocomplete="current-password" data-fa-input="pass" value="hunter2" placeholder="Password">`,
			`<input type="password" autocomplete="new-password" data-fa-input="fresh" value="" placeholder="Choose one">`,
		} {
			if !strings.Contains(markup, want) {
				t.Errorf("the server did not render %s", want)
			}
		}
	})

	t.Run("a radio group is one group", func(t *testing.T) {
		radios := controlsByBind(client.Controls, "plan")
		if len(radios) != 2 {
			t.Fatalf("want two radios over `plan`, got %d", len(radios))
		}
		// Grouping IS the shared cell: both radios carry the cell's name, which is
		// what makes a browser treat them as one-of-N without being told.
		for _, r := range radios {
			if r.Name != "plan" {
				t.Errorf("radio %q has name %q, want the bound cell's name — "+
					"without it the browser lets both be selected at once", r.Value, r.Name)
			}
		}
		if got := checkedValues(radios); len(got) != 1 || got[0] != "pro" {
			t.Errorf("checked radios = %v, want exactly [pro]", got)
		}
	})
}

// Writing the cell: the half of a control that first paint cannot show.
//
// This drives the shipped client the way a browser would — the element under the
// pointer changes, the event fires, and everything else has to follow from the
// cell. In particular the shim deliberately does NOT uncheck a radio's siblings
// when one is clicked, which a real browser would: if the client did not write
// the cell and re-sync the group from it, two radios would be checked here.
func TestAControlWritesItsCellAndEverythingBoundToItFollows(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, controlApp)

	code, page := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}

	t.Run("a checkbox flips its bool cell", func(t *testing.T) {
		_, after := runClient(t, page, []driveStep{{Sel: `[data-fa-input="notify"]`}})
		got := controlsByBind(after.Controls, "notify")
		if len(got) != 1 || got[0].Checked {
			t.Errorf("after clicking, the checkbox is %+v — the cell did not go false", got)
		}
	})

	t.Run("a radio selects one and deselects the rest", func(t *testing.T) {
		_, after := runClient(t, page, []driveStep{
			{Sel: `[data-fa-input="plan"][value="free"]`},
		})
		radios := controlsByBind(after.Controls, "plan")
		if got := checkedValues(radios); len(got) != 1 || got[0] != "free" {
			t.Errorf("after selecting `free`, checked = %v, want exactly [free] — "+
				"the group did not re-sync from its cell", got)
		}
	})

	t.Run("a textarea writes its text cell", func(t *testing.T) {
		_, after := runClient(t, page, []driveStep{
			{Sel: `[data-fa-input="note"]`, Do: "type", Value: "rewritten"},
		})
		// Read it back off the other control bound to nothing else: the textarea
		// itself, re-synced from the cell rather than from what was typed.
		got := controlsByBind(after.Controls, "note")
		if len(got) != 1 || got[0].Value != "rewritten" {
			t.Errorf("after typing, the textarea holds %+v, want \"rewritten\"", got)
		}
	})
}

// The payoff, and the thing that was impossible: a dropdown menu.
//
// A `toggle` bound to a `@client bool` that an `overlay bind` opens and closes.
// No new statement writes the cell — the control does, which is the rule this
// language already had and the reason the missing controls were the real gap.
func TestAToggleOpensAndClosesAnOverlayMenu(t *testing.T) {
	e := startEngine(t)

	t.Run("the server renders the menu closed, then open", func(t *testing.T) {
		closed := startApp(t, e, controlApp)
		code, page := closed.get("/")
		if code != 200 {
			t.Fatalf("GET /: %d", code)
		}
		if strings.Contains(mountedMarkup(page), "the menu is open") {
			t.Error("the overlay rendered its panel while its cell was false")
		}

		open := startApp(t, e, openMenuApp)
		code, openPage := open.get("/")
		if code != 200 {
			t.Fatalf("GET /: %d", code)
		}
		markup := mountedMarkup(openPage)
		if !strings.Contains(markup, "the menu is open") {
			t.Error("the overlay did not render its panel while its cell was true")
		}
		// ...and the toggle shows the same truth the overlay is reading.
		if !strings.Contains(markup, `data-fa-input="menuOpen" role="switch" checked`) {
			t.Error("the toggle is not checked on a page whose menu is open")
		}
	})

	t.Run("the client opens it by clicking the toggle", func(t *testing.T) {
		a := startApp(t, e, controlApp)
		code, page := a.get("/")
		if code != 200 {
			t.Fatalf("GET /: %d", code)
		}

		before, after := runClient(t, page, []driveStep{{Sel: `[data-fa-input="menuOpen"]`}})
		if strings.Contains(before.Text, "themenuisopen") {
			t.Fatal("the client rendered the menu open before anything was clicked")
		}
		if !strings.Contains(after.Text, "themenuisopen") {
			t.Errorf("clicking the toggle did not open the overlay; the page reads %q", after.Text)
		}
		if got := controlsByBind(after.Controls, "menuOpen"); len(got) != 1 || !got[0].Checked {
			t.Errorf("the toggle is %+v after being clicked, want checked", got)
		}
	})

	t.Run("and closes it by clicking again", func(t *testing.T) {
		a := startApp(t, e, controlApp)
		code, page := a.get("/")
		if code != 200 {
			t.Fatalf("GET /: %d", code)
		}

		_, after := runClient(t, page, []driveStep{
			{Sel: `[data-fa-input="menuOpen"]`},
			{Sel: `[data-fa-input="menuOpen"]`},
		})
		if strings.Contains(after.Text, "themenuisopen") {
			t.Error("clicking the toggle twice left the menu open")
		}
	})
}
