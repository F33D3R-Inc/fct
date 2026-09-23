package compile

import (
	"encoding/json"
	"strings"
	"testing"

	"facet/internal/ir"
)

// A parameterized derive is a projection: one definition of a response shape,
// called wherever an action answers with it. It is a compile-time abstraction
// like every derive — each call site is the body with the arguments
// substituted — so what matters is that the expansion is exactly the
// expression the author would otherwise have written out by hand.

const projectionEntities = `    entity Account:
        id: int
        handle: text
    entity Post:
        id: int
        author: int
        body: text
    entity Like:
        id: int
        post: int
        account: int
    type AuthorDTO:
        handle: text
        posts: int
    type PostDTO:
        id: text
        body: text
        author: AuthorDTO
        likes: int
        liked: bool
`

// The projected app declares postDTO before the authorDTO it is built on, one
// field per line: declaration order and layout are both the author's business.
const projectedApp = `app P:
` + projectionEntities + `    derive postDTO(p: Post, me: int): PostDTO = PostDTO{
        id: "" + p.id,
        body: p.body,
        author: authorDTO(Account(p.author)),
        likes: count(l in Like where l.post == p.id),
        liked: exists(l in Like where l.post == p.id && l.account == me)
        }
    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.handle, posts: count(x in Post where x.author == a.id)}
    action feed(me: int) -> [PostDTO]:
        return list(postDTO(p, me) in Post where p.author > 0 by id desc limit 20)
    action one(id: int, me: int) -> PostDTO:
        return postDTO(Post(id), me)
    view Home at "/":
        text "x"
`

const handWrittenApp = `app P:
` + projectionEntities + `    action feed(me: int) -> [PostDTO]:
        return list(PostDTO{id: "" + p.id, body: p.body, author: AuthorDTO{handle: Account(p.author).handle, posts: count(x in Post where x.author == Account(p.author).id)}, likes: count(l in Like where l.post == p.id), liked: exists(l in Like where l.post == p.id && l.account == me)} in Post where p.author > 0 by id desc limit 20)
    action one(id: int, me: int) -> PostDTO:
        return PostDTO{id: "" + Post(id).id, body: Post(id).body, author: AuthorDTO{handle: Account(Post(id).author).handle, posts: count(x in Post where x.author == Account(Post(id).author).id)}, likes: count(l in Like where l.post == Post(id).id), liked: exists(l in Like where l.post == Post(id).id && l.account == me)}
    view Home at "/":
        text "x"
`

func actionBodyJSON(t *testing.T, g *ir.IR, name string) string {
	t.Helper()
	for _, a := range g.Actions {
		if a.Name == name {
			b, err := json.Marshal(a.Body)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("no action %q", name)
	return ""
}

// Every call site — per row inside a list's value, and on a row looked up by id
// — lowers to the IR of the literal written out by hand: the surrounding
// `list(… where … by … limit …)` keeps its own filter (so it pushes down
// exactly as before), and a row parameter handed `Post(id)` reads each field
// straight off the lookup.
func TestProjectionExpandsToTheHandWrittenExpression(t *testing.T) {
	proj, err := String(projectedApp)
	if err != nil {
		t.Fatalf("compile projected: %v", err)
	}
	hand, err := String(handWrittenApp)
	if err != nil {
		t.Fatalf("compile hand-written: %v", err)
	}
	for _, name := range []string{"feed", "one"} {
		if got, want := actionBodyJSON(t, proj, name), actionBodyJSON(t, hand, name); got != want {
			t.Errorf("%s: the projection's expansion differs from the hand-written literal\n got: %s\nwant: %s", name, got, want)
		}
	}
	// The definitions themselves are reported, dependency first, with their
	// parameters.
	if len(proj.Derives) != 2 || proj.Derives[0].Name != "authorDTO" || proj.Derives[1].Name != "postDTO" {
		t.Fatalf("derives should be ordered authorDTO, postDTO: %+v", proj.Derives)
	}
	if ps := proj.Derives[1].Params; len(ps) != 2 || ps[0].Name != "p" || ps[0].Type != "Post" || ps[1].Name != "me" {
		t.Fatalf("postDTO's params = %+v", ps)
	}
}

// An argument is caller code. When the body's own aggregate binds the very name
// the argument reads, the aggregate is renamed rather than capturing it: here
// the caller's row is `x`, and authorDTO counts `x in Post` — handed
// `Account(x.author)`, the count must still compare against the outer row.
func TestProjectionSubstitutionIsHygienic(t *testing.T) {
	g, err := String(`app P:
` + projectionEntities + `    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.handle, posts: count(x in Post where x.author == a.id)}
    action authors() -> [AuthorDTO]:
        return list(authorDTO(Account(x.author)) in Post where x.id > 0)
    view Home at "/":
        text "x"
`)
	if err != nil {
		t.Fatal(err)
	}
	body := actionBodyJSON(t, g, "authors")
	// The inner count binds a fresh name, and its predicate reads the outer x.
	if !strings.Contains(body, `"var":"x2"`) {
		t.Fatalf("the inner aggregate's item variable should be renamed away from the argument's x: %s", body)
	}
	if !strings.Contains(body, `"r":{"kind":"eget","name":"Account","field":"id","key":{"kind":"get","field":"author","obj":{"kind":"ref","name":"x"}}}`) {
		t.Fatalf("the inner predicate should compare against the caller's row x: %s", body)
	}
}

// A projection is checked where it is defined and where it is called, with the
// same diagnostics a component's parameters get.
func TestProjectionGates(t *testing.T) {
	cases := []struct{ name, decls, want string }{
		{"arity", `    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.handle, posts: 0}
    action f() -> AuthorDTO:
        return authorDTO(Account(1), 2)
`, `derive "authorDTO" takes 1 argument(s), got 2`},
		{"an id where a row is declared", `    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.handle, posts: 0}
    action f() -> [AuthorDTO]:
        return list(authorDTO(p.author) in Post where p.id > 0)
`, `parameter "a" is a Account row, but argument 1`},
		{"text where an int is declared", `    derive likes(id: int): int = count(l in Like where l.post == id)
    action f() -> int:
        return likes("seven")
`, `parameter "id" is int, but argument 1 is text`},
		{"body of another type", `    derive authorDTO(a: Account): PostDTO = AuthorDTO{handle: a.handle, posts: 0}
    view Home2 at "/2":
        text "x"
`, `derive "authorDTO" is declared PostDTO, but its definition is AuthorDTO`},
		{"no such field on a row parameter", `    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.hnadle, posts: 0}
`, `entity "Account" has no field "hnadle"`},
		{"defined in terms of itself", `    derive a1(n: int): int = a2(n) + 1
    derive a2(n: int): int = a1(n) - 1
`, `is defined in terms of itself`},
		{"used without its arguments", `    derive likes(id: int): int = count(l in Like where l.post == id)
    action f() -> int:
        return likes
`, `derive "likes" takes 1 argument(s) — call it`},
		{"an unknown function", `    action f() -> int:
        return lieks(1)
`, `unknown function "lieks"`},
		{"a builtin's name", `    derive upper(t: text): text = t
`, `collides with the builtin upper(...)`},
		{"a proc calling one", `    derive likes(id: int): int = count(l in Like where l.post == id)
    proc f(id: int) -> int:
        return likes(id)
`, `a proc cannot call derive "likes"`},
		{"an effectful body", `    derive stamp(id: int): int = now() + id
`, `cannot use an effectful builtin`},
		{"an unknown return type", `    derive nope(id: int): Nope = id
`, `returns unknown type "Nope"`},
		{"a parameter shadowing a derive", `    derive total: int = count(Like)
    derive likes(total: int): int = total
`, `parameter "total" shadows the policy or derive`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String("app G:\n" + projectionEntities + c.decls + "    view Home at \"/\":\n        text \"x\"\n")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// The grammar's own refusals: an entity-carried derive computes from its row
// and takes no parameters, `()` is not a parameter list, and a definition
// continued onto indented lines has to close.
func TestProjectionSyntaxGates(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"entity derive with parameters", `app G:
    entity Item:
        id: int
        n: int
        derive scaled(k: int): int = n * k
    view Home at "/":
        text "x"
`, "takes no parameters"},
		{"empty parameter list", `app G:
    derive zero(): int = 0
    view Home at "/":
        text "x"
`, "declared without parentheses"},
		{"unclosed definition", `app G:
    type T:
        a: int
    derive t(n: int): T = T{
        a: n
    view Home at "/":
        text "x"
`, "missing its closing bracket"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := String(c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// A projection used in a view is an ordinary view expression: the page depends
// on every entity the expanded body reads, including those only counted inside
// the per-row value of a list.
func TestProjectionInAViewCarriesItsDependencies(t *testing.T) {
	g, err := String(`app P:
` + projectionEntities + `    derive likes(id: int): int = count(l in Like where l.post == id)
    view Home at "/":
        text "{len(list(PostDTO{id: "", body: "", author: AuthorDTO{handle: "", posts: 0}, likes: likes(p.id), liked: false} in Post where p.id > 0))}"
`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(g.Pages)
	if !strings.Contains(string(b), `"Like"`) {
		t.Fatalf("the page should refresh on Like, which only the list's per-row value reads: %s", b)
	}
}
