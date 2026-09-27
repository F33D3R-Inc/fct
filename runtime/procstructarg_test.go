package runtime

import (
	"testing"

	"facet/internal/compile"
)

// A proc typed over a wire `type` must read the rows an action projected
// into that type: `list(dto(w) in Work …)` builds each row as a map, while
// the compiled proc body reads its struct-typed parameter through the type's
// layout. The declared parameter type is the contract, so the `do` boundary
// converts to it — recursively, lists and nested types included — and a proc
// can reorder, filter or inspect projected rows like any other value.
func TestProcReadsProjectedRowsByDeclaredType(t *testing.T) {
	g, err := compile.String(`
app Hydrate:
    entity Work:
        id: int
        body: text
        author: text
    type AuthorDTO:
        handle: text
    type WorkDTO:
        id: text
        body: text
        author: AuthorDTO
    type PageDTO:
        items: [WorkDTO]
        first_author: text
    derive workDTO(w: Work): WorkDTO = WorkDTO{id: "" + w.id, body: w.body, author: AuthorDTO{handle: w.author}}
    proc inOrder(rows: [WorkDTO], ids: [int]) -> [WorkDTO]:
        let mut out = []
        let mut i = 0
        loop i < len(ids):
            let want = "" + ids[i]
            let mut j = 0
            loop j < len(rows):
                if rows[j].id == want:
                    out = out + [rows[j]]
                j = j + 1
            i = i + 1
        return out
    proc firstAuthor(rows: [WorkDTO]) -> text:
        if len(rows) == 0:
            return ""
        return rows[0].author.handle
    action seed:
        add Work { body: "one", author: "ada" }
        add Work { body: "two", author: "bob" }
        add Work { body: "three", author: "cy" }
    action hydrate(ids: [int]) -> PageDTO:
        let found = list(workDTO(w) in Work where w.id in ids by id)
        let items = do inOrder(found, ids)
        let first = do firstAuthor(items)
        return PageDTO{items: items, first_author: first}
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "seed", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := srv.RunValue("ada", "member", true, "hydrate", []any{[]any{3, 999, 1}})
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	page := got.(map[string]any)
	items, _ := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want the two existing works in asked order", page)
	}
	if b := items[0].(map[string]any)["body"]; b != "three" {
		t.Errorf("items[0].body = %v, want \"three\" (asked first)", b)
	}
	if b := items[1].(map[string]any)["body"]; b != "one" {
		t.Errorf("items[1].body = %v, want \"one\" (asked last)", b)
	}
	if fa := page["first_author"]; fa != "cy" {
		t.Errorf("first_author = %v, want \"cy\" (a nested type's field, read in a proc)", fa)
	}
}
