package integration

import (
	"strings"
	"testing"
)

// A guest's cart is keyed to `session`, and `session` survives the sign-in
// that rotates the cookie. Driven end to end against the real runtime: a
// fresh anonymous session adds a line, signs up (which rotates its cookie),
// and still sees the line — while a second, unrelated visitor sees none.
const guestCartApp = `app GuestCart:
    auth
    entity Product:
        id: int
        name: text
        stock: int
    entity CartLine:
        id: int
        visitor: text
        product: int
        qty: int
    derive cartLines: int = count(l in CartLine where l.visitor == session)
    action seed():
        add Product { name: "Pillar", stock: 5 }
    action addToCart(pid: int, n: int):
        check session != "" "no session"
        add CartLine { visitor: session, product: pid, qty: n }
    view Home at "/":
        text "lines={cartLines} actor={actor}"
        button "add" -> addToCart(1, 1)
`

func TestGuestCartSurvivesSignIn(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, guestCartApp)
	if code, body := a.action("seed"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}

	// A fresh anonymous visitor fills a cart.
	a.newSession()
	if code, body := a.action("addToCart", 1, 1); code != 200 {
		t.Fatalf("guest addToCart: %d %s", code, body)
	}
	if _, page := a.get("/"); !strings.Contains(serverText(page), "lines=1actor=guest") {
		t.Fatalf("the guest should see their line, got %q", serverText(page))
	}

	// Signing up rotates the session cookie; the cart must still be theirs.
	if code, body := a.action("signup", "ada", "pw12345678"); code != 200 {
		t.Fatalf("signup: %d %s", code, body)
	}
	if _, page := a.get("/"); !strings.Contains(serverText(page), "lines=1actor=ada") {
		t.Fatalf("after signing in the cart should still be there, got %q", serverText(page))
	}

	// Another visitor's session sees nothing of it.
	a.newSession()
	if _, page := a.get("/"); !strings.Contains(serverText(page), "lines=0actor=guest") {
		t.Fatalf("a different visitor must not see that cart, got %q", serverText(page))
	}
}
