package runtime

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A dispatching route (`api POST "/events" -> Mutation`) decodes its body —
// JSON or a form — as one variant of a tagged union whose tag field is named
// by the message (`tag "event_type"`) and whose wire names may differ from
// the variant names (`as "frequency.upvote"`). The variant's fields, or their
// aliases (`also id`), bind its action's parameters; the action's own gate
// applies; its reply answers (204 when it has none). A variant may take the
// whole body as one documented object (`body envelope: Envelope`). The
// contract publishes the route as one object body and every variant, with
// its fields' descriptions and closed values, its body, and its reply, in
// x-mutation-events.
const dispatchApp = `app D:
    type RoomDTO:
        id: text
        votes: int
    type Envelope:
        cid: text
        note: text
    entity Room:
        id: int
        votes: int
    entity Seen:
        id: int
        note: text
    policy member:
        actor != "guest"
    message Mutation tag "event_type":
        | room_upvote as "room.upvote" -> upvote since "2026-09-06" "Upvote a room.":
            room_id: int "The room; ` + "`id`" + ` is accepted too." also id
            weight: int = 1 "How much."
        | tone -> setTone since "2026-09-07" "Set the tone.":
            tone: text "The tone." one of "calm", "loud"
        | ping -> ping since "2026-09-08" "Nothing but a ping."
        | become -> become since "2026-09-11" "Switch who you are.":
            who: text "The new actor."
        | stamp -> stamp body envelope: Envelope since "2026-09-09" "A documented body."
        | then_run as "step_up.verify" -> thenRun since "2026-09-10" "Pattern fields.":
            then: text "What to do."
            then_<field>: text? "The pending action's fields." into then_fields
    action upvote(room_id: int, weight: int?) -> RoomDTO:
        requires member
        check exists(r in Room where r.id == room_id) "no such room" status 404
        set Room(room_id).votes = Room(room_id).votes + weight
        return RoomDTO{id: "" + room_id, votes: Room(room_id).votes}
    action setTone(tone: text):
        requires member
        check tone == "calm" || tone == "loud" "unknown tone" status 400
        add Seen { note: tone }
    action become(who: text):
        requires member
        establish actor who
    action ping():
        add Seen { note: "ping" }
    action stamp(envelope: Envelope) -> Envelope:
        add Seen { note: envelope.note }
        return envelope
    action thenRun(then: text, then_fields: json?) -> Envelope:
        return Envelope{cid: then, note: canonicalJson(then_fields)}
    action seed():
        add Room { votes: 0 }
    api POST "/events" -> Mutation rate write since "2026-09-06"
    view Home at "/":
        text "{count(Seen)}"
`

func TestDispatchRoute(t *testing.T) {
	g, err := compile.String(dispatchApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	srv.Run("ada", "member", true, "seed", nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/?as=ada")
	token := ""
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			token = c.Value
		}
	}
	resp.Body.Close()
	post := func(ct, body string, auth bool) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/events", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(raw)
	}
	js := "application/json"
	if code, body := post(js, `{"event_type":"room.upvote","room_id":1}`, true); code != 200 || !strings.Contains(body, `"votes":1`) {
		t.Fatalf("upvote = %d %s", code, body)
	}
	// The alias, and a form body.
	form := url.Values{"event_type": {"room.upvote"}, "id": {"1"}}.Encode()
	if code, body := post("application/x-www-form-urlencoded", form, true); code != 200 || !strings.Contains(body, `"votes":2`) {
		t.Fatalf("form upvote by alias = %d %s", code, body)
	}
	if code, _ := post(js, `{"event_type":"room.upvote","room_id":1}`, false); code != 401 {
		t.Fatalf("gated variant without a session = %d", code)
	}
	if code, body := post(js, `{"event_type":"room.upvote"}`, true); code != 400 || !strings.Contains(body, "room_id") {
		t.Fatalf("missing field = %d %s", code, body)
	}
	if code, body := post(js, `{"event_type":"nope"}`, true); code != 400 || !strings.Contains(body, "unknown event_type") {
		t.Fatalf("unknown variant = %d %s", code, body)
	}
	if code, _ := post(js, `{"event_type":"room.upvote","room_id":9}`, true); code != 404 {
		t.Fatalf("the action's own check = %d", code)
	}
	if code, body := post(js, `{"event_type":"ping"}`, false); code != 204 || body != "" {
		t.Fatalf("a variant with no reply = %d %q", code, body)
	}
	if code, body := post(js, `{"event_type":"stamp","cid":"c1","note":"hello"}`, false); code != 200 || !strings.Contains(body, `"note":"hello"`) {
		t.Fatalf("body variant = %d %s", code, body)
	}

	// A re-keyed session is handed back to a Bearer client.
	req, _ := http.NewRequest("POST", ts.URL+"/events", strings.NewReader(`{"event_type":"become","who":"bea"}`))
	req.Header.Set("Content-Type", js)
	req.Header.Set("Authorization", "Bearer "+token)
	rk, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rk.Body.Close()
	fresh := rk.Header.Get("X-Session-Token")
	if rk.StatusCode != 204 || fresh == "" || fresh == token {
		t.Fatalf("re-key = %d, X-Session-Token %q", rk.StatusCode, fresh)
	}
	token = fresh
	if code, body := post(js, `{"event_type":"room.upvote","room_id":1}`, true); code != 200 {
		t.Fatalf("the handed-back token does not work: %d %s", code, body)
	}

	// An array sent where the variant's field is text binds as its JSON text.
	if code, body := post(js, `{"event_type":"stamp","cid":"c2","note":"x"}`, false); code != 200 {
		t.Fatalf("stamp = %d %s", code, body)
	}
	if code, body := post(js, `{"event_type":"tone","tone":["calm"]}`, true); code != 400 || !strings.Contains(body, "unknown tone") {
		t.Fatalf("array into a text field = %d %s (want it bound as JSON text and refused by the action's own check)", code, body)
	}

	if code, body := post(js, `{"event_type":"step_up.verify","then":"wallet.send","then_amount":"5","then_to":"bob"}`, false); code != 200 || !strings.Contains(body, `"note":"{\"amount\":\"5\",\"to\":\"bob\"}"`) {
		t.Fatalf("pattern field = %d %s", code, body)
	}

	r, _ := http.Get(ts.URL + "/api/_contract")
	var doc map[string]any
	json.NewDecoder(r.Body).Decode(&doc)
	r.Body.Close()
	op := doc["paths"].(map[string]any)["/events"].(map[string]any)["post"].(map[string]any)
	rb, _ := json.Marshal(op["requestBody"])
	if !strings.Contains(string(rb), `"application/x-www-form-urlencoded"`) || !strings.Contains(string(rb), `"required":["event_type"]`) || op["responses"].(map[string]any)["default"] == nil {
		t.Fatalf("dispatch op = %v", op)
	}
	evs, _ := json.Marshal(doc["x-mutation-events"])
	for _, want := range []string{
		`{"event_type":"room.upvote","fields":[{"description":"The room; ` + "`id`" + ` is accepted too.","name":"room_id","required":true,"type":"integer"},{"description":"How much.","name":"weight","required":false,"type":"integer"}],"result":{"oneOf":[{"$ref":"#/components/schemas/RoomDTO"},{"type":"null"}]},"summary":"Upvote a room.","x-since":"2026-09-06"}`,
		`"enum":["calm","loud"]`,
		`"body":{"oneOf":[{"$ref":"#/components/schemas/Envelope"},{"type":"null"}]}`,
		`"name":"then_\u003cfield\u003e"`,
	} {
		if !strings.Contains(string(evs), want) {
			t.Fatalf("x-mutation-events lacks %s:\n%s", want, evs)
		}
	}

	// The compiler's gates.
	for name, c := range map[string][2]string{
		"no action":          {"| ping -> ping since", "| ping since"},
		"unknown field":      {`weight: int = 1 "How much."`, `weight: int = 1 "How much."` + "\n            extra: text"},
		"missing required":   {"action ping():", "action ping(x: int):"},
		"GET dispatch route": {`api POST "/events" -> Mutation`, `api GET "/events" -> Mutation`},
	} {
		src := strings.Replace(dispatchApp, c[0], c[1], 1)
		if _, err := compile.String(src); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

// ed25519Verify, sha256Hex, canonicalJson and shuffleOrder: the authority's
// crypto and content-identity builtins.
func TestAuthorityBuiltins(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sig := ed25519.Sign(priv, []byte("sha256:abc"))
	pb, sb := base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(sig)
	if callBuiltin("ed25519Verify", []any{pb, "sha256:abc", sb}) != true ||
		callBuiltin("ed25519Verify", []any{pb, "sha256:abd", sb}) != false ||
		callBuiltin("ed25519Verify", []any{"nope", "sha256:abc", sb}) != false {
		t.Fatal("ed25519Verify")
	}
	if callBuiltin("sha256Hex", []any{"abc"}) != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal("sha256Hex")
	}
	if got := callBuiltin("canonicalJson", []any{map[string]any{"b": "<&>", "a": []any{1, "x"}}}); got != `{"a":[1,"x"],"b":"<&>"}` {
		t.Fatalf("canonicalJson = %v", got)
	}
	// The legacy ballot's order for these seeds (its Fisher–Yates over
	// math/rand seeded by FNV-1a 64 of "viewer|work").
	for seed, want := range map[string]string{
		"a|b": "[4,3,2,1,0]",
		"11111111-2222-3333-4444-555555555555|66666666-7777-8888-9999-000000000000": "[2,4,1,0,3]",
		"": "[0,1,2,3,4]",
	} {
		raw, _ := json.Marshal(callBuiltin("shuffleOrder", []any{seed, 5}))
		if string(raw) != want {
			t.Errorf("shuffleOrder(%q, 5) = %s, want %s", seed, raw, want)
		}
	}
	src := `app A:
    action a() -> text:
        return sha256Hex("x")
    view Home at "/":
        text "{sha256Hex(\"x\")}"
`
	if _, err := compile.String(src); err == nil || !strings.Contains(err.Error(), "runs only on the authority") {
		t.Fatalf("authority-only builtin in a view: %v", err)
	}
}

// ecdsaP256Verify checks a WebCrypto-style P-256 signature (r||s, base64url)
// against an SPKI public key — legacy's pre-device signing path.
func TestECDSAP256Verify(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spki, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	digest := sha256.Sum256([]byte("sha256:abc"))
	r, s, _ := ecdsa.Sign(rand.Reader, priv, digest[:])
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	pub, sb := base64.StdEncoding.EncodeToString(spki), base64.RawURLEncoding.EncodeToString(sig)
	if callBuiltin("ecdsaP256Verify", []any{pub, "sha256:abc", sb}) != true ||
		callBuiltin("ecdsaP256Verify", []any{pub, "sha256:abd", sb}) != false ||
		callBuiltin("ecdsaP256Verify", []any{"x", "sha256:abc", sb}) != false {
		t.Fatal("ecdsaP256Verify")
	}
}
