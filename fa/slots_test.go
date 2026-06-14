package fa

import (
	"os"
	"strings"
	"testing"
)

// TestSlotFilled: a block-form child renders the parent's content (in the
// PARENT's scope) at the child's slot — including a nested child facet.
func TestSlotFilled(t *testing.T) {
	src, err := os.ReadFile("../examples/layout.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := c.Render("Page", map[string]any{
		"User": map[string]any{"ID": "42", "Name": "Ada", "URL": "/a.png", "Handle": "ada"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)

	for _, want := range []string{
		`class="card__title">Welcome`,    // child's own data
		`Hello, Ada!`,                    // slot content, evaluated in PARENT scope
		`data-facet-id="Avatar:user:42"`, // a child facet nested inside the slot
		`src="/a.png"`,                   // its prop, passed through from the parent
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "No content.") {
		t.Errorf("default slot content should be replaced when filled:\n%s", got)
	}
}

// TestSlotDefault: using the layout facet with no block content shows the
// slot's default.
func TestSlotDefault(t *testing.T) {
	src, err := os.ReadFile("../examples/layout.fct")
	if err != nil {
		t.Skipf("example missing: %v", err)
	}
	c, err := Compile(string(src))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Card has no who:, so plain Render is allowed; no __children → default shows.
	html, err := c.Render("Card", map[string]any{"Title": "Empty"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, "card__title\">Empty") {
		t.Errorf("title missing: %s", got)
	}
	if !strings.Contains(got, "No content.") {
		t.Errorf("expected default slot content when empty:\n%s", got)
	}
}

// namedSlotsFDL: a Frame facet with two named slots (header/footer) plus the
// default slot, and a Page that fills them via `fill name:`.
const namedSlotsFDL = `
facet Frame:
    what:
        title: str
    looks:
        <div class="frame">
            <header>
                slot header:
                    <span>default head</span>
            </header>
            <main>
                slot:
            </main>
            <footer>
                slot footer:
                    <span>default foot</span>
            </footer>
        </div>

facet Page:
    what:
        name: str
    looks:
        <Frame title="Hi">
            fill header:
                <h1>Header for {name}</h1>
            <p>body for {name}</p>
            fill footer:
                <small>Footer for {name}</small>
        </Frame>
`

// TestNamedSlotsFilled: each `fill name:` lands at its matching `slot name:`,
// the unnamed content goes to the default slot, and all render in PARENT scope.
func TestNamedSlotsFilled(t *testing.T) {
	c, err := Compile(namedSlotsFDL)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := c.Render("Page", map[string]any{"Name": "Ada"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		"<h1>Header for Ada</h1>", // named slot "header"
		"<p>body for Ada</p>",     // default slot
		"<small>Footer for Ada</small>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n---\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"default head", "default foot"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("filled named slot should replace its default, found %q\n%s", unwanted, got)
		}
	}
	// header must render before footer (slot order in the child, not fill order).
	if i, j := strings.Index(got, "Header for Ada"), strings.Index(got, "Footer for Ada"); i < 0 || j < 0 || i > j {
		t.Errorf("named slots rendered out of order:\n%s", got)
	}
}

// TestNamedSlotDefault: an unfilled named slot shows its own default content.
func TestNamedSlotDefault(t *testing.T) {
	c, err := Compile(namedSlotsFDL)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := c.Render("Frame", map[string]any{"Title": "x"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	for _, want := range []string{"default head", "default foot"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected named-slot default %q:\n%s", want, got)
		}
	}
}

// TestFillUnknownSlot: filling a slot the child doesn't declare is a compile
// error — the compiler has your back on a typo'd slot name.
func TestFillUnknownSlot(t *testing.T) {
	const bad = `
facet Box:
    looks:
        <div>
            slot header:
                x
        </div>

facet Use:
    looks:
        <Box>
            fill heeder:
                <p>oops</p>
        </Box>
`
	_, err := Compile(bad)
	if err == nil {
		t.Fatal("expected compile error for fill targeting unknown slot")
	}
	if !strings.Contains(err.Error(), "no slot") {
		t.Errorf("error should name the missing slot, got: %v", err)
	}
}
