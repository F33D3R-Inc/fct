package runtime

// Single sign-on via OpenID Connect — the modern SSO standard (Google,
// Microsoft Entra, Okta, Auth0, Keycloak, …). It is configured entirely from the
// environment, so no application code changes:
//
//	FACET_OIDC_ISSUER         e.g. https://accounts.google.com
//	FACET_OIDC_CLIENT_ID
//	FACET_OIDC_CLIENT_SECRET
//	FACET_OIDC_REDIRECT       e.g. https://app.example.com/auth/oidc/callback
//
// The flow is the authorization-code grant with PKCE: /auth/oidc/login sends the
// browser to the identity provider; /auth/oidc/callback exchanges the returned
// code for an ID token, maps its subject to a managed FacetUser (auto-
// provisioned on first sign-in), and signs the session in. The ID token comes
// straight from the provider's token endpoint over TLS, so its claims are
// trusted for this server-to-server exchange.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// oidcProvider holds the SSO configuration and the discovered endpoints.
type oidcProvider struct {
	issuer       string
	clientID     string
	clientSecret string
	redirect     string

	mu            sync.Mutex
	discovered    bool
	authEndpoint  string
	tokenEndpoint string

	states map[string]*oidcState // CSRF state -> the pending flow
}

// oidcState is one in-flight login: its PKCE verifier, nonce, and birth time.
type oidcState struct {
	verifier string
	created  time.Time
}

// newOIDCFromEnv builds a provider when the OIDC env vars are present, else nil
// (SSO simply off).
func newOIDCFromEnv() *oidcProvider {
	issuer := os.Getenv("FACET_OIDC_ISSUER")
	clientID := os.Getenv("FACET_OIDC_CLIENT_ID")
	redirect := os.Getenv("FACET_OIDC_REDIRECT")
	if issuer == "" || clientID == "" || redirect == "" {
		return nil
	}
	return &oidcProvider{
		issuer:       strings.TrimRight(issuer, "/"),
		clientID:     clientID,
		clientSecret: os.Getenv("FACET_OIDC_CLIENT_SECRET"),
		redirect:     redirect,
		states:       map[string]*oidcState{},
	}
}

// discovery struct mirrors the bits of the provider metadata we use.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// discover fetches and caches the provider's endpoints (once).
func (p *oidcProvider) discover() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discovered {
		return nil
	}
	res, err := http.Get(p.issuer + "/.well-known/openid-configuration")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var d oidcDiscovery
	if err := json.NewDecoder(res.Body).Decode(&d); err != nil {
		return err
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return fmt.Errorf("oidc discovery missing endpoints")
	}
	p.authEndpoint, p.tokenEndpoint = d.AuthorizationEndpoint, d.TokenEndpoint
	p.discovered = true
	return nil
}

// pkce returns a random code verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	verifier = randomToken(32)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authURL builds the authorization-request URL for a fresh login.
func (p *oidcProvider) authURL(state, challenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", p.clientID)
	v.Set("redirect_uri", p.redirect)
	v.Set("scope", "openid email profile")
	v.Set("state", state)
	v.Set("code_challenge", challenge)
	v.Set("code_challenge_method", "S256")
	return p.authEndpoint + "?" + v.Encode()
}

// idClaims are the subset of ID-token claims used to map an SSO identity to a
// Facet user.
type idClaims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
}

// parseIDClaims decodes (without signature verification) the claim set from a
// JWT ID token. The token arrives directly from the token endpoint over TLS, so
// for the code grant this is the trusted channel.
func parseIDClaims(idToken string) (idClaims, error) {
	var c idClaims
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return c, fmt.Errorf("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, err
	}
	if c.Sub == "" {
		return c, fmt.Errorf("id_token has no subject")
	}
	return c, nil
}

// oidcUsername chooses a stable local username for an SSO identity.
func oidcUsername(c idClaims) string {
	switch {
	case c.PreferredUsername != "":
		return c.PreferredUsername
	case c.Email != "":
		return c.Email
	default:
		return "oidc:" + c.Sub
	}
}

// ── server handlers ───────────────────────────────────────────────────────────

// handleOIDCLogin starts the SSO flow: it mints PKCE + state and redirects the
// browser to the identity provider.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.oidc.discover(); err != nil {
		http.Error(w, "sso unavailable", http.StatusBadGateway)
		return
	}
	verifier, challenge := pkce()
	state := randomToken(24)
	s.oidc.mu.Lock()
	// forget stale flows (older than 10 minutes) so the map cannot grow without
	// bound, then remember this one.
	for k, st := range s.oidc.states {
		if time.Since(st.created) > 10*time.Minute {
			delete(s.oidc.states, k)
		}
	}
	s.oidc.states[state] = &oidcState{verifier: verifier, created: time.Now()}
	s.oidc.mu.Unlock()
	http.Redirect(w, r, s.oidc.authURL(state, challenge), http.StatusFound)
}

// handleOIDCCallback completes the flow: validate state, exchange the code, read
// the ID token, provision/sign in the user, and land back on the app.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	s.oidc.mu.Lock()
	st := s.oidc.states[state]
	delete(s.oidc.states, state)
	s.oidc.mu.Unlock()
	if st == nil || code == "" {
		http.Error(w, "invalid sso state", http.StatusBadRequest)
		return
	}
	claims, err := s.oidc.exchange(code, st.verifier)
	if err != nil {
		http.Error(w, "sso token exchange failed", http.StatusBadGateway)
		return
	}
	sid := s.session(w, r)
	s.oidcSignIn(sid, claims)
	s.recordAudit(oidcUsername(claims), "ssoLogin", true, s.oidc.issuer)
	http.Redirect(w, r, "/", http.StatusFound)
}

// exchange swaps an authorization code for the ID token's claims.
func (p *oidcProvider) exchange(code, verifier string) (idClaims, error) {
	if err := p.discover(); err != nil {
		return idClaims{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.redirect)
	form.Set("client_id", p.clientID)
	form.Set("code_verifier", verifier)
	if p.clientSecret != "" {
		form.Set("client_secret", p.clientSecret)
	}
	res, err := http.PostForm(p.tokenEndpoint, form)
	if err != nil {
		return idClaims{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return idClaims{}, fmt.Errorf("token endpoint %d: %s", res.StatusCode, body)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return idClaims{}, err
	}
	if tok.IDToken == "" {
		return idClaims{}, fmt.Errorf("no id_token in token response")
	}
	return parseIDClaims(tok.IDToken)
}

// oidcSignIn maps an SSO identity to a managed user (provisioning on first
// sign-in) and stamps the session with it.
func (s *Server) oidcSignIn(sid string, claims idClaims) {
	username := oidcUsername(claims)
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		role := roleMember
		if len(s.entities[reservedUserEntity]) == 0 {
			role = roleAdmin
		}
		// An SSO account has no local password; store an unusable bcrypt hash.
		hash, _ := bcrypt.GenerateFromPassword([]byte(randomToken(32)), bcrypt.DefaultCost)
		s.nextID[reservedUserEntity]++
		u = record{
			"id": s.nextID[reservedUserEntity], "username": username,
			"password": string(hash), "role": role, "email": claims.Email,
			"verified": claims.EmailVerified, "verifyToken": "",
			"resetToken": "", "resetExpires": 0, "mfaSecret": "", "mfaEnabled": false,
		}
		s.entities[reservedUserEntity] = append(s.entities[reservedUserEntity], u)
		s.persist(s.store.Save(reservedUserEntity, u))
	}
	s.signIn(sid, u)
}
