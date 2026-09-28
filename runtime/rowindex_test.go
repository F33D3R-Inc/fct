package runtime

import (
	"fmt"
	"testing"

	"facet/internal/compile"
)

const rowIndexApp = `app RI:
    entity Work:
        id: int
        author: int
        quoted: int
        tag: text
        score: int
    entity Like:
        id: int
        work: int
        account: int
    derive dto(w: Work, me: int): text = "" + w.id + ":" + count(q in Work where q.quoted == w.id) + "/" + count(l in Like where l.work == w.id) + "/" + exists(l in Like where l.work == w.id && l.account == me) + "/" + count(r in Work where r.author == w.author && r.score > 2)
    action report(x: int, t: text, me: int) -> text:
        return "" + count(q in Work where q.quoted == x) + "|" + exists(q in Work where q.quoted == x && q.score > 2) + "|" + count(q in Work where q.tag == t) + "|" + count(q in Work where x == q.author) + "|" + Work(x).score + "|" + join(list(q.id in Work where q.author == x by id desc limit 5), ",") + "|" + join(list(dto(w, me) in Work where w.author == x by id limit 7), ";") + "|" + count(q in Work where q.quoted == toInt("" + x))
    action probes(x: int) -> text:
        return "" + count(q in Work where q.author == Work(x).author) + "|" + count(q in Work where Work(x + 1).quoted == q.quoted) + "|" + count(q in Work where q.id in [x, x + 3, 7, 7, 1000]) + "|" + count(q in Work where q.quoted in [x, "3", 5]) + "|" + count(q in Work where q.quoted in [true]) + "|" + join(list(q.id in Work where q.tag in ["t1", "t" + x % 9] && q.score > 1 by id limit 6), ",")
    derive heavy(w: Work, x: int): bool = w.score != 3 && !exists(l in Like where l.work == w.id && l.account == x)
    action lists(x: int) -> text:
        return join(list(w.id in Work where heavy(w, x) by id desc limit 9), ",") + "|" + join(list(w.id in Work where w.author == 1 + x % 17 by score desc limit 6), ",") + "|" + join(list(w.id in Work where heavy(w, x) by tag limit 8), ",") + "|" + join(list(w.id in Work where heavy(w, x) by quoted limit 8), ",") + "|" + join(list(w.id in Work where w.score > x % 5 limit 4), ",") + "|" + exists(w in Work where heavy(w, x) && w.author == x) + "|" + join(list(w.id in Work where heavy(w, x) by id limit 0), ",") + "|" + join(list(w.id in Work where heavy(w, x) by id desc limit 1000), ",")
    action mutate(x: int) -> text:
        let a = count(q in Work where q.quoted == x)
        add Work { author: 1, quoted: x, tag: "new", score: 9 }
        let b = count(q in Work where q.quoted == x)
        set q in Work where q.id == 3:
            quoted = x
        let c = count(q in Work where q.quoted == x)
        set q in Work where q.quoted == x && q.score == 9:
            score = count(r in Work where r.quoted == x)
        let d = count(q in Work where q.quoted == x && q.score > 9)
        remove q in Work where q.id == 4
        let e = count(q in Work where q.quoted == x) + count(q in Work where q.id == 4)
        return "" + a + "," + b + "," + c + "," + d + "," + e
    view Home at "/":
        text "ri"
`

// rowIndexServer is the app with 400 works and 300 likes, their fields
// holding every kind of value an int field's equality can meet: ints, nil,
// text that reads as a number, a float, a bool.
func rowIndexServer(t *testing.T) *Server {
	t.Helper()
	g, err := compile.String(rowIndexApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	var works, likes []any
	for i := 1; i <= 400; i++ {
		var quoted any = i % 37
		switch i % 11 {
		case 0:
			quoted = nil
		case 1:
			quoted = fmt.Sprint(i % 37) // text that reads as the number
		case 2:
			quoted = float64(i % 37)
		case 3:
			quoted = i%2 == 0
		}
		var tag any = fmt.Sprintf("t%d", i%9)
		if i%13 == 0 {
			tag = i % 9 // a number where text is declared
		}
		works = append(works, record{"id": i, "author": 1 + i%17, "quoted": quoted, "tag": tag, "score": i % 5})
	}
	for i := 1; i <= 300; i++ {
		likes = append(likes, record{"id": i, "work": 1 + i%97, "account": 1 + i%7})
	}
	srv.mu.Lock()
	srv.entities["Work"], srv.entities["Like"] = works, likes
	srv.nextID["Work"], srv.nextID["Like"] = 400, 300
	srv.mu.Unlock()
	return srv
}

// With and without row indexes (and the early stops of a capped ordered
// list and an exists), every aggregate, lookup and list answers the same — over mixed-type values, text probes, probes on either side of
// the ==, a DTO derive's per-row counts, and across writes an action makes
// between its reads (an add, an in-place set, a set whose own values are
// counts over the rows it is editing, a remove).
func TestRowIndexesAnswerAsTheScan(t *testing.T) {
	run := func(indexing bool) []string {
		rowIndexing = indexing
		defer func() { rowIndexing = true }()
		srv := rowIndexServer(t)
		var out []string
		for x := 0; x <= 40; x++ {
			for _, tg := range []string{"t3", "3", "", "t8"} {
				v, err := srv.RunValue("ada", "member", true, "report", []any{x, tg, x % 7})
				if err != nil {
					t.Fatalf("report(%d, %q): %v", x, tg, err)
				}
				out = append(out, fmt.Sprint(v))
			}
		}
		for x := 0; x <= 40; x++ {
			v, err := srv.RunValue("ada", "member", true, "probes", []any{x})
			if err != nil {
				t.Fatalf("probes(%d): %v", x, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		for x := 0; x <= 20; x++ {
			v, err := srv.RunValue("ada", "member", true, "lists", []any{x})
			if err != nil {
				t.Fatalf("lists(%d): %v", x, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		for _, x := range []int{0, 1, 5, 36, 99} {
			v, err := srv.RunValue("ada", "member", true, "mutate", []any{x})
			if err != nil {
				t.Fatalf("mutate(%d): %v", x, err)
			}
			out = append(out, fmt.Sprint(v))
		}
		return out
	}
	scan := run(false)
	hits := rowIndexHits.Load()
	indexed := run(true)
	if used := rowIndexHits.Load() - hits; used < 1000 {
		t.Fatalf("the indexed run answered only %d reads from an index", used)
	}
	if len(scan) != len(indexed) {
		t.Fatalf("%d answers vs %d", len(scan), len(indexed))
	}
	for i := range scan {
		if scan[i] != indexed[i] {
			t.Fatalf("answer %d differs:\n scan:    %s\n indexed: %s", i, scan[i], indexed[i])
		}
	}
	t.Logf("%d answers identical; e.g. %s / %s", len(scan), scan[5], scan[len(scan)-1])
}
