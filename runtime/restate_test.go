package runtime

import (
	"strings"
	"testing"
	"time"

	"facet/internal/compile"
)

// `restate actor A role R` re-roles every live session of A at once — this
// instance's cache, the shared session table (a session only a peer ever
// held), and peers, which are told to drop their cached copies — without
// signing anyone out; a policy checked on A's next request sees R.
func TestRestateReachesLiveSessions(t *testing.T) {
	src := `app R:
    entity Grant:
        id: int
        who: text
    policy admin:
        role == "admin"
    action promote(who: text):
        add Grant { who: who }
        restate actor who role "admin"
    action adminOnly() -> text:
        requires admin
        return "ok"
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
	ns := &notifyStore{Store: srv.store}
	srv.store = ns
	srv.cluster = &cluster{srv: srv, instanceID: "me"}
	srv.mu.Lock()
	srv.sessions["ada-phone"] = srv.newSession("ada", "member")
	srv.sessions["ada-laptop"] = srv.newSession("ada", "member")
	srv.sessions["bob"] = srv.newSession("bob", "member")
	srv.mu.Unlock()
	ns.Store.SaveSession("ada-peer", &persistedSession{Actor: "ada", Role: "member", Expires: time.Now().Add(time.Hour)})

	if _, err := srv.Run("ops", "admin", true, "promote", []any{"ada"}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	phone, laptop, bob := srv.sessions["ada-phone"].role, srv.sessions["ada-laptop"].role, srv.sessions["bob"].role
	srv.mu.Unlock()
	if phone != "admin" || laptop != "admin" || bob != "member" {
		t.Fatalf("roles after restate: phone %q laptop %q bob %q", phone, laptop, bob)
	}
	if ps, found, _ := ns.Store.LoadSession("ada-peer"); !found || ps.Role != "admin" {
		t.Fatalf("the shared table's peer-only session was not re-roled: %+v %v", ps, found)
	}
	if !ns.published(`"restated":["ada"]`) {
		t.Fatalf("peers were not told: %v", ns.notes)
	}
	// The policy now admits ada's existing session.
	_, value, _, status, msg := srv.runActionValue("ada-phone", srv.byAction["adminOnly"], nil)
	if status != 200 || value != "ok" {
		t.Fatalf("admin-only on the re-roled session = %d %s", status, msg)
	}

	// A peer's announcement drops only that actor's cached sessions.
	c := &cluster{srv: srv, instanceID: "other"}
	c.consume(strings.NewReader("id: 1\ndata: {\"origin\":\"me2\",\"restated\":[\"ada\"]}\n\n"), 0)
	srv.mu.Lock()
	_, hasPhone := srv.sessions["ada-phone"]
	_, hasBob := srv.sessions["bob"]
	srv.mu.Unlock()
	if hasPhone || !hasBob {
		t.Fatalf("peer restate: ada's cache kept=%v, bob's kept=%v", hasPhone, hasBob)
	}
}
