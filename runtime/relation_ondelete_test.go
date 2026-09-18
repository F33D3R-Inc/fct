package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A relation field's on-delete modifier is enforced by the store, not merely
// accepted by the compiler — `facet check` reporting "ok" on a parsed
// `@restrict` field would be worthless if the delete it names still went
// through. This is the fix for apps/storefront/gaps/relation-always-cascades.fct:
// a storefront wants "you can't delete a Product while a CartLine references
// it" (restrict), not silent cascading deletion of carts.
const restrictApp = `app Shop:
    entity Product:
        id: int
        name: text
    entity CartLine:
        id: int
        product: Product @restrict
        qty: int
    action addProduct(name: text):
        add Product { name: name }
    action addLine(product: int, qty: int):
        add CartLine { product: product, qty: qty }
    action removeProduct(id: int):
        remove Product(id)
    view Home at "/":
        box:
            text "{count(Product)}"
`

// TestRestrictRefusesTheDelete proves @restrict at the database level: it runs
// a live in-process server (NewInMemory, the same backend `facet test`/`facet
// console` use), adds a Product a CartLine actually references, and asserts the
// delete is genuinely rejected — the row survives in the store, not just that
// the app compiled.
func TestRestrictRefusesTheDelete(t *testing.T) {
	g, err := compile.String(restrictApp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(apiReadEnv, "*")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) int {
		t.Helper()
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	rows := func(entity string) []any {
		t.Helper()
		out, _ := getJSON(t, ts.URL+"/api/"+entity)["rows"].([]any)
		return out
	}

	if code := post("/api/addProduct", `{"args":["widget"]}`); code != 200 {
		t.Fatalf("addProduct should succeed, got %d", code)
	}
	if code := post("/api/addLine", `{"args":[1,3]}`); code != 200 {
		t.Fatalf("addLine should succeed, got %d", code)
	}
	if got := len(rows("Product")); got != 1 {
		t.Fatalf("want 1 Product before the refused delete, got %d", got)
	}

	// The delete this action names is exactly what @restrict exists to refuse:
	// a CartLine still points at Product(1).
	if code := post("/api/removeProduct", `{"args":[1]}`); code == 200 {
		t.Error("deleting a @restrict-referenced Product answered 200; the delete should have been refused")
	}

	// Both sides must have survived — the parent AND the child that referenced
	// it — not merely "the request said 500 while the store went ahead anyway".
	if got := rows("Product"); len(got) != 1 {
		t.Errorf("the restricted Product was deleted anyway: %d rows, want 1", len(got))
	}
	if got := rows("CartLine"); len(got) != 1 {
		t.Errorf("the referencing CartLine did not survive: %d rows, want 1", len(got))
	}

	// The working set the API/SSE layer reads is the same store checkRestrict
	// guarded, so it must agree with what the database itself holds.
	srv.mu.Lock()
	inMemProducts := len(srv.entities["Product"])
	srv.mu.Unlock()
	if inMemProducts != 1 {
		t.Errorf("in-memory working set shows %d Product rows, want 1 (undo should have restored it)", inMemProducts)
	}
}

// TestSetNullClearsTheReferenceInsteadOfDeleting proves the second on-delete
// mode end to end: a review that merely remembers which product it was about
// (optional, `@setNull`) survives the product's deletion with its reference
// cleared, rather than being cascade-deleted or blocking the delete.
const setNullApp = `app Shop2:
    entity Product:
        id: int
        name: text
    entity Review:
        id: int
        product: Product? @setNull
        body: text
    action addProduct(name: text):
        add Product { name: name }
    action addReview(product: int, body: text):
        add Review { product: product, body: body }
    action removeProduct(id: int):
        remove Product(id)
    view Home at "/":
        box:
            text "{count(Product)}"
`

func TestSetNullClearsTheReferenceInsteadOfDeleting(t *testing.T) {
	g, err := compile.String(setNullApp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(apiReadEnv, "*")
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(path, body string) int {
		t.Helper()
		r, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	if code := post("/api/addProduct", `{"args":["widget"]}`); code != 200 {
		t.Fatalf("addProduct should succeed, got %d", code)
	}
	if code := post("/api/addReview", `{"args":[1,"great"]}`); code != 200 {
		t.Fatalf("addReview should succeed, got %d", code)
	}
	if code := post("/api/removeProduct", `{"args":[1]}`); code != 200 {
		t.Fatalf("@setNull should let the delete through, got %d", code)
	}

	reviews, _ := getJSON(t, ts.URL+"/api/Review")["rows"].([]any)
	if len(reviews) != 1 {
		t.Fatalf("the review should survive (its reference cleared, not the row deleted), got %d rows", len(reviews))
	}
	if got := reviews[0].(map[string]any)["product"]; got != nil {
		t.Errorf("Review.product = %v after its Product was deleted, want nil (@setNull)", got)
	}
}
