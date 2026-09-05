package integration

import (
	"strings"
	"testing"
)

// A multi-field update of an entity something else points at. `Comment` makes
// `Post` a relation TARGET, which is precisely what gives Post a unique index on
// `id` (fqWantedIndexes), and the unique index is what the second `insert_node`
// at the same address in one batch collides with.
const twoSetApp = `app Blog:
    entity Post:
        id: int
        title: text
        body: text
    entity Comment:
        id: int
        post: Post
        body: text
    action seed(title: text, body: text):
        add Post { title: title, body: body }
    action update(id: int, title: text, body: text):
        set Post(id).title = title
        set Post(id).body = body
    view Home at "/":
        box:
            for p in Post by id:
                text "{p.title}/{p.body}"
`

// Two `set`s on the same row in one action must be one durable write, and the
// caller must be told the truth about whether it happened.
//
// Both halves of this were broken at once, and the pairing is what made it
// dangerous: `fqTx.Save` appended one `insert_node` per call with no dedupe by
// address, so the engine rejected the batch — and `commit` logged that rejection
// and returned nothing, so the action answered 200 {"ok":true} to a write it had
// not performed. The app kept serving the new values out of its in-memory mirror
// until it was restarted, at which point the edit was simply gone.
func TestTwoSetsOnOneRowAreOneDurableWrite(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, twoSetApp)

	if code, body := a.action("seed", "first title", "first body"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}
	if code, body := a.action("update", 1, "second title", "second body"); code != 200 {
		t.Fatalf("a two-set update was rejected: %d %s", code, body)
	}

	// The claim is durability, so the only honest reader is a cold one.
	e.stop()

	revived := startEngineIn(t, e.dir)
	b := startApp(t, revived, twoSetApp)

	code, html := b.get("/")
	if code != 200 {
		t.Fatalf("GET / after restart: %d", code)
	}
	if !strings.Contains(html, "second title/second body") {
		t.Errorf("the two-field update did not survive: the action answered ok and wrote nothing.\npage was: %s", html)
	}
}
