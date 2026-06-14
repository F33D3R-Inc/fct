package fa

import (
	"strings"
	"testing"
)

// TestComputedFieldRender: a computed field is derived at render time from the
// supplied inputs (the caller never passes it) and is usable in looks both as a
// value and in a condition. A later computed field may use an earlier one.
func TestComputedFieldRender(t *testing.T) {
	const src = `
facet Cart:
    what:
        price: int
        qty: int
        total = price * qty
        free: bool = total >= 100
    looks:
        <div class="cart">
            <span class="total">{total}</span>
            if free:
                <span class="ship">free shipping</span>
            else:
                <span class="ship">pay shipping</span>
        </div>
`
	c, err := Compile(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// Caller supplies only price and qty; total/free are derived.
	html, err := c.Render("Cart", map[string]any{"Price": 30, "Qty": 4})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<span class="total">120</span>`) {
		t.Errorf("computed total not rendered (want 120):\n%s", got)
	}
	if !strings.Contains(got, "free shipping") || strings.Contains(got, "pay shipping") {
		t.Errorf("computed bool `free` (total>=100) not honored:\n%s", got)
	}

	// Below the free-shipping threshold.
	html2, _ := c.Render("Cart", map[string]any{"Price": 10, "Qty": 2})
	if g := string(html2); !strings.Contains(g, "pay shipping") || strings.Contains(g, "free shipping") {
		t.Errorf("computed bool should be false for total 20:\n%s", g)
	}
}

// TestComputedForwardRef: a computed field referencing a not-yet-declared field
// is a compile error (the var would be used before definition).
func TestComputedForwardRef(t *testing.T) {
	const bad = `
facet T:
    what:
        a: int
        x = y + 1
        y = a + 1
    looks:
        <p>{x}</p>
`
	_, err := Compile(bad)
	if err == nil {
		t.Fatal("expected compile error for forward reference in computed field")
	}
	if !strings.Contains(err.Error(), "y") {
		t.Errorf("error should name the undeclared reference, got: %v", err)
	}
}
