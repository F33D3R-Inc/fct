// Command oidc is the worked SSO example for FA: a complete OpenID Connect
// relying party in the Go standard library only — no OAuth/OIDC dependency —
// wired into FA's sessions, Identify, and a Guard.
//
// The flow (authorization code + PKCE, the one every IdP supports):
//
//	/login    → remember state/nonce/PKCE verifier in a short-lived signed
//	            cookie, redirect to the IdP's authorization endpoint
//	/callback → verify state, exchange the code at the token endpoint, verify
//	            the ID token's RS256 signature against the IdP's JWKS plus its
//	            iss/aud/exp/nonce claims, then Save the FA session
//	/logout   → clear the session
//
// After that, SSO identity IS FA identity: app.Identify(sessions.Identity)
// makes the IdP "sub" the identity that scopes SSE delivery (EmitTo), guards
// (app.Guard), and `who:` policies (RenderFor).
//
// Works against any spec-compliant IdP (Keycloak, Okta, Entra ID, Auth0,
// Google, Dex…). Configure and run:
//
//	export OIDC_ISSUER=https://accounts.google.com
//	export OIDC_CLIENT_ID=...        # from your IdP
//	export OIDC_CLIENT_SECRET=...    # omit for a public client (PKCE only)
//	export OIDC_REDIRECT_URL=http://localhost:7373/callback
//	go run ./examples/oidc
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/F33D3R-Inc/fct/fa"
)

func main() {
	issuer := os.Getenv("OIDC_ISSUER")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	if issuer == "" || clientID == "" {
		log.Fatal("set OIDC_ISSUER and OIDC_CLIENT_ID (and usually OIDC_CLIENT_SECRET, OIDC_REDIRECT_URL)")
	}
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")
	if redirectURL == "" {
		redirectURL = "http://localhost:7373/callback"
	}

	provider, err := discover(issuer)
	if err != nil {
		log.Fatalf("OIDC discovery: %v", err)
	}

	app := fa.New([]byte(`{}`))
	sessions := app.Sessions(fa.SessionInsecure()) // Insecure: local dev only — drop for production (HTTPS)
	app.Identify(sessions.Identity)                // IdP sub → FA identity (SSE scopes, Guard, who:)

	// A guarded event: only authenticated users (any non-empty identity) may
	// fire it. The same identity drives `who:` policies in facets via RenderFor.
	app.On("whoami", func(c fa.Ctx) ([]fa.Event, error) {
		return []fa.Event{{Op: "replace", FacetID: "Who:1",
			Fragment: "<b>" + template.HTMLEscapeString(c.Identity) + "</b>"}}, nil
	})
	app.Guard("whoami", func(c fa.Ctx) bool { return c.Identity != "" })

	rp := &relyingParty{
		provider:     provider,
		clientID:     clientID,
		clientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		redirectURL:  redirectURL,
		sessions:     sessions,
		// The login-flow state (state/nonce/PKCE verifier) lives in its own
		// short-lived signed cookie — same key as the session (app.Sessions wires
		// it), just a different name and a short TTL.
		flow: app.Sessions(fa.SessionName("fa_oidc"), fa.SessionMaxAge(10*time.Minute), fa.SessionInsecure()),
	}

	mux := http.NewServeMux()
	app.Mount(mux)
	mux.HandleFunc("GET /login", rp.login)
	mux.HandleFunc("GET /callback", rp.callback)
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) {
		sessions.Clear(w)
		http.Redirect(w, r, "/", http.StatusFound)
	})
	app.HandlePage(mux, fa.ShellOptions{Title: "FA + OIDC"}, func(r *http.Request) template.HTML {
		if uid := sessions.Get(r, "uid"); uid != "" {
			email := template.HTMLEscapeString(sessions.Get(r, "email"))
			return template.HTML(`<p>Signed in as ` + email + `</p>` +
				`<p data-facet-id="Who:1" data-action="whoami">click to ask the server who you are</p>` +
				`<p><a href="/logout">Log out</a></p>`)
		}
		return `<p><a href="/login">Log in with your identity provider</a></p>`
	})

	addr := os.Getenv("FA_ADDR")
	if addr == "" {
		addr = ":7373"
	}
	log.Printf("oidc example on %s (issuer %s)", addr, issuer)
	log.Fatal(http.ListenAndServe(addr, fa.LogRequests(mux)))
}

// ── the relying party ─────────────────────────────────────────────────────────

type relyingParty struct {
	provider     *providerConfig
	clientID     string
	clientSecret string
	redirectURL  string
	sessions     *fa.SessionManager // the app session (uid/email after login)
	flow         *fa.SessionManager // short-lived state/nonce/PKCE cookie
}

// login starts the authorization-code + PKCE flow.
func (rp *relyingParty) login(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := randToken(), randToken(), randToken()
	rp.flow.Save(w, map[string]string{"state": state, "nonce": nonce, "verifier": verifier})

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {rp.clientID},
		"redirect_uri":          {rp.redirectURL},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	http.Redirect(w, r, rp.provider.AuthorizationEndpoint+"?"+q.Encode(), http.StatusFound)
}

// callback finishes the flow: state check, code exchange, ID-token
// verification, then the FA session is the login.
func (rp *relyingParty) callback(w http.ResponseWriter, r *http.Request) {
	flow := rp.flow.Load(r)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "IdP error: "+e, http.StatusBadGateway)
		return
	}
	if s := r.URL.Query().Get("state"); s == "" || s != flow["state"] {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	idToken, err := rp.exchange(r.URL.Query().Get("code"), flow["verifier"])
	if err != nil {
		http.Error(w, "code exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	claims, err := verifyIDToken(idToken, rp.provider, rp.clientID, flow["nonce"])
	if err != nil {
		http.Error(w, "id_token rejected: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// The verified IdP identity becomes the FA session. "uid" is what
	// SessionManager.Identity returns — i.e. what Guard/EmitTo/who: see.
	rp.sessions.Save(w, map[string]string{"uid": claims.Subject, "email": claims.Email})
	http.Redirect(w, r, "/", http.StatusFound)
}

// exchange redeems the authorization code at the token endpoint.
func (rp *relyingParty) exchange(code, verifier string) (string, error) {
	if code == "" {
		return "", errors.New("missing code")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {rp.redirectURL},
		"client_id":     {rp.clientID},
		"code_verifier": {verifier},
	}
	if rp.clientSecret != "" {
		form.Set("client_secret", rp.clientSecret)
	}
	resp, err := http.PostForm(rp.provider.TokenEndpoint, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Error != "" {
		return "", errors.New(body.Error)
	}
	if body.IDToken == "" {
		return "", errors.New("no id_token in response")
	}
	return body.IDToken, nil
}

// ── OIDC provider metadata + ID-token verification (stdlib only) ──────────────

type providerConfig struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// discover fetches {issuer}/.well-known/openid-configuration.
func discover(issuer string) (*providerConfig, error) {
	resp, err := http.Get(strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var p providerConfig
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, err
	}
	if p.AuthorizationEndpoint == "" || p.TokenEndpoint == "" || p.JWKSURI == "" {
		return nil, errors.New("incomplete discovery document")
	}
	return &p, nil
}

type idClaims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Nonce    string `json:"nonce"`
	Expiry   int64  `json:"exp"`
	Audience any    `json:"aud"` // string or []string per spec
}

// verifyIDToken checks the JWT's RS256 signature against the IdP's published
// JWKS and the iss / aud / exp / nonce claims. Fails closed on anything
// unexpected — including an algorithm other than RS256 (no `alg:none` games).
func verifyIDToken(token string, p *providerConfig, clientID, nonce string) (*idClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	headJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headJSON, &head); err != nil {
		return nil, err
	}
	if head.Alg != "RS256" {
		return nil, fmt.Errorf("unexpected alg %q", head.Alg)
	}

	key, err := fetchJWK(p.JWKSURI, head.Kid)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("bad signature")
	}

	claimJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c idClaims
	if err := json.Unmarshal(claimJSON, &c); err != nil {
		return nil, err
	}
	switch {
	case strings.TrimRight(c.Issuer, "/") != strings.TrimRight(p.Issuer, "/"):
		return nil, fmt.Errorf("issuer %q is not %q", c.Issuer, p.Issuer)
	case !hasAudience(c.Audience, clientID):
		return nil, errors.New("token not for this client (aud)")
	case time.Now().Unix() >= c.Expiry:
		return nil, errors.New("token expired")
	case c.Nonce != nonce:
		return nil, errors.New("nonce mismatch")
	case c.Subject == "":
		return nil, errors.New("no sub claim")
	}
	return &c, nil
}

func hasAudience(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// fetchJWK pulls the IdP's JWKS and returns the RSA public key with the given
// kid (or the sole key when the set has exactly one and the token names none).
func fetchJWK(jwksURI, kid string) (*rsa.PublicKey, error) {
	resp, err := http.Get(jwksURI)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if k.Kid != kid && !(kid == "" && len(set.Keys) == 1) {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}, nil
	}
	return nil, fmt.Errorf("no RSA key %q in JWKS", kid)
}

// randToken returns 32 bytes of crypto randomness, base64url-encoded.
func randToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
