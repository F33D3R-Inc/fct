package runtime

import (
	"testing"

	"facet/internal/compile"
)

// A parameterized derive answers with the same values the literal it stands for
// would: per row inside a list, on a row looked up by id, and nested inside
// another projection — with the viewer-relative fields computed for the viewer
// passed in, and the counts over the rows that exist.
const projectionApp = `app P:
    entity Account:
        id: int
        handle: text
    entity Post:
        id: int
        author: int
        body: text
    entity Follow:
        id: int
        follower: int
        followee: int
    entity Like:
        id: int
        post: int
        account: int
    type AuthorDTO:
        handle: text
        posts: int
        is_mutual: bool
    type PostDTO:
        id: text
        body: text
        author: AuthorDTO
        likes: int
        liked: bool
    derive authorDTO(a: Account, me: int): AuthorDTO = AuthorDTO{
        handle: a.handle,
        posts: count(p in Post where p.author == a.id),
        is_mutual: exists(f in Follow where f.follower == me && f.followee == a.id) && exists(f in Follow where f.follower == a.id && f.followee == me)
        }
    derive postDTO(p: Post, me: int): PostDTO = PostDTO{
        id: "" + p.id,
        body: p.body,
        author: authorDTO(Account(p.author), me),
        likes: count(l in Like where l.post == p.id),
        liked: exists(l in Like where l.post == p.id && l.account == me)
        }
    action seed():
        let ada = add Account { handle: "ada" }
        let bo = add Account { handle: "bo" }
        let cy = add Account { handle: "cy" }
        add Follow { follower: ada, followee: bo }
        add Follow { follower: bo, followee: ada }
        add Follow { follower: ada, followee: cy }
        let p1 = add Post { author: bo, body: "hello" }
        add Post { author: cy, body: "world" }
        add Post { author: bo, body: "again" }
        add Like { post: p1, account: ada }
        add Like { post: p1, account: cy }
    action feed(me: int) -> [PostDTO]:
        return list(postDTO(p, me) in Post where p.id > 0 by id)
    action one(id: int, me: int) -> PostDTO:
        return postDTO(Post(id), me)
    view Home at "/":
        for p in Post by id:
            text "{postDTO(p, 1).author.handle}:{postDTO(p, 1).likes}"
`

func TestProjectionValues(t *testing.T) {
	g, err := compile.String(projectionApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "seed", nil); err != nil {
		t.Fatal(err)
	}

	v, err := srv.RunValue("ada", "member", true, "feed", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := v.([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("feed(1) should answer three posts, got %#v", v)
	}
	type want struct {
		body, author  string
		likes, posts  int
		liked, mutual bool
	}
	for i, w := range []want{
		{"hello", "bo", 2, 2, true, true},   // ada and bo follow each other
		{"world", "cy", 0, 1, false, false}, // ada follows cy, cy does not follow back
		{"again", "bo", 0, 2, false, true},
	} {
		r := rows[i].(record)
		a := r["author"].(record)
		if r["body"] != w.body || a["handle"] != w.author || toInt(r["likes"]) != w.likes ||
			toInt(a["posts"]) != w.posts || r["liked"] != w.liked || a["is_mutual"] != w.mutual {
			t.Errorf("feed(1)[%d] = %#v (author %#v), want %+v", i, r, a, w)
		}
	}

	// On a row looked up by id, for another viewer: cy liked p1 but is not
	// mutual with bo.
	v, err = srv.RunValue("ada", "member", true, "one", []any{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(record)
	if r["id"] != "1" || r["liked"] != true || r["author"].(record)["is_mutual"] != false {
		t.Fatalf("one(1, 3) = %#v", r)
	}
}
