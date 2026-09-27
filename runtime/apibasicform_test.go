package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A server-to-server exchange authenticates with HTTP Basic client
// credentials and reads a form body — the OAuth token endpoint's shape. The
// route binds the two halves of the credential and the form's fields to the
// action's parameters, refuses a missing credential with a Basic challenge,
// and documents both in the served contract.
func TestDeclaredRouteBasicCredentialsAndFormBody(t *testing.T) {
	g, err := compile.String(`
app Exchange:
    entity Client:
        id: int
        client_id: text
        client_secret: text @password
    type TokenDTO:
        access_token: text
        grant: text
    action exchange(client_id: text, client_secret: text, grant_type: text, code: text) -> TokenDTO:
        check grant_type == "authorization_code" "grant_type must be authorization_code" status 400 code "unsupported_grant_type"
        check exists(c in Client where c.client_id == client_id) "unknown client" status 401 code "invalid_client"
        let cid = max(c.id in Client where c.client_id == client_id)
        check verifyPassword(Client(cid).client_secret, client_secret) "unknown client" status 401 code "invalid_client"
        return TokenDTO{access_token: "tok-" + code, grant: grant_type}
    api POST "/api/v2/oauth/token" -> exchange auth dev_client basic client_id client_secret:
        summary "Exchange a code."
        body form
        errors 400, 401
    view Home at "/":
        text "hi"
`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.AddRow("Client", map[string]any{"client_id": "app-1", "client_secret": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	post := func(user, pass string, form url.Values, ct string) (int, http.Header, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", ct)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		return res.StatusCode, res.Header, body
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {"abc"}}

	st, h, body := post("", "", form, "application/x-www-form-urlencoded")
	if st != http.StatusUnauthorized || !strings.HasPrefix(h.Get("WWW-Authenticate"), `Basic realm="dev_client"`) {
		t.Errorf("no credential: %d %q, want 401 with a Basic challenge naming the scheme", st, h.Get("WWW-Authenticate"))
	}
	if e, _ := body["error"].(map[string]any); e == nil || e["code"] != "invalid_client" {
		t.Errorf("no credential: error = %v, want code invalid_client (RFC 6749 §5.2)", body["error"])
	}
	if st, _, body := post("app-1", "wrong", form, "application/x-www-form-urlencoded"); st != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d %v, want the action's 401", st, body)
	}
	bad := url.Values{"grant_type": {"password"}, "code": {"abc"}}
	if st, _, body := post("app-1", "s3cret", bad, "application/x-www-form-urlencoded"); st != http.StatusBadRequest {
		t.Errorf("bad grant_type from the form: %d %v, want the action's 400", st, body)
	}
	st, _, body = post("app-1", "s3cret", form, "application/x-www-form-urlencoded")
	if st != http.StatusOK || body["access_token"] != "tok-abc" || body["grant"] != "authorization_code" {
		t.Errorf("form exchange: %d %v, want 200 with the form's fields bound", st, body)
	}
	// A JSON body is still understood on a form route (the field names are the same).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v2/oauth/token", strings.NewReader(`{"grant_type":"authorization_code","code":"json"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("app-1", "s3cret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "tok-json") {
		t.Errorf("json on a form route: %d %s", res.StatusCode, raw)
	}

	// The served contract documents the credential scheme and the form body.
	res, err = http.Get(ts.URL + "/api/_contract")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("contract: %v", err)
	}
	op := doc["paths"].(map[string]any)["/api/v2/oauth/token"].(map[string]any)["post"].(map[string]any)
	if sec, _ := json.Marshal(op["security"]); string(sec) != `[{"dev_client":[]}]` {
		t.Errorf("security = %s, want the basic scheme", sec)
	}
	content := op["requestBody"].(map[string]any)["content"].(map[string]any)
	if _, ok := content["application/x-www-form-urlencoded"]; !ok {
		t.Errorf("requestBody content = %v, want application/x-www-form-urlencoded", content)
	}
	props := content["application/x-www-form-urlencoded"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	if _, leaked := props["client_secret"]; leaked || props["code"] == nil {
		t.Errorf("form properties = %v: the credential must not be a body field, the code must", props)
	}
	schemes := doc["components"].(map[string]any)["securitySchemes"].(map[string]any)
	if sch, _ := schemes["dev_client"].(map[string]any); sch == nil || sch["scheme"] != "basic" {
		t.Errorf("securitySchemes = %v, want dev_client as http basic", schemes)
	}
}
