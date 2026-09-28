package compile

import (
	"strings"
	"testing"
)

// A fragment written against `s: Stream`, `pay(...)`, `draft` and
// `/profile/{h}` compiles on its own once it states what it expects: the
// expectations stand in for the host's declarations.
func TestExpectStandsInStandalone(t *testing.T) {
	g, err := String(`app Frag:
    expect entity Stream:
        id: int
        title: text
        live: bool
    expect action pay(stream: int, cents: money)
    expect state draft: text
    expect view at "/profile/:handle"
    component StreamCard(s: Stream):
        box:
            text "{s.title}"
            if s.live:
                text "LIVE"
            input bind draft
            button "Tip" -> pay(s.id, 500)
            link "Profile" -> "/profile/{s.title}"
    view Home at "/":
        for s in Stream where s.live:
            use StreamCard(s)
`)
	if err != nil {
		t.Fatalf("a fragment with its expectations must compile standalone: %v", err)
	}
	var ent, act bool
	for _, e := range g.Entities {
		if e.Name == "Stream" && len(e.Fields) == 3 {
			ent = true
		}
	}
	for _, a := range g.Actions {
		if a.Name == "pay" && len(a.Params) == 2 {
			act = true
		}
	}
	if !ent || !act {
		t.Errorf("the stand-ins must be in the graph: entity=%v action=%v", ent, act)
	}
}

const expectHostMain = `import "card.fct"

app Host:
    entity Stream:
        id: int
        owner: text
        title: text
        live: bool
        viewers: int
    state draft: text = "" @client
    action pay(stream: int, cents: money):
        set Stream(stream).viewers = Stream(stream).viewers + 1
    view Home at "/":
        for s in Stream:
            use StreamCard(s)
    view Profile at "/profile/:handle":
        text "{handle}"
`

const expectCard = `app Host:
    expect entity Stream:
        id: int
        title: text
        live: bool
    expect action pay(stream: int, cents: money)
    expect state draft: text
    expect view at "/profile/:handle"
    component StreamCard(s: Stream):
        box:
            text "{s.title}"
            input bind draft
            button "Tip" -> pay(s.id, 500)
            link "Profile" -> "/profile/{s.title}"
`

// A host whose declarations fit discharges every expectation structurally:
// its Stream has more fields than the card reads, and that is fine.
func TestExpectSatisfiedByHost(t *testing.T) {
	g, err := File(writeModules(t, map[string]string{"main.fct": expectHostMain, "card.fct": expectCard}))
	if err != nil {
		t.Fatalf("host + fragment must compile: %v", err)
	}
	for _, e := range g.Entities {
		if e.Name == "Stream" && len(e.Fields) != 5 {
			t.Errorf("the host's Stream must be the one compiled (5 fields), got %d", len(e.Fields))
		}
	}
	n := 0
	for _, a := range g.Actions {
		if a.Name == "pay" {
			n++
			if len(a.Body) == 0 {
				t.Errorf("the host's pay (with a body) must be the one compiled")
			}
		}
	}
	if n != 1 {
		t.Errorf("pay declared %d times, want 1", n)
	}
}

// A host that contradicts an expectation is refused, naming the fragment,
// the field, and the mismatch — a component over a row it cannot read is not
// something to compile on into.
func TestExpectContradictedByHost(t *testing.T) {
	cases := []struct{ name, host, want string }{
		{"missing field",
			strings.Replace(expectHostMain, "        live: bool\n", "", 1),
			`card.fct:2 expects entity Stream to have a field ` + "`live: bool`"},
		{"field type",
			strings.Replace(expectHostMain, "        live: bool\n", "        live: text\n", 1),
			`expects entity Stream's field "live" to be bool, but this app's entity Stream (line 4) declares it as text`},
		{"action signature",
			strings.Replace(expectHostMain, "action pay(stream: int, cents: money):", "action pay(stream: int):", 1),
			`expects action pay(stream: int, cents: money), but this app's action (line 11) is pay(stream: int)`},
		{"state type",
			strings.Replace(expectHostMain, `state draft: text = "" @client`, `state draft: int = 0 @client`, 1),
			`expects state draft: text, but this app's state draft (line 10) is int`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := File(writeModules(t, map[string]string{"main.fct": c.host, "card.fct": expectCard}))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v\nwant it to contain %q", err, c.want)
			}
		})
	}
}

// Two fragments may expect the same entity with different field subsets —
// the stand-in is their union — but not the same field with two types.
func TestExpectUnionAcrossModules(t *testing.T) {
	a := `app Lib:
    expect entity Stream:
        id: int
        title: text
    component A(s: Stream):
        text "{s.title}"
`
	b := `app Lib:
    expect entity Stream:
        id: int
        live: bool
    component B(s: Stream):
        if s.live:
            text "live"
`
	main := "import \"a.fct\"\nimport \"b.fct\"\n\napp Lib:\n    view Home at \"/\":\n        for s in Stream:\n            use A(s)\n            use B(s)\n"
	g, err := File(writeModules(t, map[string]string{"main.fct": main, "a.fct": a, "b.fct": b}))
	if err != nil {
		t.Fatalf("two fragments' shapes must union: %v", err)
	}
	for _, e := range g.Entities {
		if e.Name == "Stream" && len(e.Fields) != 3 {
			t.Errorf("Stream stand-in = %d fields, want the union of 3", len(e.Fields))
		}
	}
	b2 := strings.Replace(b, "        live: bool\n", "        title: bool\n", 1)
	b2 = strings.Replace(b2, "if s.live:", "if s.title:", 1)
	_, err = File(writeModules(t, map[string]string{"main.fct": main, "a.fct": a, "b.fct": b2}))
	if err == nil || !strings.Contains(err.Error(), `field "title" to be bool, but another module expects it to be text`) {
		t.Fatalf("conflicting shapes must be refused, got %v", err)
	}
}

const emitterModule = `app Host:
    type NotificationDTO:
        text: text
    expect event notification: NotificationDTO
    entity Note:
        id: int
        body: text
    action notify(body: text):
        add Note { body: body }
        emit notification NotificationDTO{text: body} to actor
`

// A module that emits into a stream its host declares states the event it
// needs carried; the host's stream discharges it, a differing payload is
// refused, and standalone the emit compiles against the expectation.
func TestExpectEventAgainstHostStream(t *testing.T) {
	if _, err := String(emitterModule); err != nil {
		t.Fatalf("an emitter with its expectation must compile standalone: %v", err)
	}
	host := `import "emitter.fct"

app Host:
    policy member:
        actor != "guest"
    stream "/api/events" requires member:
        notification: NotificationDTO "A note arrived."
    view Home at "/":
        text "hi"
`
	if _, err := File(writeModules(t, map[string]string{"main.fct": host, "emitter.fct": emitterModule})); err != nil {
		t.Fatalf("a host stream carrying the event must discharge it: %v", err)
	}
	bad := strings.Replace(host, "notification: NotificationDTO", "notification: OtherDTO", 1)
	bad = strings.Replace(bad, "    policy member:", "    type OtherDTO:\n        n: int\n    policy member:", 1)
	_, err := File(writeModules(t, map[string]string{"main.fct": bad, "emitter.fct": emitterModule}))
	if err == nil || !strings.Contains(err.Error(), `emitter.fct:4 expects a stream to carry event notification as NotificationDTO, but this app's stream carries "notification" as OtherDTO`) {
		t.Fatalf("a differing payload must be refused, got %v", err)
	}
}

// A fragment whose actions `requires member` states the guard as an
// expectation: the host's policy of that name discharges it (matching
// signature), a differing signature is refused, and standalone the stand-in
// admits everyone so the fragment's actions run as declared.
func TestExpectPolicy(t *testing.T) {
	frag := `app Host:
    expect policy member
    expect policy owns(id: int)
    entity Note:
        id: int
        body: text
    action write(body: text):
        requires member
        add Note { body: body }
    action wipe(id: int):
        requires owns(id)
        remove Note(id)
`
	g, err := String(frag)
	if err != nil {
		t.Fatalf("standalone: %v", err)
	}
	n := 0
	for _, p := range g.Policies {
		if p.Name == "member" || p.Name == "owns" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("stand-in policies = %d, want 2", n)
	}
	host := `import "frag.fct"

app Host:
    policy member:
        actor != "guest"
    policy owns(id: int):
        true
    view Home at "/":
        text "hi"
`
	if _, err := File(writeModules(t, map[string]string{"main.fct": host, "frag.fct": frag})); err != nil {
		t.Fatalf("host policies must discharge the expectations: %v", err)
	}
	bad := strings.Replace(host, "policy owns(id: int):", "policy owns(id: text):", 1)
	_, err = File(writeModules(t, map[string]string{"main.fct": bad, "frag.fct": frag}))
	if err == nil || !strings.Contains(err.Error(), `frag.fct:3 expects policy owns(id: int), but this app's policy (line 6) is owns(id: text)`) {
		t.Fatalf("a differing signature must be refused, got %v", err)
	}
}

// An expected action stands for the host's server action: a fragment may
// `run` it standalone (the stand-in is placed on the authority), and a host
// whose action differs in @internal from the expectation is refused.
func TestExpectActionRunnableAndInternal(t *testing.T) {
	frag := `app Host:
    expect action recordReport(user: text) @internal
    entity R:
        id: int
        who: text
    action report(who: text):
        add R { who: who }
        run recordReport(who)
`
	if _, err := String(frag); err != nil {
		t.Fatalf("a fragment must run its expected action standalone: %v", err)
	}
	host := func(mod string) string {
		return "import \"frag.fct\"\n\napp Host:\n    entity Standing:\n        id: int\n        who: text\n    action recordReport(user: text)" + mod + ":\n        add Standing { who: user }\n    view Home at \"/\":\n        text \"hi\"\n"
	}
	if _, err := File(writeModules(t, map[string]string{"main.fct": host(" @internal"), "frag.fct": frag})); err != nil {
		t.Fatalf("a matching @internal host action discharges it: %v", err)
	}
	_, err := File(writeModules(t, map[string]string{"main.fct": host(""), "frag.fct": frag}))
	if err == nil || !strings.Contains(err.Error(), "to be @internal, but this app's action (line 7) is client-callable") {
		t.Fatalf("a client-callable host action must be refused, got %v", err)
	}
}

// A host action whose parameters extend the expected ones with optional
// parameters only — and whose reply the fragment does not read — serves
// the fragment's calls; a required extra parameter, a renamed or retyped
// one, or an optional one the host makes required does not.
func TestExpectActionOptionalTail(t *testing.T) {
	frag := "app Frag:\n    expect action post(body: text)\n    state draft: text = \"\" @client\n    component Box():\n        button \"Post\" -> post(draft)\n"
	host := func(sig string) string {
		return "import \"frag.fct\"\napp Host:\n    type Out:\n        id: int\n    action " + sig + ":\n        return Out{id: 1}\n    view V at \"/\":\n        use Box()\n"
	}
	if _, err := File(writeModules(t, map[string]string{"main.fct": host("post(body: text, is_nsfw: bool?, tags: [text]?) -> Out"), "frag.fct": frag})); err != nil {
		t.Fatalf("an optional tail must fit: %v", err)
	}
	for _, sig := range []string{"post(body: text, is_nsfw: bool) -> Out", "post(text: text) -> Out", "post(body: int) -> Out"} {
		if _, err := File(writeModules(t, map[string]string{"main.fct": host(sig), "frag.fct": frag})); err == nil || !strings.Contains(err.Error(), "expects action post(body: text)") {
			t.Errorf("%s: err = %v, want a refusal", sig, err)
		}
	}
	optFrag := strings.Replace(frag, "post(body: text)", "post(body: text?)", 1)
	if _, err := File(writeModules(t, map[string]string{"main.fct": host("post(body: text) -> Out"), "frag.fct": optFrag})); err == nil {
		t.Error("a host requiring what the fragment may omit must not fit")
	}
}
