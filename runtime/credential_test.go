package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
)

// Credentials end to end: a @password field is hashed on every write and never
// served; verifyPassword checks a candidate; the TOTP builtins mint and check a
// real RFC 6238 secret; and `sessionToken` is the credential Bearer accepts.
const credentialRuntimeApp = `app C:
    type TokenDTO:
        token: text
    type SecretDTO:
        secret: text
    entity Account:
        id: int
        handle: text
        password: text @password @min(8)
        totp: text @secret
    policy member:
        actor != "guest"
    action signup(handle: text, password: text) -> TokenDTO:
        add Account { handle: handle, password: password, totp: "" }
        establish actor handle
        return TokenDTO{token: sessionToken}
    action login(handle: text, password: text) -> TokenDTO:
        check exists(a in Account where a.handle == handle) "no such account" status 401
        let id = max(a.id in Account where a.handle == handle)
        check verifyPassword(Account(id).password, password) "wrong password" status 401
        establish actor handle
        return TokenDTO{token: sessionToken}
    action changePassword(password: text):
        requires member
        set a in Account where a.handle == actor:
            password = password
    action whoami() -> Account:
        requires member
        let id = max(a.id in Account where a.handle == actor)
        return Account(id)
    action enroll() -> SecretDTO:
        requires member
        let s = totpSecret()
        set a in Account where a.handle == actor:
            totp = s
        return SecretDTO{secret: s}
    action confirm(code: text):
        requires member
        let id = max(a.id in Account where a.handle == actor)
        check totpValid(Account(id).totp, code) "invalid code" status 401
    action sessionKey() -> TokenDTO:
        requires member
        return TokenDTO{token: session}
    action revokeKey(key: text):
        requires member
        revoke key
    action revokeThenFail(key: text):
        requires member
        revoke key
        check key == "" "refused after the revoke"
    action signOut():
        requires member
        revoke session
    action disableTotp(code: text):
        requires member
        let id = max(a.id in Account where a.handle == actor)
        check totpValid(Account(id).totp, code) "invalid code" status 401
        set Account(id).totp = ""
    api POST "/api/accounts" -> signup status 201
    api POST "/api/sessions" -> login status 201
    api PUT "/api/me/password" -> changePassword status 204
    api GET "/api/me" -> whoami
    api POST "/api/me/totp" -> enroll status 201
    api PUT "/api/me/totp" -> confirm status 204
    api DELETE "/api/me/totp" -> disableTotp status 204
    api GET "/api/me/session" -> sessionKey
    api DELETE "/api/sessions" -> revokeKey status 204
    api POST "/api/sessions/failing" -> revokeThenFail status 204
    api DELETE "/api/sessions/current" -> signOut status 204
    view Home at "/":
        for a in Account by id:
            text "{a.handle}"
`

func credentialServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	g, err := compile.String(credentialRuntimeApp)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Shutdown() })
	return srv, ts
}

func TestPasswordFieldHashedAndNeverServed(t *testing.T) {
	srv, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}

	if code, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"short"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("a 5-char password against @min(8) = %d %v, want 422 (the constraint reads the plaintext)", code, body)
	}
	code, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	if code != http.StatusCreated {
		t.Fatalf("signup = %d %v", code, body)
	}

	stored := func() string {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return toStr(srv.entities["Account"][0].(record)["password"])
	}
	first := stored()
	if !strings.HasPrefix(first, "$2") || strings.Contains(first, "correct horse") {
		t.Fatalf("stored password = %q, want a bcrypt hash and never the plaintext", first)
	}

	if code, _, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"wrong horse"}`); code != http.StatusUnauthorized {
		t.Fatalf("login with the wrong password = %d, want 401", code)
	}
	code, body, _ = anon.do("POST", "/api/sessions", `{"handle":"ada","password":"correct horse"}`)
	if code != http.StatusCreated {
		t.Fatalf("login = %d %v", code, body)
	}
	me := &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}

	// An entity-typed reply is a projection like any other: no hash in it.
	code, row, _ := me.do("GET", "/api/me", "")
	if code != http.StatusOK || row["handle"] != "ada" {
		t.Fatalf("whoami = %d %v", code, row)
	}
	if _, leaked := row["password"]; leaked {
		t.Fatalf("an Account reply carried its password hash: %v", row)
	}

	// A filtered set hashes too, and the old password stops working.
	if code, body, _ := me.do("PUT", "/api/me/password", `{"password":"battery staple"}`); code != http.StatusNoContent {
		t.Fatalf("changePassword = %d %v", code, body)
	}
	if second := stored(); second == first || !strings.HasPrefix(second, "$2") {
		t.Fatalf("after a change the stored value = %q, want a fresh bcrypt hash", second)
	}
	if code, _, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"correct horse"}`); code != http.StatusUnauthorized {
		t.Fatalf("the old password still logs in (%d)", code)
	}
	if code, _, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"battery staple"}`); code != http.StatusCreated {
		t.Fatalf("the new password does not log in (%d)", code)
	}

	// Every other door a row takes out: the page bootstrap, the export, the
	// admin console.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(page), "$2a$") {
		t.Fatal("the page bootstrap carried a password hash")
	}
	var acct ir.Entity
	for _, e := range srv.ir.Entities {
		if e.Name == "Account" {
			acct = e
		}
	}
	srv.mu.Lock()
	rec := srv.entities["Account"][0].(record)
	exported := redactRow(acct, rec)
	cell := displayCell(acct, "password", rec["password"])
	srv.mu.Unlock()
	if _, leaked := exported["password"]; leaked {
		t.Fatalf("export carried the password hash: %v", exported)
	}
	if cell != "••••••" {
		t.Fatalf("admin list cell = %q, want it masked", cell)
	}
}

// The admin editor writes a @password field through the same hashing every
// action write takes, and a blank field leaves the stored hash alone.
func TestAdminWritesPasswordHashed(t *testing.T) {
	srv, ts := credentialServer(t)
	resp, err := http.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var sid string
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			sid, _ = verifySigned(c.Value)
		}
	}
	srv.mu.Lock()
	srv.sessions[sid].role = roleAdmin
	srv.mu.Unlock()

	save := func(form string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/admin/_save", strings.NewReader(form+"&_csrf="+csrfToken(sid)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "fa_sid", Value: signValue(sid)})
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := save("_entity=Account&handle=bo&password=admin-set-pw&totp="); code >= 400 {
		t.Fatalf("admin create = %d", code)
	}
	srv.mu.Lock()
	h := toStr(srv.entities["Account"][0].(record)["password"])
	srv.mu.Unlock()
	if !passwordMatches(h, "admin-set-pw") {
		t.Fatalf("admin-written password stored as %q, want a hash of what was typed", h)
	}
	if code := save("_entity=Account&_id=1&handle=bo&password=&totp="); code >= 400 {
		t.Fatalf("admin edit = %d", code)
	}
	srv.mu.Lock()
	after := toStr(srv.entities["Account"][0].(record)["password"])
	srv.mu.Unlock()
	if after != h {
		t.Fatal("an admin edit with the password left blank replaced the stored hash")
	}

	req, _ := http.NewRequest("GET", ts.URL+"/admin/Account", nil)
	req.AddCookie(&http.Cookie{Name: "fa_sid", Value: signValue(sid)})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(list), "$2a$") || strings.Contains(string(list), "admin-set-pw") {
		t.Fatal("the admin list served the password or its hash")
	}
}

// sessionToken is the caller's own session, signed exactly as the cookie is:
// the token a login hands out authenticates as `Authorization: Bearer`, and it
// names the same session the response's cookie does.
func TestSessionTokenIsTheBearerCredential(t *testing.T) {
	_, ts := credentialServer(t)
	resp, err := http.Post(ts.URL+"/api/accounts", "application/json", strings.NewReader(`{"handle":"ada","password":"correct horse"}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	token := toStr(body["token"])
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			cookie = c.Value
		}
	}
	if token == "" || token != cookie {
		t.Fatalf("token %q and session cookie %q differ — they must name one session", token, cookie)
	}
	me := &apiClient{t: t, base: ts.URL, token: token}
	if code, row, _ := me.do("GET", "/api/me", ""); code != http.StatusOK || row["handle"] != "ada" {
		t.Fatalf("Bearer <token> /api/me = %d %v, want ada", code, row)
	}
	forged := &apiClient{t: t, base: ts.URL, token: token + "x"}
	if code, _, _ := forged.do("GET", "/api/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("a tampered token = %d, want 401", code)
	}
	if got := sessionToken(systemSID); got != "" {
		t.Fatalf("the system session was handed a token %q — that is an admin credential", got)
	}
}

func TestTOTPBuiltins(t *testing.T) {
	_, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	me := &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}

	if code, _, _ := me.do("PUT", "/api/me/totp", `{"code":"000000"}`); code != http.StatusUnauthorized {
		t.Fatalf("a code before enrollment (no secret) = %d, want 401", code)
	}
	code, body, _ := me.do("POST", "/api/me/totp", "")
	if code != http.StatusCreated {
		t.Fatalf("enroll = %d %v", code, body)
	}
	secret := toStr(body["secret"])
	if _, err := base32NoPad.DecodeString(secret); err != nil || len(secret) != 32 {
		t.Fatalf("totpSecret() = %q, want 32 chars of base32 (160 bits)", secret)
	}
	good := totpCode(secret, time.Now())
	bad := "000000"
	if bad == good {
		bad = "111111"
	}
	for _, c := range []string{bad, "12345", "abcdef", ""} {
		if code, _, _ := me.do("PUT", "/api/me/totp", `{"code":"`+c+`"}`); code != http.StatusUnauthorized {
			t.Errorf("code %q = %d, want 401", c, code)
		}
	}
	if code, body, _ := me.do("PUT", "/api/me/totp", `{"code":"`+good+`"}`); code != http.StatusNoContent {
		t.Fatalf("the current code = %d %v, want 204", code, body)
	}
}

// A list parameter binds from a comma-separated or repeated query key, a JSON
// array body, and the generic /api/<action> args — and `in` matches whole ids,
// never a substring (`?ids=12` is 12, not 1, 2 and 12).
func TestActionListParams(t *testing.T) {
	g, err := compile.String(`app L:
    type DTO:
        id: int
    entity Work:
        id: int
        title: text
    action addWork(title: text):
        add Work { title: title }
    action getWorks(ids: [int]) -> [DTO]:
        return list(DTO{id: w.id} in Work where w.id in ids by id)
    action pickWorks(ids: [int], titles: [text]?) -> [DTO]:
        return list(DTO{id: w.id} in Work where w.id in ids || w.title in titles by id)
    api GET "/api/works" -> getWorks
    api POST "/api/works/picks" -> pickWorks
    view V at "/":
        text "{count(Work)}"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for i := 1; i <= 12; i++ {
		srv.runAction("t", srv.byAction["addWork"], []any{"w" + itoa(i)})
	}
	ids := func(arr []any) []int {
		var out []int
		for _, x := range arr {
			out = append(out, toInt(x.(map[string]any)["id"]))
		}
		return out
	}
	c := &apiClient{t: t, base: ts.URL}
	for _, tc := range []struct {
		method, path, body string
		want               []int
	}{
		{"GET", "/api/works?ids=12", "", []int{12}},
		{"GET", "/api/works?ids=1,2", "", []int{1, 2}},
		{"GET", "/api/works?ids=2&ids=11,3", "", []int{2, 3, 11}},
		{"POST", "/api/works/picks", `{"ids":[12,5]}`, []int{5, 12}},
		{"POST", "/api/works/picks", `{"ids":"7","titles":["w1"]}`, []int{1, 7}},
	} {
		code, obj, arr := c.do(tc.method, tc.path, tc.body)
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("%s %s = %d %v", tc.method, tc.path, code, obj)
		}
		if got := ids(arr); jsonOf(got) != jsonOf(tc.want) {
			t.Errorf("%s %s %s = %v, want %v", tc.method, tc.path, tc.body, got, tc.want)
		}
	}
	if code, obj, _ := c.do("GET", "/api/works?ids=1,x", ""); code != http.StatusBadRequest || !strings.Contains(errMessage(obj), "[int]") {
		t.Fatalf("a non-integer element = %d %v, want 400 naming [int]", code, obj)
	}

	// The generic action endpoint decodes the same list from its positional args.
	resp, err := http.Post(ts.URL+"/api/getWorks", "application/json", strings.NewReader(`{"args":[[3,4]]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Value []any `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if got := ids(reply.Value); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("/api/getWorks args [[3,4]] = %v, want [3 4]", got)
	}
}

func sidCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == "fa_sid" {
			return c.Value
		}
	}
	return ""
}

func cookieGET(t *testing.T, url, cookie string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.AddCookie(&http.Cookie{Name: "fa_sid", Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// `establish` moves the session to a fresh id once the action commits, the way
// the built-in login does: an id a visitor held (or was handed) before signing
// in never becomes the signed-in session.
func TestEstablishRekeysTheSession(t *testing.T) {
	srv, ts := credentialServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	planted := sidCookie(resp)
	if planted == "" {
		t.Fatal("no session minted for the first visit")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/accounts", strings.NewReader(`{"handle":"ada","password":"correct horse"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "fa_sid", Value: planted})
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	fresh := sidCookie(resp)
	if fresh == "" || fresh == planted {
		t.Fatalf("signing in kept the planted session id (cookie %q)", fresh)
	}
	if n := strings.Count(strings.Join(resp.Header.Values("Set-Cookie"), "\n"), "fa_sid="); n != 1 {
		t.Fatalf("the response carries %d fa_sid cookies, want exactly the new one", n)
	}
	if toStr(body["token"]) != fresh {
		t.Fatalf("token %q is not the re-keyed session %q", body["token"], fresh)
	}
	if code := cookieGET(t, ts.URL+"/api/me", planted); code != http.StatusUnauthorized {
		t.Fatalf("the planted id still authenticates after sign-in (%d)", code)
	}
	if code := cookieGET(t, ts.URL+"/api/me", fresh); code != http.StatusOK {
		t.Fatalf("the new id does not authenticate (%d)", code)
	}
	// A failed establish re-keys nothing.
	before := len(srv.sessions)
	anon := &apiClient{t: t, base: ts.URL}
	if code, _, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"nope nope"}`); code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", code)
	}
	srv.mu.Lock()
	after := len(srv.sessions)
	srv.mu.Unlock()
	if after > before+1 {
		t.Fatalf("a refused login left %d new sessions", after-before)
	}
}

// `revoke` ends the runtime session behind a `session` key, so a revoked token
// is refused everywhere — and only when the action commits.
func TestRevokeEndsTheRuntimeSession(t *testing.T) {
	_, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	login := func() *apiClient {
		_, body, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"correct horse"}`)
		return &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}
	}
	a, b := login(), login()
	_, keyB, _ := b.do("GET", "/api/me/session", "")

	if code, _, _ := a.do("POST", "/api/sessions/failing", `{"key":"`+toStr(keyB["token"])+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("revokeThenFail = %d", code)
	}
	if code, _, _ := b.do("GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("a revoke in a failed action still ended the session (%d)", code)
	}
	if code, _, _ := a.do("DELETE", "/api/sessions", `{"key":"`+toStr(keyB["token"])+`"}`); code != http.StatusNoContent {
		t.Fatalf("revokeKey = %d", code)
	}
	if code, _, _ := b.do("GET", "/api/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("a revoked token still authenticates (%d)", code)
	}
	if code, _, _ := a.do("GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("revoking another session ended the caller's (%d)", code)
	}
	if code, _, _ := a.do("DELETE", "/api/sessions/current", ""); code != http.StatusNoContent {
		t.Fatalf("signOut = %d", code)
	}
	if code, _, _ := a.do("GET", "/api/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("a signed-out token still authenticates (%d)", code)
	}
}

// A @secret field is decrypted for the program, never carried by a row: the
// entity API, an entity-typed reply and the admin form all leave it out.
func TestSecretFieldNeverServedInRows(t *testing.T) {
	srv, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	me := &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}
	_, enrolled, _ := me.do("POST", "/api/me/totp", "")
	secret := toStr(enrolled["secret"])

	if _, row, _ := me.do("GET", "/api/me", ""); row["totp"] != nil || strings.Contains(jsonOf(row), secret) {
		t.Fatalf("an Account reply carried the @secret totp: %v", row)
	}
	srv.mu.Lock()
	rows := srv.visibleRows("Account", srv.entities["Account"], nil)
	full := srv.visibleRows("Account", srv.entities["Account"], srv.scope(""))
	srv.mu.Unlock()
	if strings.Contains(jsonOf(rows), secret) || strings.Contains(jsonOf(full), secret) {
		t.Fatal("a row projection carried the @secret value")
	}
	for _, e := range srv.ir.Entities {
		for _, f := range e.Fields {
			if f.Name == "totp" && strings.Contains(adminInput(f, secret), secret) {
				t.Fatal("the admin form echoed the @secret value")
			}
		}
	}
}

// randomToken draws uniformly from the OS CSPRNG over a readable alphabet.
func TestRandomToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		tok := secureText(12)
		if len(tok) != 12 || strings.Trim(tok, tokenAlphabet) != "" || seen[tok] {
			t.Fatalf("secureText(12) = %q (repeat: %v)", tok, seen[tok])
		}
		seen[tok] = true
	}
	if secureText(0) != "" || len(secureText(1<<20)) != maxSecureText {
		t.Fatal("randomToken's length bounds are not enforced")
	}
}

// A declared DELETE binds its non-path parameters from a JSON body, as the
// contract publishes them.
func TestDeclaredDeleteReadsBody(t *testing.T) {
	_, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	me := &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}
	_, enrolled, _ := me.do("POST", "/api/me/totp", "")
	if code, obj, _ := me.do("DELETE", "/api/me/totp", `{"code":"000000"}`); code != http.StatusUnauthorized {
		t.Fatalf("DELETE with a wrong code in the body = %d %v, want 401", code, obj)
	}
	good := totpCode(toStr(enrolled["secret"]), time.Now())
	if code, obj, _ := me.do("DELETE", "/api/me/totp", `{"code":"`+good+`"}`); code != http.StatusNoContent {
		t.Fatalf("DELETE with the current code in the body = %d %v, want 204", code, obj)
	}
	_, doc, _ := me.do("GET", "/api/_contract", "")
	var op map[string]any
	json.Unmarshal([]byte(jsonOf(doc["paths"].(map[string]any)["/api/me/totp"].(map[string]any)["delete"])), &op)
	if ps, _ := op["parameters"].([]any); op["requestBody"] == nil || len(ps) != 0 {
		t.Fatalf("the contract publishes DELETE /api/me/totp as %v, want a JSON body", op)
	}
}

// The generic /api/<action> endpoint resolves a Bearer token through the same
// sidForRequest the declared routes use.
func TestGenericActionAcceptsBearer(t *testing.T) {
	_, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	call := func(token string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", ts.URL+"/api/whoami", strings.NewReader(`{"args":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := call(toStr(body["token"]))
	if v, _ := out["value"].(map[string]any); code != http.StatusOK || v["handle"] != "ada" {
		t.Fatalf("Bearer /api/whoami = %d %v, want ada", code, out)
	}
	if code, _ := call(toStr(body["token"]) + "x"); code != http.StatusUnauthorized {
		t.Fatalf("a tampered Bearer on /api/whoami = %d, want 401", code)
	}
}

// `@secret @requires(policy)` is the explicit allowance: the plaintext reaches
// exactly the actors the policy admits, and still never the anonymous stream.
func TestSecretFieldServedOnlyWhereRequiresAdmits(t *testing.T) {
	g, err := compile.String(`app S:
    entity Key:
        id: int
        owner: text
        key: text @secret @requires(member)
        read: owner == actor
    policy member:
        actor != "guest"
    view V at "/":
        text "x"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	rows := []any{record{"id": 1, "owner": "ada", "key": "live_ada_1"}}
	if got := jsonOf(srv.visibleRows("Key", rows, map[string]any{"actor": "ada"})); !strings.Contains(got, "live_ada_1") {
		t.Fatalf("an admitted actor did not receive the @secret @requires field: %s", got)
	}
	if got := jsonOf(srv.visibleRows("Key", rows, map[string]any{"actor": "guest"})); strings.Contains(got, "live_ada_1") {
		t.Fatalf("a guest received it: %s", got)
	}
	if got := jsonOf(srv.visibleRows("Key", rows, nil)); strings.Contains(got, "live_ada_1") {
		t.Fatalf("the stream (no actor) received it: %s", got)
	}
}

// The id+secret credential shape (dev access tokens, reset tokens): the id
// selects the row, the secret is checked against its @password hash, so the
// store never holds a usable token and a forged or reused secret is refused.
func TestSelectorVerifierToken(t *testing.T) {
	g, err := compile.String(`app T:
    type TokenDTO:
        token: text
    entity Grant:
        id: int
        secret: text @password
        expires: int
    proc tokenPart(t: text, i: int) -> text:
        let parts = split(t, ".")
        if len(parts) != 2:
            return ""
        return parts[i]
    action issue() -> TokenDTO:
        let secret = randomToken(40)
        let gid = add Grant { secret: secret, expires: now() + 3600 }
        return TokenDTO{token: "" + gid + "." + secret}
    action use(token: text) -> TokenDTO:
        let t0 = now()
        let sel = do tokenPart(token, 0)
        let secret = do tokenPart(token, 1)
        check exists(x in Grant where "" + x.id == sel && x.expires > t0) "invalid token" status 401
        let gid = max(x.id in Grant where "" + x.id == sel)
        check verifyPassword(Grant(gid).secret, secret) "invalid token" status 401
        return TokenDTO{token: "ok"}
    api POST "/api/grants" -> issue status 201
    api POST "/api/grants/use" -> use
    view V at "/":
        text "x"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	c := &apiClient{t: t, base: ts.URL}
	_, body, _ := c.do("POST", "/api/grants", "")
	token := toStr(body["token"])
	id, secret, _ := strings.Cut(token, ".")
	srv.mu.Lock()
	stored := toStr(srv.entities["Grant"][0].(record)["secret"])
	srv.mu.Unlock()
	if len(secret) != 40 || strings.Contains(stored, secret) {
		t.Fatalf("token %q / stored %q: want a 40-char secret stored only as a hash", token, stored)
	}
	for _, bad := range []string{id + ".wrong", id, "9." + secret, ""} {
		if code, _, _ := c.do("POST", "/api/grants/use", `{"token":"`+bad+`"}`); code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401", bad, code)
		}
	}
	if code, _, _ := c.do("POST", "/api/grants/use", `{"token":"`+token+`"}`); code != http.StatusOK {
		t.Fatalf("the issued token = %d, want 200", code)
	}
}
