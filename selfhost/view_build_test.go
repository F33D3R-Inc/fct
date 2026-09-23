package selfhost

// view_build.fct / view_parse.fct conformance: small apps written to reach
// every view node kind, modifier, link-destination form and region shape
// the compiler has — beyond what the example apps happen to use — each
// compiled by the self-hosted driver (compileSrc) and by the real compiler
// (compile.String), whole IR compared structurally. A second table pins
// the view compiler's refusals: a program the real compiler rejects must be
// rejected by the driver too.

import (
	"encoding/json"
	"strings"
	"testing"

	"facet/internal/compile"
)

func viewWant(t *testing.T, src string) map[string]interface{} {
	t.Helper()
	g, err := compile.String(src)
	if err != nil {
		t.Fatalf("real compiler refused the conformance app: %v", err)
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var full map[string]interface{}
	if err := json.Unmarshal(b, &full); err != nil {
		t.Fatal(err)
	}
	return full
}

// viewConformanceApps: v1 reaches every node kind, modifier, control and
// link-destination form (plus meta, route params, a guarded route, @e2e
// seals); v2 reaches the region machinery — nested for/if/match/overlay/
// popover inside a row, list-state and entity ranges with where/by/limit,
// data-driven select/radio, stage tiles and sprites, after timers,
// pending/failed/dirty/touched, aggregates, an on-change debounce.
var viewConformanceApps = []struct{ name, src string }{
	{"v1", `app ViewKinds:
    enum Mode: draft, live, archived
    enum Kind: note, link

    entity Post:
        id: int
        title: text
        body: text
        kind: Kind
        score: int
        author: text
        secretNote: text @e2e

    entity Category:
        id: int
        name: text

    state mode: Mode = Mode.draft @client
    state tab: text = "a" @client
    state pick: text = "" @client
    state catId: int = 0 @client
    state flag: bool = false @client
    state on: bool = false @client
    state pw: text = "" @client
    state note: text = "" @client
    state shown: int = 10 @client
    state count: int = 0
    state q: text = "" @client

    policy admin:
        role == "admin"

    derive doubled: int = count * 2

    action bump:
        count = count + 1

    action loadMore:
        shown = shown + 10

    action save(t: text, b: text, s: text):
        add Post { title: t, body: b, kind: Kind.note, score: 0, author: actor, secretNote: s }

    view Home at "/":
        meta title "Home — {count}"
        heading 1 "Welcome"
        image "/logo.png" alt "the logo"
        video "/clip.mp4" poster "/p.png" alt "a clip" autoplay loop
        icon "star"
        richtext "**bold** {count}"
        box class "wrap" style "gap: 4px" anchor "top":
            text "count {count} doubled {doubled}"
            row class "r-{mode}":
                badge "{mode}"
        tabs bind tab:
            tab "First" -> "a":
                text "first {count}"
            tab "Second" -> "b":
                for p in Post where p.score > 3 && p.author == actor by score desc limit shown more loadMore:
                    text "{p.title}"
        match mode:
            case "draft":
                text "draft"
            case "live":
                text "live"
            case "archived":
                text "gone"
        select bind mode
        select bind pick:
            option "One" -> "one"
            option "Two"
        select bind catId:
            for c in Category by name:
                option "{c.name}" -> c.id
        radio bind pick:
            option "One" -> "one"
            option "Two" -> "two"
        textarea bind note placeholder "write…"
        checkbox bind flag label "Flag it"
        toggle bind on label "On"
        password bind pw placeholder "secret"
        typeahead bind q from Category.name placeholder "cat"
        form "Save" -> save(note, pw, q):
            input bind note
        upload bind note
        if admin:
            button "bump" -> bump
        for p in Post by id desc limit 5:
            match p.kind:
                case "note":
                    text "{p.secretNote}"
                case "link":
                    link "open {p.title}" -> "/post/{p.id}"
            if p.score > count:
                text "hot"
        link "About" -> "/about"
        link "Docs" -> "https://example.com/docs/{count}"
        link "Mail" -> "mailto:hi@example.com"
        link "Top" -> "#top"
        link "Post" -> "/post/{count}#top"

    view About:
        text "about"
        link "home" -> "/"

    view Read at "/post/:id" requires admin:
        meta title "{Post(toInt(id)).title}"
        meta description "Post {id}"
        text "{Post(toInt(id)).title} by {Post(toInt(id)).author}"
        button "bump" -> bump
`},
	{"v2", `app Regions:
    enum Color: red, green

    entity Item:
        id: int
        name: text
        rank: int
        color: Color
        owner: text

    entity Tag:
        id: int
        label: text

    state tags: [text] = ["a", "b"]
    state page: int = 5 @client
    state minRank: int = 0 @client
    state href: text = "/" @client
    state open: bool = false @client
    state menu: bool = false @client
    state draft: text = "" @client
    state picked: int = 0 @client
    state sel: text = "" @client
    state bg: text = "grass" @client
    state token: text = "" @private
    state visible: bool = true @client

    derive threshold: int = minRank + 1

    action rename(id: int, name: text):
        set Item(id).name = name

    action typing(t: text):
        draft = t

    action flip:
        open = !open

    view Main:
        text "{count(i in Item where i.rank > minRank)} ranked, {sum(Item.rank)} total"
        if pending(rename):
            text "saving…"
        text "{failed(rename)}"
        if dirty(draft) || touched(draft):
            text "edited"
        input bind draft placeholder "type" on change -> typing(draft) debounce 2s
        for tag in tags:
            badge "{tag}"
        for i in Item where i.rank >= threshold && i.color == "red" by rank limit page:
            row class "item-{i.color}":
                heading 3 "{i.name}"
                overlay bind open:
                    text "details {i.name}"
                button "open" -> flip
                popover bind menu:
                    text "menu"
                if i.owner == actor:
                    button "rename" -> rename(i.id, draft)
                match i.color:
                    case "red":
                        text "hot"
                    else:
                        text "cool"
        match picked:
            case 1:
                text "one"
            case "2":
                text "two"
            else:
                text "many"
        select bind picked:
            for t in Tag where t.label != "" by label limit 10:
                option "{t.label}" -> t.id
        radio bind sel:
            for t in Tag:
                option "{t.label}" -> t.label
        stage width 200 height 100:
            tiles from bg
            sprite for i in Item where i.rank > 0 by rank at (i.rank * 10, 5) facing i.rank image "dot" label "{i.name}"
            sprite for t in Tag at (1, 2) image t.label
        if visible:
            after 2s:
                visible = false
                open = true
        link "go" -> "{href}"

    view Second:
        text "second {page}"
        link "main" -> "/"
`},
}

func TestViewBuildConformance(t *testing.T) {
	ts := loadDriverApp(t)
	for _, app := range viewConformanceApps {
		app := app
		t.Run(app.name, func(t *testing.T) {
			want := viewWant(t, app.src)
			got, perr := driverGotSrc(t, ts, app.src)
			if perr != "" {
				t.Fatal(perr)
			}
			if d := driverDiff(want, got); d != "" {
				t.Fatal(d)
			}
		})
	}
}

// viewRefusals: programs the real compiler rejects, each with a phrase that
// must appear in BOTH the real compiler's error and the driver's.
var viewRefusals = []struct {
	name, body, phrase string
}{
	{"unknown action", `
    view Main:
        button "x" -> nope`, `references unknown action "nope"`},
	{"arity", `
    action go(n: int):
        count = n
    view Main:
        button "x" -> go`, `action "go" takes 1 argument(s), got 0`},
	{"unserved link", `
    view Main:
        link "x" -> "/missing"`, `no view serves that route`},
	{"unknown collection", `
    view Main:
        for x in Nope:
            text "{x}"`, "`for` over unknown collection \"Nope\""},
	{"authoritative input", `
    view Main:
        input bind count`, `input binds "count", which is authoritative`},
	{"order by unknown field", `
    view Main:
        for p in Post by nope:
            text "{p.body}"`, `entity "Post" has no field "nope" to order by`},
	{"non-exhaustive match", `
    view Main:
        match mode:
            case "a":
                text "a"`, `match on enum "Mode" is not exhaustive: missing b`},
	{"overlay non-bool", `
    view Main:
        overlay bind note:
            text "x"`, `overlay binds "note", which is not a bool`},
	{"row as text", `
    view Main:
        for p in Post:
            text "{p}"`, "is a whole row and cannot be rendered as text"},
	{"unknown node", `
    view Main:
        blink "x"`, `unknown view node`},
	{"popover first", `
    view Main:
        box:
            popover bind flag:
                text "x"`, `popover has no previous sibling to anchor beside`},
	{"javascript link", `
    view Main:
        link "x" -> "javascript:alert(1)"`, `uses the "javascript" scheme`},
	{"missing anchor", `
    view Main:
        link "x" -> "#nowhere"`, `no node declares that anchor`},
	{"duplicate route", `
    view Main at "/":
        text "a"
    view Other at "/":
        text "b"`, `both map to route "/"`},
	{"private rendered", `
    view Main:
        text "{hidden}"`, `"hidden" is @private (server-only) and cannot be rendered`},
	{"secret filtered", `
    view Main:
        for s in Vault where s.code == "x":
            text "{s.id}"`, `field "code" is @secret and cannot be used in a`},
	{"heading level", `
    view Main:
        heading 7 "x"`, `a heading level is between 1 and 6, but this one is 7`},
	{"unknown field read", `
    view Main:
        for p in Post:
            text "{p.bdoy}"`, `entity "Post" has no field "bdoy"`},
	{"private entity", `
    private entity Thing:
        id: int`, "`private` is only supported on proc and view declarations"},
	{"act outside daemon", `
    proc helper() -> int:
        act bump()
        return 1
    action bump:
        count = count + 1
    view Main:
        text "x"`, `act is only valid inside a daemon body`},
	{"daemon capability", `
    daemon D uses io.disk:
        let x = 1
    view Main:
        text "x"`, `declares unknown capability "io.disk"`},
}

const viewRefusalPrelude = `app Refuse:
    enum Mode: a, b
    entity Post:
        id: int
        body: text
    entity Vault:
        id: int
        code: text @secret
    state count: int = 0
    state mode: Mode = Mode.a @client
    state note: text = "" @client
    state flag: bool = false @client
    state hidden: text = "" @private
`

func TestViewBuildRefusals(t *testing.T) {
	ts := loadDriverApp(t)
	for _, c := range viewRefusals {
		c := c
		t.Run(c.name, func(t *testing.T) {
			src := viewRefusalPrelude + strings.TrimPrefix(c.body, "\n") + "\n"
			_, goErr := compile.String(src)
			if goErr == nil {
				t.Fatalf("real compiler accepted the program; the case is wrong")
			}
			if !strings.Contains(goErr.Error(), c.phrase) {
				t.Fatalf("real compiler's error %q lacks %q", goErr, c.phrase)
			}
			d := postExprJSON(t, ts, "runCompileSrc", src)
			j, _ := d["compiledOut"].(string)
			var out map[string]interface{}
			if err := json.Unmarshal([]byte(j), &out); err != nil {
				t.Fatalf("driver output is not JSON: %v", err)
			}
			msg, ok := out["error"].(string)
			if !ok {
				t.Fatalf("driver accepted a program the real compiler refuses (%v)", goErr)
			}
			if !strings.Contains(msg, c.phrase) {
				t.Fatalf("driver's error %q lacks %q (real compiler: %v)", msg, c.phrase, goErr)
			}
		})
	}
}
