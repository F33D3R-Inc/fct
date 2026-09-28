package compile

import (
	"strings"
	"testing"
)

// A view may link to what the app serves that is not a page — a declared
// `api GET` route (with `{param}` segments), a stream, the contract route, an
// upload — and is refused only when nothing serves the path.
func TestLinkToServedNonPageRoute(t *testing.T) {
	src := `app L:
    type Out:
        n: int
    type Tick:
        n: int
    action export(id: text) -> Out:
        return Out{n: 1}
    action tick():
        emit Tick{n: 1}
    api GET "/api/v2/things/{id}/export" -> export
    stream "/api/v2/ticks": Tick
    contract "/api/v2/contract"
    view V at "/":
        link "Export" -> "/api/v2/things/7/export"
        link "Ticks" -> "/api/v2/ticks"
        link "Contract history" -> "/api/v2/contract/history"
        link "A file" -> "/uploads/abc.png"
`
	if _, err := String(src); err != nil {
		t.Fatalf("links to served routes must compile: %v", err)
	}
	bad := strings.Replace(src, `"/api/v2/things/7/export"`, `"/api/v2/nothing"`, 1)
	if _, err := String(bad); err == nil || !strings.Contains(err.Error(), "no view serves that route") {
		t.Errorf("a link nothing serves: err = %v", err)
	}
	post := strings.Replace(src, `api GET "/api/v2/things/{id}/export"`, `api POST "/api/v2/things/{id}/export"`, 1)
	if _, err := String(post); err == nil {
		t.Error("a link to a POST-only route must be refused")
	}
}
