package compile

import (
	"strings"
	"testing"

	"facet/internal/ir"
)

// A small social-graph app exercising the Tier-1 additions: a self-referential
// relation (reply threads), a join entity (likes), and filtered aggregates
// (`count(x in E where …)`, `exists(x in E where …)`) — including an `exists`
// inside a `for … where` to build a "people I follow" feed.
const socialApp = `app S:
    auth
    entity Tweet:
        id: int
        author: text
        body: text
        parent: Tweet?
        created: int
    entity Like:
        id: int
        tweet: Tweet
        user: text
    entity Follow:
        id: int
        follower: text
        followee: text
    policy member:
        actor != "guest"
    action like(id: int):
        requires member
        add Like { tweet: id, user: actor }
    view Home at "/":
        box:
            for t in Tweet where exists(f in Follow where f.follower == actor && f.followee == t.author) by created desc limit 50:
                box:
                    text "{t.author}: {t.body}"
                    text "likes {count(l in Like where l.tweet == t.id)}"
                    if exists(l in Like where l.tweet == t.id && l.user == actor):
                        text "you liked this"
`

func TestFilteredAggregatesAndSelfRelation(t *testing.T) {
	graph, err := String(socialApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Self-referential relation: Tweet.parent is a foreign key back to Tweet.
	var parent *ir.Field
	for i := range graph.Entities {
		if graph.Entities[i].Name != "Tweet" {
			continue
		}
		for j := range graph.Entities[i].Fields {
			if graph.Entities[i].Fields[j].Name == "parent" {
				parent = &graph.Entities[i].Fields[j]
			}
		}
	}
	if parent == nil || parent.Ref != "Tweet" {
		t.Fatalf("self-referential relation parent->Tweet not recorded: %+v", parent)
	}

	// A filtered aggregate parses and lowers with its item var + filter predicate.
	ex, err := ir.CompileExpr(graph, "count(l in Like where l.tweet == 1)")
	if err != nil {
		t.Fatalf("compile filtered count: %v", err)
	}
	if ex.Kind != "agg" || ex.Op != "count" || ex.Var != "l" || ex.Where == nil {
		t.Fatalf("filtered count lowered wrong: %+v", ex)
	}
	ex2, err := ir.CompileExpr(graph, "exists(l in Like where l.user == actor)")
	if err != nil {
		t.Fatalf("compile exists: %v", err)
	}
	if ex2.Op != "exists" || ex2.Var != "l" || ex2.Where == nil {
		t.Fatalf("exists lowered wrong: %+v", ex2)
	}

	// A filtered sum (item A) carries the summed field on the item var alongside the
	// filter predicate: sum(t.created in Tweet where t.author == actor).
	ex3, err := ir.CompileExpr(graph, "sum(t.created in Tweet where t.author == actor)")
	if err != nil {
		t.Fatalf("compile filtered sum: %v", err)
	}
	if ex3.Op != "sum" || ex3.Var != "t" || ex3.Field != "created" || ex3.Where == nil {
		t.Fatalf("filtered sum lowered wrong: %+v", ex3)
	}

	// The feed (a list whose `where` reads Follow, whose body reads Like) refreshes
	// when any of those entities change — that is what keeps counts/feeds live.
	for _, ent := range []string{"Tweet", "Follow", "Like"} {
		if len(graph.DepGraph[ent]) == 0 {
			t.Errorf("feed list should depend on %q for live updates, deps=%v", ent, graph.DepGraph)
		}
	}
}

func TestAggregateErrors(t *testing.T) {
	graph, err := String(socialApp)
	if err != nil {
		t.Fatalf("compile base: %v", err)
	}
	cases := []struct{ name, expr, want string }{
		{"exists needs a filter", "exists(Like)", "filtered form"},
		{"filtered sum needs a field", "sum(l in Like where l.user == actor)", "needs a field"},
		{"unknown entity", "count(x in Nope where x.id == 1)", "not an entity"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ir.CompileExpr(graph, c.expr); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
