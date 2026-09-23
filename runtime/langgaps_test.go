package runtime

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"facet/internal/compile"
)

const gapsApp = `app G:
    type TagDTO:
        tag: text
        n: int
    type PackDTO:
        name: text
        members: [TagDTO]
    entity Pk:
        id: int
        name: text
    entity T:
        id: int
        pk: int
        tag: text
        n: int
    derive pDTO(x: Pk, me: int): PackDTO = PackDTO{name: x.name, members: list(TagDTO{tag: t.tag, n: t.n} in T where t.pk == x.id by id)}
    action seed():
        let p = add Pk { name: "p1" }
        add T { pk: p, tag: "zeta", n: 3 }
        add T { pk: p, tag: "alpha", n: 1 }
        add T { pk: p, tag: "mid", n: 2 }
    action maxTag() -> text:
        return max(t.tag in T)
    action minTag() -> text:
        return min(t.tag in T where t.n > 1)
    action noTag() -> text:
        return max(t.tag in T where t.n > 99)
    action joined() -> [int]:
        let a = list(t.n in T where t.n > 1 by n)
        let b = list(t.n in T where t.n <= 1)
        return a + b
    action packs() -> [PackDTO]:
        return list(pDTO(x, 0) in Pk)
    action tagEach(names: [text]):
        for nm in names:
            add T { pk: 0, tag: nm, n: 7 }
    action countSeven() -> int:
        return count(t in T where t.n == 7)
    view Home at "/":
        text "x"
`

// Text min/max, a projection call naming its row by a bare argument (with a
// nested list inside the projection), list + list, and `for` over a list.
func TestLanguageGaps(t *testing.T) {
	g, err := compile.String(gapsApp)
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
	val := func(name string, args ...any) string {
		t.Helper()
		v, err := srv.RunValue("ada", "member", true, name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	for name, want := range map[string]string{
		"maxTag": `"zeta"`, "minTag": `"mid"`, "noTag": `""`, "joined": `[2,3,1]`,
		"packs": `[{"members":[{"n":3,"tag":"zeta"},{"n":1,"tag":"alpha"},{"n":2,"tag":"mid"}],"name":"p1"}]`,
	} {
		if got := val(name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if _, err := srv.Run("ada", "member", true, "tagEach", []any{[]any{"a", "b", "c"}}); err != nil {
		t.Fatal(err)
	}
	if got := val("countSeven"); got != "3" {
		t.Fatalf("for over a list added %s rows, want 3", got)
	}

	bad := map[string]string{
		"row name is a local": strings.Replace(gapsApp, "        return list(pDTO(x, 0) in Pk)", "        let x = 1\n        return list(pDTO(x, 0) in Pk)", 1),
		"filtered list for":   strings.Replace(gapsApp, "for nm in names:", "for nm in names where nm != \"\":", 1),
	}
	wants := map[string]string{"row name is a local": "is a local", "filtered list for": "walks a list value"}
	for name, src := range bad {
		if _, err := compile.String(src); err == nil || !strings.Contains(err.Error(), wants[name]) {
			t.Errorf("%s: want %q, got %v", name, wants[name], err)
		}
	}
}

// A proc may return a wire type and is callable in an action's expressions —
// per row inside list(...) too — but not in a derive, which has no proc runner.
func TestProcInExpressions(t *testing.T) {
	src := `app P:
    type LabelDTO:
        text: text
        n: int
    entity G:
        id: int
        n: int
    proc label(n: int) -> LabelDTO:
        if n > 1:
            return LabelDTO{text: "many", n: n}
        return LabelDTO{text: "one", n: n}
    proc ord(n: int) -> text:
        if n == 1:
            return "1st"
        return "" + n + "th"
    action seed():
        add G { n: 1 }
        add G { n: 4 }
    action labels() -> [LabelDTO]:
        return list(label(g.n) in G by id)
    action place(n: int) -> text:
        return "finished " + ord(n)
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, a := range g.Actions {
		if a.Name == "place" && a.Placement != "server" {
			t.Fatalf("an action calling a proc must run on the authority, got %q", a.Placement)
		}
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.Run("ada", "member", true, "seed", nil)
	v, err := srv.RunValue("ada", "member", true, "labels", nil)
	raw, _ := json.Marshal(v)
	if err != nil || string(raw) != `[{"n":1,"text":"one"},{"n":4,"text":"many"}]` {
		t.Fatalf("labels = %s %v", raw, err)
	}
	if v, _ := srv.RunValue("ada", "member", true, "place", []any{4}); v != "finished 4th" {
		t.Fatalf("place = %v", v)
	}
	// A derive may call a proc; it is then usable in actions only.
	withDerive := strings.Replace(src, "    view Home", "    derive d(n: int): text = \"#\" + ord(n)\n    action viaDerive(n: int) -> text:\n        return d(n)\n    view Home", 1)
	g2, err := compile.String(withDerive)
	if err != nil {
		t.Fatalf("proc in a derive used by an action: %v", err)
	}
	srv2, _ := NewInMemory(g2)
	defer srv2.Shutdown()
	if v, _ := srv2.RunValue("ada", "member", true, "viaDerive", []any{2}); v != "#2th" {
		t.Fatalf("viaDerive = %v", v)
	}
	inView := strings.Replace(withDerive, "        text \"x\"", "        text \"{d(1)}\"", 1)
	if _, err := compile.String(inView); err == nil || !strings.Contains(err.Error(), "runs only on the authority") {
		t.Fatalf("proc-calling derive in a view: %v", err)
	}
	if _, err := compile.String(strings.Replace(src, "        text \"x\"", "        text \"{ord(1)}\"", 1)); err == nil || !strings.Contains(err.Error(), "only in an action") {
		t.Fatalf("proc in a view: %v", err)
	}
}

// `list(… by <expr>)` sorts by a key computed per row — an aggregate over
// the row, arithmetic, or an entity derive — while a bare column stays a
// column order.
func TestComputedSortKeys(t *testing.T) {
	src := `app S:
    entity Work:
        id: int
        body: text
        created: int
        derive age: int = 1000 - created
    entity Like:
        id: int
        work: int
    action seed():
        let a = add Work { body: "a", created: 10 }
        let b = add Work { body: "b", created: 30 }
        let c = add Work { body: "c", created: 20 }
        add Like { work: c }
        add Like { work: c }
        add Like { work: a }
    action top() -> [text]:
        return list(w.body in Work where w.id > 0 by count(l in Like where l.work == w.id) desc)
    action byArith() -> [text]:
        return list(w.body in Work where w.id > 0 by 0 - w.created)
    action byDerive() -> [text]:
        return list(w.body in Work where w.id > 0 by age)
    action byColumn() -> [text]:
        return list(w.body in Work where w.id > 0 by created desc limit 2)
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.Run("ada", "member", true, "seed", nil)
	for name, want := range map[string]string{
		"top": `["c","a","b"]`, "byArith": `["b","c","a"]`, "byDerive": `["b","c","a"]`, "byColumn": `["b","c"]`,
	} {
		v, err := srv.RunValue("ada", "member", true, name, nil)
		raw, _ := json.Marshal(v)
		if err != nil || string(raw) != want {
			t.Errorf("%s = %s %v, want %s", name, raw, err, want)
		}
	}
	for _, a := range g.Actions {
		if a.Name == "byColumn" && a.Body[0].Value.Order != "created" {
			t.Errorf("a bare column must stay a column order: %+v", a.Body[0].Value)
		}
	}
}

// An optional (`?`) wire field that evaluates to nothing — null, "", or an
// empty list — is absent from the value, as the contract's `required` says;
// a required one keeps its zero value, and 0/false stay present.
func TestOptionalWireFieldsOmitted(t *testing.T) {
	src := `app O:
    type GifDTO:
        id: text
        title: text?
        width: int?
        tags: [text]?
        note: text
    entity Gif:
        id: int
        title: text
        width: int?
    action seed():
        add Gif { title: "", width: 0 }
        add Gif { title: "cat" }
    action gifs() -> [GifDTO]:
        return list(GifDTO{id: "" + g.id, title: g.title, width: g.width, tags: [], note: ""} in Gif where g.id > 0 by id)
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.Run("ada", "member", true, "seed", nil)
	v, err := srv.RunValue("ada", "member", true, "gifs", nil)
	raw, _ := json.Marshal(v)
	if err != nil || string(raw) != `[{"id":"1","note":"","width":0},{"id":"2","note":"","title":"cat"}]` {
		t.Fatalf("gifs = %s %v", raw, err)
	}
}

// iso(0) is the unset instant: "" — which an optional datetime field omits.
func TestIsoUnset(t *testing.T) {
	if iso(0) != "" || iso(1) != "1970-01-01T00:00:01Z" {
		t.Fatalf("iso(0) = %q, iso(1) = %q", iso(0), iso(1))
	}
	src := `app I:
    type D:
        at: datetime
        seen: datetime?
    action d(t: int) -> D:
        return D{at: iso(now()), seen: iso(t)}
    action bad(t: int) -> D:
        return D{at: "" + t}
    view Home at "/":
        text "x"
`
	if _, err := compile.String(src); err == nil || !strings.Contains(err.Error(), "fill it with iso") {
		t.Fatalf("text into a datetime field: %v", err)
	}
	g, err := compile.String(strings.Replace(src, "    action bad(t: int) -> D:\n        return D{at: \"\" + t}\n", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	v, _ := srv.RunValue("ada", "member", true, "d", []any{0})
	if m, _ := v.(record); m == nil || m["at"] == "" || m["seen"] != nil {
		t.Fatalf("d(0) = %v", v)
	}
}

// A DTO field filled with a projection of a different DTO is refused.
func TestWireFieldShapeMismatch(t *testing.T) {
	src := `app W:
    type AuthorDTO:
        handle: text
    type AccountDTO:
        handle: text
    type WorkDTO:
        author: AccountDTO
    entity Account:
        id: int
        handle: text
    derive authorDTO(a: Account): AuthorDTO = AuthorDTO{handle: a.handle}
    action w(id: int) -> WorkDTO:
        return WorkDTO{author: authorDTO(Account(id))}
    view Home at "/":
        text "x"
`
	if _, err := compile.String(src); err == nil || !strings.Contains(err.Error(), "WorkDTO.author is a AccountDTO, but this value is a AuthorDTO") {
		t.Fatalf("got %v", err)
	}
}

// fromIso is iso's inverse; anything that is not RFC 3339 is 0, the unset instant.
func TestFromIso(t *testing.T) {
	if fromIso(iso(1790000000)) != 1790000000 || fromIso("") != 0 || fromIso("tomorrow") != 0 || fromIso("2026-09-23T10:00:00+02:00") != 1790150400 {
		t.Fatalf("fromIso: %d %d", fromIso(iso(1790000000)), fromIso("2026-09-23T10:00:00+02:00"))
	}
}

// first(list) is the list's first element or nothing; an optional field
// given nothing is left out.
func TestFirstBuiltin(t *testing.T) {
	src := `app F:
    type Opt:
        n: int
    type Box:
        opt: Opt?
    entity P:
        id: int
        n: int
    action seed():
        add P { n: 7 }
    action box(id: int) -> Box:
        return Box{opt: first(list(Opt{n: p.n} in P where p.id == id))}
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	srv.Run("ada", "member", true, "seed", nil)
	for id, want := range map[int]string{1: `{"opt":{"n":7}}`, 2: `{}`} {
		v, _ := srv.RunValue("ada", "member", true, "box", []any{id})
		raw, _ := json.Marshal(v)
		if string(raw) != want {
			t.Errorf("box(%d) = %s, want %s", id, raw, want)
		}
	}
}

// fromJson decodes JSON text; "" or malformed text is nothing.
func TestFromJson(t *testing.T) {
	v := callBuiltin("fromJson", []any{`{"x":"https://x.example"}`})
	if m, _ := v.(map[string]any); m["x"] != "https://x.example" {
		t.Fatalf("fromJson object = %v", v)
	}
	if callBuiltin("fromJson", []any{""}) != nil || callBuiltin("fromJson", []any{"{nope"}) != nil {
		t.Fatal("fromJson of non-JSON must be nothing")
	}
}

// DTO field values are held to the field's JSON kind: a number where the
// wire carries text (read off a row or a derive parameter), text where it
// carries a number, and a list where it carries one object are refused.
func TestWireFieldKinds(t *testing.T) {
	base := `app S:
    type Page:
        items: [D]
    type D:
        id: text
        n: int
    type Box:
        page: Page
    entity W:
        id: int
        n: int
        t: text
    derive dd(w: W): D = D{id: "" + w.id, n: w.n}
    action a() -> [D]:
        return list(D{id: "" + w.id, n: w.n} in W)
    action b() -> Box:
        return Box{page: Page{items: []}}
    view Home at "/":
        text "x"
`
	if _, err := compile.String(base); err != nil {
		t.Fatalf("clean app: %v", err)
	}
	cases := map[string]string{
		`D{id: w.id, n: w.n} in W`:      "D.id is a string on the wire, but this value is a number",
		`D{id: "" + w.id, n: w.t} in W`: "D.n is a number on the wire, but this value is a string",
	}
	for to, want := range cases {
		src := strings.Replace(base, `D{id: "" + w.id, n: w.n} in W`, to, 1)
		if _, err := compile.String(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", to, want, err)
		}
	}
	if _, err := compile.String(strings.Replace(base, "Box{page: Page{items: []}}", "Box{page: []}", 1)); err == nil || !strings.Contains(err.Error(), "is one value") {
		t.Errorf("list into an object field: %v", err)
	}
	if _, err := compile.String(strings.Replace(base, `D{id: "" + w.id, n: w.n}`+"\n", `D{id: "" + w.id, n: w.t}`+"\n", 1)); err == nil || !strings.Contains(err.Error(), "is a number on the wire") {
		t.Errorf("derive parameter row field: %v", err)
	}
}

// `T or null`: always present, null when empty; `T?`: left out when empty;
// the contract marks the first required and nullable.
func TestNullableWireFields(t *testing.T) {
	src := `app N:
    type D:
        a: text or null
        b: text?
        c: text or null
    action d(x: text) -> D:
        return D{a: x, b: x}
    api GET "/d" -> d
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	for x, want := range map[string]string{"": `{"a":null,"c":null}`, "hi": `{"a":"hi","b":"hi","c":null}`} {
		v, _ := srv.RunValue("ada", "member", true, "d", []any{x})
		raw, _ := json.Marshal(v)
		if string(raw) != want {
			t.Errorf("d(%q) = %s, want %s", x, raw, want)
		}
	}
	sch, _ := json.Marshal(buildContract(g, 0)["components"].(map[string]any)["schemas"].(map[string]any)["D"])
	if !strings.Contains(string(sch), `"a":{"oneOf":[{"type":"string"},{"type":"null"}]}`) || !strings.Contains(string(sch), `"required":["a","c"]`) {
		t.Fatalf("schema = %s", sch)
	}
	if _, err := compile.String(strings.Replace(src, "c: text or null", "c: text? or null", 1)); err == nil {
		t.Fatal("`T? or null` should be refused")
	}
}

// given(p) tells an absent optional parameter from a sent zero.
func TestGivenParam(t *testing.T) {
	src := `app G:
    type R:
        sent: bool
        value: bool
    action a(flag: bool?) -> R:
        return R{sent: given(flag), value: flag}
    api POST "/a" -> a
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := &apiClient{t: t, base: ts.URL}
	for body, want := range map[string][2]bool{`{}`: {false, false}, `{"flag":false}`: {true, false}, `{"flag":true}`: {true, true}} {
		_, obj, _ := c.do("POST", "/a", body)
		if obj["sent"] != want[0] || obj["value"] != want[1] {
			t.Errorf("%s -> %v", body, obj)
		}
	}
	bad := strings.Replace(src, "given(flag)", "given(nope)", 1)
	if _, err := compile.String(bad); err == nil || !strings.Contains(err.Error(), "not a parameter") {
		t.Fatalf("given of a non-parameter: %v", err)
	}
}

// Indexing reads a list's element or a json value's member outside procs too.
func TestIndexJsonAndLists(t *testing.T) {
	src := `app I:
    type R:
        first: text
        name: text
        missing: text?
    action a(raw: text, xs: [text]) -> R:
        let v = fromJson(raw)
        return R{first: xs[0], name: "" + v["user"]["name"], missing: "" + xs[9]}
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	v, err := srv.RunValue("ada", "member", true, "a", []any{`{"user":{"name":"Ada"}}`, []any{"p", "q"}})
	raw, _ := json.Marshal(v)
	if err != nil || string(raw) != `{"first":"p","name":"Ada"}` {
		t.Fatalf("a = %s %v", raw, err)
	}
	if _, err := compile.String(strings.Replace(src, `xs[0]`, `raw[0]`, 1)); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Fatalf("indexing text: %v", err)
	}
}

// join(list, sep) is split's inverse.
func TestJoinBuiltin(t *testing.T) {
	if callBuiltin("join", []any{[]any{"a", 2, "c"}, ", "}) != "a, 2, c" || callBuiltin("join", []any{[]any{}, "-"}) != "" {
		t.Fatal("join")
	}
	src := "app J:\n    action a(xs: [text]) -> text:\n        return join(xs, \"|\")\n    view Home at \"/\":\n        text \"x\"\n"
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	if v, _ := srv.RunValue("ada", "member", true, "a", []any{[]any{"x", "y"}}); v != "x|y" {
		t.Fatalf("join in an action = %v", v)
	}
}

// A proc calls another proc directly in an expression.
func TestProcCallsProc(t *testing.T) {
	src := `app P:
    proc plural(n: int, unit: text) -> text:
        if n == 1:
            return "1 " + unit
        return "" + n + " " + unit + "s"
    proc left(n: int) -> text:
        return plural(n, "day") + " left"
    action a(n: int) -> text:
        return left(n)
    view Home at "/":
        text "x"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	for n, want := range map[int]string{1: "1 day left", 3: "3 days left"} {
		if v, err := srv.RunValue("ada", "member", true, "a", []any{n}); v != want {
			t.Errorf("a(%d) = %v %v", n, v, err)
		}
	}
}

// A check message may interpolate the action's values, read when it fails.
func TestCheckMessageInterpolates(t *testing.T) {
	src := `app C:
    state count: int = 0
    action bump(n: int, who: text):
        check n > 0 "{who}: n must be positive, got {n}" status 400
        count = count + n
    view Home at "/":
        text "{count}"
`
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := NewInMemory(g)
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "bump", []any{-2, "ada"}); err == nil || err.Error() != "ada: n must be positive, got -2" {
		t.Fatalf("message = %v", err)
	}
}

// Wall-clock time in an IANA zone, from the embedded zone database.
func TestZones(t *testing.T) {
	// 10:00 in Paris on a summer day is 08:00 UTC; in New York in winter, 15:00.
	if got := fromLocal("2026-07-01T10:00", "Europe/Paris"); got != 1782892800 {
		t.Fatalf("Paris = %d", got)
	}
	if fromLocal("2026-01-15 10:00", "America/New_York") != fromLocal("2026-01-15T15:00", "UTC") {
		t.Fatal("New York winter offset")
	}
	if fromLocal("2026-07-01T10:00", "Mars/Olympus") != 0 || fromLocal("tomorrow", "UTC") != 0 {
		t.Fatal("bad zone or time must read as unset")
	}
	if got := formatIn(1790000000, "America/New_York", "Mon 3:04 PM MST"); got != "Mon 10:13 AM EDT" {
		t.Fatalf("formatIn = %q", got)
	}
	if _, ok := zone("Asia/Kolkata"); !ok {
		t.Fatal("embedded zone missing")
	}
}
