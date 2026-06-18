package runtime

import (
	"encoding/json"
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// Built-in authentication. When an app declares `auth`, the runtime manages a
// users table (the reserved FacetUser entity) and provides three server actions —
// signup, login, logout — that the view calls like any other. Identity flows into
// every expression as `actor` (the username) and `role`. Passwords are stored as
// bcrypt hashes, never in plaintext, and the users table is hidden from the API.

const reservedUserEntity = "FacetUser"

func isAuthAction(name string) bool {
	return name == "signup" || name == "login" || name == "logout"
}

// runAuth executes a built-in auth action against the session and the user store
// and replies with {"reload": true} so the client reloads into its new identity
// (avoiding the need to make actor/role reactive).
func (s *Server) runAuth(w http.ResponseWriter, r *http.Request, action string, args []any) {
	sid := s.session(w, r)
	switch action {
	case "signup":
		s.authSignup(w, sid, argStr(args, 0), argStr(args, 1))
	case "login":
		s.authLogin(w, sid, argStr(args, 0), argStr(args, 1))
	case "logout":
		s.authLogout(w, sid)
	default:
		http.Error(w, "unknown auth action", http.StatusNotFound)
	}
}

func (s *Server) authSignup(w http.ResponseWriter, sid, username, password string) {
	if username == "" || password == "" {
		http.Error(w, "username and password are required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	users := s.entities[reservedUserEntity]
	for _, u := range users {
		if m, ok := u.(record); ok && toStr(m["username"]) == username {
			s.mu.Unlock()
			http.Error(w, "username is taken", http.StatusConflict)
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.mu.Unlock()
		http.Error(w, "could not create account", http.StatusInternalServerError)
		return
	}
	role := "member"
	if len(users) == 0 {
		role = "admin" // the first account to sign up owns the app
	}
	s.nextID[reservedUserEntity]++
	row := record{"id": s.nextID[reservedUserEntity], "username": username, "password": string(hash), "role": role}
	s.entities[reservedUserEntity] = append(users, row)
	s.persist(s.store.Save(reservedUserEntity, row))
	s.actors[sid] = username
	s.roles[sid] = role
	s.mu.Unlock()
	reloadResponse(w)
}

func (s *Server) authLogin(w http.ResponseWriter, sid, username, password string) {
	s.mu.Lock()
	var found record
	for _, u := range s.entities[reservedUserEntity] {
		if m, ok := u.(record); ok && toStr(m["username"]) == username {
			found = m
			break
		}
	}
	s.mu.Unlock()
	if found == nil || bcrypt.CompareHashAndPassword([]byte(toStr(found["password"])), []byte(password)) != nil {
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.actors[sid] = username
	s.roles[sid] = toStr(found["role"])
	s.mu.Unlock()
	reloadResponse(w)
}

func (s *Server) authLogout(w http.ResponseWriter, sid string) {
	s.mu.Lock()
	s.actors[sid] = "guest"
	s.roles[sid] = "guest"
	s.mu.Unlock()
	reloadResponse(w)
}

func reloadResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"reload": true})
}

// argStr reads the i-th argument as text, or "" if absent.
func argStr(args []any, i int) string {
	if i < len(args) {
		return toStr(args[i])
	}
	return ""
}
