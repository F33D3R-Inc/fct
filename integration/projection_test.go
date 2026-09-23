package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// A parameterized derive — one definition of a response shape — against the
// real engine. Its expansion is the literal it stands for, so the thing to
// prove end to end is the thing that literal already had to get right: the
// per-row counts inside a list resolve against the store, the list's own filter
// still narrows what is read, and a page that renders a projection prints the
// same characters on the server and in the browser.
const projectionApp = `app Projection:
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
    derive likes(id: int): int = count(l in Like where l.post == id)
    action seed():
        let ada = add Account { handle: "ada" }
        let bo = add Account { handle: "bo" }
        add Follow { follower: ada, followee: bo }
        add Follow { follower: bo, followee: ada }
        let p1 = add Post { author: bo, body: "hello" }
        add Post { author: ada, body: "world" }
        add Like { post: p1, account: ada }
        add Like { post: p1, account: bo }
    action feed(me: int, author: int) -> [PostDTO]:
        return list(postDTO(p, me) in Post where p.author == author by id)
    view Home at "/":
        box:
            for p in Post by id:
                text "{p.body} has {likes(p.id)} likes"
`

func TestAProjectionAnswersFromTheStore(t *testing.T) {
	e := startEngine(t)
	a := startApp(t, e, projectionApp)

	if code, body := a.action("seed"); code != 200 {
		t.Fatalf("seed: %d %s", code, body)
	}

	// ada (1) reads bo's (2) posts: one post, liked by both, and they follow
	// each other.
	code, body := a.action("feed", 1, 2)
	if code != 200 {
		t.Fatalf("feed: %d %s", code, body)
	}
	var reply struct {
		Value []struct {
			ID     string `json:"id"`
			Body   string `json:"body"`
			Likes  int    `json:"likes"`
			Liked  bool   `json:"liked"`
			Author struct {
				Handle   string `json:"handle"`
				Posts    int    `json:"posts"`
				IsMutual bool   `json:"is_mutual"`
			} `json:"author"`
		} `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if len(reply.Value) != 1 {
		t.Fatalf("the list's filter should keep only bo's post: %s", body)
	}
	got := reply.Value[0]
	if got.Body != "hello" || got.Likes != 2 || !got.Liked || got.Author.Handle != "bo" || got.Author.Posts != 1 || !got.Author.IsMutual {
		t.Fatalf("feed(1, 2) = %+v", got)
	}

	code, html := a.get("/")
	if code != 200 {
		t.Fatalf("GET /: %d", code)
	}
	// serverText drops whitespace, so the sentences are compared without it.
	for _, want := range []string{"hellohas2likes", "worldhas0likes"} {
		if !strings.Contains(serverText(html), want) {
			t.Errorf("the page should print %q; got:\n%s", want, serverText(html))
		}
	}
	_, clientText := renderClientText(t, html)
	if want := serverText(html); clientText != want {
		serverWindow, clientWindow := firstDifference(want, clientText)
		t.Errorf("the client rendered the projection differently than the server.\nserver: %s\nclient: %s", serverWindow, clientWindow)
	}
}
