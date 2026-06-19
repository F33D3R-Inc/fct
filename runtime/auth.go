package runtime

import (
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Built-in authentication and the account lifecycle. When an app declares
// `auth`, the runtime manages the reserved FacetUser table and provides the
// server actions that operate on it: identity (signup/login/logout), RBAC
// management (setRole), password reset, email/account verification, and MFA
// enrollment plus a second factor at login. The view calls them like any other
// action. Passwords and tokens are stored hashed; the TOTP secret is encrypted
// at rest; the table is hidden from the API and the live stream.
//
// Account verification and password reset issue a one-time token. In this build
// the token is returned in the response (a development convenience) so the flow
// is end-to-end usable and testable; a production deployment would instead mail
// the token and drop it from the body.

const reservedUserEntity = "FacetUser"

const (
	roleAdmin  = "admin"
	roleMember = "member"
	roleGuest  = "guest"
)

const resetTTL = time.Hour // a password-reset token's lifetime

func isAuthAction(name string) bool {
	switch name {
	case "signup", "login", "logout", "setRole", "requestReset",
		"resetPassword", "verifyEmail", "enableMFA", "confirmMFA", "loginMFA":
		return true
	}
	return false
}

// runAuth dispatches a built-in auth action against the session and user store.
func (s *Server) runAuth(w http.ResponseWriter, r *http.Request, action string, args []any) {
	sid := s.session(w, r)
	switch action {
	case "signup":
		s.authSignup(w, sid, argStr(args, 0), argStr(args, 1))
	case "login":
		s.authLogin(w, sid, argStr(args, 0), argStr(args, 1))
	case "logout":
		s.authLogout(w, sid)
	case "setRole":
		s.authSetRole(w, sid, argStr(args, 0), argStr(args, 1))
	case "requestReset":
		s.authRequestReset(w, argStr(args, 0))
	case "resetPassword":
		s.authResetPassword(w, argStr(args, 0), argStr(args, 1), argStr(args, 2))
	case "verifyEmail":
		s.authVerifyEmail(w, sid, argStr(args, 0))
	case "enableMFA":
		s.authEnableMFA(w, sid)
	case "confirmMFA":
		s.authConfirmMFA(w, sid, argStr(args, 0))
	case "loginMFA":
		s.authLoginMFA(w, sid, argStr(args, 0), argStr(args, 1))
	default:
		http.Error(w, "unknown auth action", http.StatusNotFound)
	}
}

// findUser returns the user row with the given username, or nil. Caller holds mu.
func (s *Server) findUser(username string) record {
	for _, u := range s.entities[reservedUserEntity] {
		if m, ok := u.(record); ok && toStr(m["username"]) == username {
			return m
		}
	}
	return nil
}

// signIn stamps a session with an identity (called under mu).
func (s *Server) signIn(sid string, u record) {
	if ses := s.sessions[sid]; ses != nil {
		ses.actor = toStr(u["username"])
		ses.role = toStr(u["role"])
		ses.verified = truthy(u["verified"])
		ses.pendingMFA = ""
	}
}

func (s *Server) authSignup(w http.ResponseWriter, sid, username, password string) {
	if username == "" || password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "could not create account", http.StatusInternalServerError)
		return
	}
	verifyToken := randomToken(24)

	s.mu.Lock()
	if s.findUser(username) != nil {
		s.mu.Unlock()
		http.Error(w, "username is taken", http.StatusConflict)
		return
	}
	role := roleMember
	if len(s.entities[reservedUserEntity]) == 0 {
		role = roleAdmin // the first account to sign up owns the app
	}
	s.nextID[reservedUserEntity]++
	row := record{
		"id": s.nextID[reservedUserEntity], "username": username,
		"password": string(hash), "role": role, "email": "",
		"verified": false, "verifyToken": hashToken(verifyToken),
		"resetToken": "", "resetExpires": 0, "mfaSecret": "", "mfaEnabled": false,
	}
	s.entities[reservedUserEntity] = append(s.entities[reservedUserEntity], row)
	s.persist(s.store.Save(reservedUserEntity, row))
	s.signIn(sid, row)
	s.mu.Unlock()

	s.persistSession(sid)
	s.recordAudit(username, "signup", true, "")
	// The verify token would be emailed in production; surfaced here for the flow.
	writeJSON(w, map[string]any{"reload": true, "verifyToken": verifyToken})
}

func (s *Server) authLogin(w http.ResponseWriter, sid, username, password string) {
	if s.lockout.locked(username) {
		s.recordAudit(username, "login", false, "locked out")
		http.Error(w, "account temporarily locked after too many attempts", http.StatusTooManyRequests)
		return
	}
	s.mu.Lock()
	u := s.findUser(username)
	s.mu.Unlock()
	if u == nil || bcrypt.CompareHashAndPassword([]byte(toStr(u["password"])), []byte(password)) != nil {
		s.lockout.fail(username)
		s.recordAudit(username, "login", false, "bad credentials")
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	s.lockout.reset(username)

	// Password is correct. If MFA is enrolled, demand the second factor instead of
	// signing in now: the session remembers the pending user.
	if truthy(u["mfaEnabled"]) {
		s.mu.Lock()
		if ses := s.sessions[sid]; ses != nil {
			ses.pendingMFA = username
		}
		s.mu.Unlock()
		s.persistSession(sid)
		s.recordAudit(username, "login", true, "mfa required")
		writeJSON(w, map[string]any{"mfa": true})
		return
	}
	s.mu.Lock()
	s.signIn(sid, u)
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(username, "login", true, "")
	reloadResponse(w)
}

// authLoginMFA completes a login that required a second factor.
func (s *Server) authLoginMFA(w http.ResponseWriter, sid, username, code string) {
	if s.lockout.locked(username) {
		http.Error(w, "account temporarily locked after too many attempts", http.StatusTooManyRequests)
		return
	}
	s.mu.Lock()
	ses := s.sessions[sid]
	pending := ses != nil && ses.pendingMFA == username
	u := s.findUser(username)
	s.mu.Unlock()
	if !pending || u == nil || !truthy(u["mfaEnabled"]) || !totpValid(toStr(u["mfaSecret"]), code, time.Now()) {
		s.lockout.fail(username)
		s.recordAudit(username, "loginMFA", false, "bad code")
		http.Error(w, "invalid code", http.StatusUnauthorized)
		return
	}
	s.lockout.reset(username)
	s.mu.Lock()
	s.signIn(sid, u)
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(username, "loginMFA", true, "")
	reloadResponse(w)
}

func (s *Server) authLogout(w http.ResponseWriter, sid string) {
	s.mu.Lock()
	actor := ""
	if ses := s.sessions[sid]; ses != nil {
		actor = ses.actor
		ses.actor, ses.role, ses.verified, ses.pendingMFA = roleGuest, roleGuest, false, ""
	}
	s.mu.Unlock()
	s.persistSession(sid)
	s.recordAudit(actor, "logout", true, "")
	reloadResponse(w)
}

// authSetRole is RBAC management: an admin assigns another user's role.
func (s *Server) authSetRole(w http.ResponseWriter, sid, username, role string) {
	if role != roleAdmin && role != roleMember {
		http.Error(w, "role must be admin or member", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	caller := s.sessions[sid]
	if caller == nil || caller.role != roleAdmin {
		s.mu.Unlock()
		s.recordAudit(callerName(caller), "setRole", false, "not admin")
		http.Error(w, "forbidden: admin only", http.StatusForbidden)
		return
	}
	u := s.findUser(username)
	if u == nil {
		s.mu.Unlock()
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}
	u["role"] = role
	s.persist(s.store.Save(reservedUserEntity, u))
	// reflect the change in any live session belonging to that user.
	for _, ses := range s.sessions {
		if ses.actor == username {
			ses.role = role
		}
	}
	admin := caller.actor
	s.mu.Unlock()
	s.recordAudit(admin, "setRole", true, username+" -> "+role)
	writeJSON(w, map[string]any{"ok": true})
}

// authRequestReset issues a one-time password-reset token for a username.
func (s *Server) authRequestReset(w http.ResponseWriter, username string) {
	token := randomToken(24)
	s.mu.Lock()
	u := s.findUser(username)
	if u != nil {
		u["resetToken"] = hashToken(token)
		u["resetExpires"] = int(time.Now().Add(resetTTL).Unix())
		s.persist(s.store.Save(reservedUserEntity, u))
	}
	s.mu.Unlock()
	s.recordAudit(username, "requestReset", u != nil, "")
	// Always reply ok (do not reveal whether the user exists). The token would be
	// emailed in production; it is included here only when the user exists.
	out := map[string]any{"ok": true}
	if u != nil {
		out["resetToken"] = token
	}
	writeJSON(w, out)
}

// authResetPassword consumes a reset token and sets a new password.
func (s *Server) authResetPassword(w http.ResponseWriter, username, token, password string) {
	if password == "" {
		http.Error(w, "a new password is required", http.StatusBadRequest)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "could not reset password", http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	u := s.findUser(username)
	ok := u != nil && tokenEqual(token, toStr(u["resetToken"])) && int64(toInt(u["resetExpires"])) > time.Now().Unix()
	if ok {
		u["password"] = string(hash)
		u["resetToken"] = ""
		u["resetExpires"] = 0
		s.persist(s.store.Save(reservedUserEntity, u))
	}
	s.mu.Unlock()
	s.recordAudit(username, "resetPassword", ok, "")
	if !ok {
		http.Error(w, "invalid or expired reset token", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// authVerifyEmail confirms an account from its verification token.
func (s *Server) authVerifyEmail(w http.ResponseWriter, sid, token string) {
	s.mu.Lock()
	var verified record
	for _, row := range s.entities[reservedUserEntity] {
		m, ok := row.(record)
		if ok && tokenEqual(token, toStr(m["verifyToken"])) {
			m["verified"] = true
			m["verifyToken"] = ""
			s.persist(s.store.Save(reservedUserEntity, m))
			verified = m
			break
		}
	}
	// reflect into any live session for that user.
	if verified != nil {
		for _, ses := range s.sessions {
			if ses.actor == toStr(verified["username"]) {
				ses.verified = true
			}
		}
	}
	s.mu.Unlock()
	if verified == nil {
		s.recordAudit("", "verifyEmail", false, "invalid token")
		http.Error(w, "invalid verification token", http.StatusBadRequest)
		return
	}
	s.recordAudit(toStr(verified["username"]), "verifyEmail", true, "")
	writeJSON(w, map[string]any{"ok": true, "reload": true})
}

// authEnableMFA begins TOTP enrollment for the signed-in user, returning the
// shared secret and provisioning URI to add to an authenticator app. MFA is not
// active until confirmMFA proves the user can generate a valid code.
func (s *Server) authEnableMFA(w http.ResponseWriter, sid string) {
	secret := newTOTPSecret()
	s.mu.Lock()
	ses := s.sessions[sid]
	if ses == nil || ses.actor == "" || ses.actor == roleGuest {
		s.mu.Unlock()
		http.Error(w, "sign in first", http.StatusForbidden)
		return
	}
	u := s.findUser(ses.actor)
	if u == nil {
		s.mu.Unlock()
		http.Error(w, "no such user", http.StatusNotFound)
		return
	}
	u["mfaSecret"] = secret // stored encrypted at rest (@secret column)
	u["mfaEnabled"] = false
	s.persist(s.store.Save(reservedUserEntity, u))
	actor := ses.actor
	s.mu.Unlock()
	s.recordAudit(actor, "enableMFA", true, "")
	writeJSON(w, map[string]any{"ok": true, "secret": secret, "otpauth": otpauthURI(s.ir.App, actor, secret)})
}

// authConfirmMFA activates MFA once the user proves a valid code.
func (s *Server) authConfirmMFA(w http.ResponseWriter, sid, code string) {
	s.mu.Lock()
	ses := s.sessions[sid]
	if ses == nil || ses.actor == "" || ses.actor == roleGuest {
		s.mu.Unlock()
		http.Error(w, "sign in first", http.StatusForbidden)
		return
	}
	u := s.findUser(ses.actor)
	ok := u != nil && totpValid(toStr(u["mfaSecret"]), code, time.Now())
	if ok {
		u["mfaEnabled"] = true
		s.persist(s.store.Save(reservedUserEntity, u))
	}
	actor := ses.actor
	s.mu.Unlock()
	s.recordAudit(actor, "confirmMFA", ok, "")
	if !ok {
		http.Error(w, "invalid code", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func callerName(ses *sessionState) string {
	if ses == nil {
		return ""
	}
	return ses.actor
}

func reloadResponse(w http.ResponseWriter) {
	writeJSON(w, map[string]any{"reload": true})
}

// argStr reads the i-th argument as text, or "" if absent.
func argStr(args []any, i int) string {
	if i < len(args) {
		return toStr(args[i])
	}
	return ""
}
