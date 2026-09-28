package runtime

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"facet/internal/compile"
)

// notifyStore records what a server publishes on the cluster bus and which
// visitors it ends in the shared session table, around a real memStore.
type notifyStore struct {
	Store
	mu       sync.Mutex
	notes    []string
	visitors []string
}

func (n *notifyStore) Notify(payload string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes = append(n.notes, payload)
	return nil
}

func (n *notifyStore) DeleteVisitorSessions(v string) error {
	n.mu.Lock()
	n.visitors = append(n.visitors, v)
	n.mu.Unlock()
	return n.Store.DeleteVisitorSessions(v)
}

func (n *notifyStore) published(substr string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.notes {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// A peer's announcement evicts the named sessions (by id, and by `session`
// key), an instance ignores its own, and a lost resume position evicts all.
func TestClusterEvictsEndedSessions(t *testing.T) {
	srv, _ := credentialServer(t)
	srv.mu.Lock()
	srv.sessions["s1"] = srv.newSession("ada", "member")
	srv.sessions["s2"] = srv.newSession("bo", "member")
	srv.sessions["s3"] = srv.newSession("cy", "member")
	srv.sessions["s4"] = srv.newSession("di", "member")
	v3 := srv.sessions["s3"].visitor
	srv.mu.Unlock()
	c := &cluster{srv: srv, instanceID: "me"}
	c.consume(strings.NewReader(
		"id: 1\ndata: {\"origin\":\"peer\",\"sessions\":[\"s1\"]}\n\n"+
			"id: 2\ndata: {\"origin\":\"me\",\"sessions\":[\"s2\"]}\n\n"+
			"id: 3\ndata: {\"origin\":\"peer\",\"visitors\":[\""+v3+"\"]}\n\n"), 0)
	srv.mu.Lock()
	_, has1 := srv.sessions["s1"]
	_, has2 := srv.sessions["s2"]
	_, has3 := srv.sessions["s3"]
	srv.mu.Unlock()
	if has1 || !has2 || has3 {
		t.Fatalf("after the bus: s1=%v (peer ended, want gone) s2=%v (own echo, want kept) s3=%v (peer revoked its key, want gone)", has1, has2, has3)
	}
	srv.evictCachedSessions(nil)
	srv.mu.Lock()
	_, has4 := srv.sessions["s4"]
	srv.mu.Unlock()
	if has4 {
		t.Fatal("a full eviction (lost resume position) kept a cached session")
	}
}

// Re-keying and revoking reach the whole cluster: the shared table loses the
// sessions (including one this instance never cached) and peers are told.
func TestRevokeAndRekeyPublishToCluster(t *testing.T) {
	srv, ts := credentialServer(t)
	ns := &notifyStore{Store: srv.store}
	srv.store = ns
	srv.cluster = &cluster{srv: srv, instanceID: "me"}

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	planted, _ := verifySigned(sidCookie(resp))
	req, _ := http.NewRequest("POST", ts.URL+"/api/accounts", strings.NewReader(`{"handle":"ada","password":"correct horse"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "fa_sid", Value: signValue(planted)})
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !ns.published(`"sessions":["` + planted + `"]`) {
		t.Fatalf("the re-keyed id was not announced to peers: %v", ns.notes)
	}

	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/sessions", `{"handle":"ada","password":"correct horse"}`)
	a := &apiClient{t: t, base: ts.URL, token: toStr(body["token"])}
	_, key, _ := a.do("GET", "/api/me/session", "")
	// A session with the same key that only a peer ever held: it is in the shared
	// table, not in this instance's cache.
	ns.Store.SaveSession("peer-only", &persistedSession{Actor: "ada", Visitor: "k-peer", Expires: time.Now().Add(time.Hour)})
	if code, _, _ := a.do("DELETE", "/api/sessions", `{"key":"k-peer"}`); code != http.StatusNoContent {
		t.Fatalf("revokeKey = %d", code)
	}
	if _, found, _ := ns.Store.LoadSession("peer-only"); found {
		t.Fatal("revoke left a peer's session in the shared table")
	}
	if !ns.published(`"visitors":["k-peer"]`) || len(ns.visitors) != 1 {
		t.Fatalf("revoke was not announced cluster-wide: notes %v, visitors %v", ns.notes, ns.visitors)
	}
	if toStr(key["token"]) == "" {
		t.Fatal("no session key")
	}
}

// A Bearer caller slides its signed-in session forward like a cookie does; an
// anonymous or guest read writes nothing.
func TestBearerSlidesSession(t *testing.T) {
	srv, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	token := toStr(body["token"])
	sid, _ := verifySigned(token)
	soon := time.Now().Add(time.Minute)
	srv.mu.Lock()
	srv.sessions[sid].expires = soon
	guest := srv.newSession("guest", "guest")
	guest.expires = soon
	srv.sessions["g1"] = guest
	count := len(srv.sessions)
	srv.mu.Unlock()

	me := &apiClient{t: t, base: ts.URL, token: token}
	if code, _, _ := me.do("GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("GET /api/me = %d", code)
	}
	(&apiClient{t: t, base: ts.URL, token: signValue("g1")}).do("GET", "/api/me", "")
	anon.do("GET", "/api/me", "")
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.sessions[sid].expires.After(soon.Add(time.Hour)) {
		t.Fatalf("a Bearer request did not slide the session (expires %v)", srv.sessions[sid].expires)
	}
	if !srv.sessions["g1"].expires.Equal(soon) {
		t.Fatal("a guest's read slid its session")
	}
	if len(srv.sessions) != count {
		t.Fatalf("unauthenticated reads created %d sessions", len(srv.sessions)-count)
	}
}

// A session ended between resolving a request and running its action (a
// peer's revoke evicting it here) is not minted back as a guest under the same
// id: that request runs as a transient guest, and the id stays dead — the next
// request with the same token is unauthenticated, not a signed-in-looking guest.
func TestEndedSessionIsNotMintedBackByAnAction(t *testing.T) {
	srv, ts := credentialServer(t)
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	token := toStr(body["token"])
	sid, _ := verifySigned(token)
	me := &apiClient{t: t, base: ts.URL, token: token}
	if code, _, _ := me.do("GET", "/api/me", ""); code != http.StatusOK {
		t.Fatalf("GET /api/me before the revoke = %d", code)
	}

	// The request resolved sid; the peer's revoke lands before its action runs.
	srv.store.DeleteSession(sid)
	srv.evictCachedSessions([]string{sid})
	_, _, status, _, _ := srv.runActionReply(sid, srv.byAction["whoami"], nil)
	if status != http.StatusForbidden {
		t.Fatalf("the in-flight action ran with status %d, want 403 (as a guest)", status)
	}
	srv.mu.Lock()
	_, cached := srv.sessions[sid]
	srv.mu.Unlock()
	if cached {
		t.Fatal("running an action under an ended id cached a session under it")
	}
	if code, _, _ := me.do("GET", "/api/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me after the revoke = %d, want 401", code)
	}
}

// A clustered instance's write-back of a session a peer has ended (a slide or
// a state save racing the peer's revoke) does not re-create it in the shared
// table: the ending wins, and the local copy is dropped too.
func TestSessionWriteBackDoesNotResurrect(t *testing.T) {
	srv, ts := credentialServer(t)
	srv.cluster = &cluster{srv: srv, instanceID: "me"}
	srv.store = &notifyStore{Store: srv.store}
	anon := &apiClient{t: t, base: ts.URL}
	_, body, _ := anon.do("POST", "/api/accounts", `{"handle":"ada","password":"correct horse"}`)
	token := toStr(body["token"])
	sid, _ := verifySigned(token)
	if _, found, _ := srv.store.LoadSession(sid); !found {
		t.Fatal("signup did not persist its session")
	}
	srv.mu.Lock()
	visitor := srv.sessions[sid].visitor
	srv.mu.Unlock()

	// A peer revokes the session: the shared row goes, the bus has not arrived.
	srv.store.DeleteVisitorSessions(visitor)
	srv.persistSession(sid)
	if _, found, _ := srv.store.LoadSession(sid); found {
		t.Fatal("a write-back re-created a session a peer had ended")
	}
	srv.mu.Lock()
	_, cached := srv.sessions[sid]
	srv.mu.Unlock()
	if cached {
		t.Fatal("the instance kept a session the shared table no longer has")
	}
	me := &apiClient{t: t, base: ts.URL, token: token}
	if code, _, _ := me.do("GET", "/api/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me after the peer's revoke = %d, want 401", code)
	}

	// A new session is still inserted, and a live one still written back.
	_, body, _ = anon.do("POST", "/api/sessions", `{"handle":"ada","password":"correct horse"}`)
	sid2, _ := verifySigned(toStr(body["token"]))
	srv.mu.Lock()
	srv.sessions[sid2].state = map[string]any{"k": "v"}
	srv.mu.Unlock()
	srv.persistSession(sid2)
	if ps, found, _ := srv.store.LoadSession(sid2); !found || toStr(ps.State["k"]) != "v" {
		t.Fatalf("a live session's write-back was lost (found %v)", found)
	}
}

// A peer's change reloads an entity into the working set the way boot does:
// a @softdelete row the peer archived is gone here too, and its id stays
// taken.
func TestRemoteChangeHidesArchivedRows(t *testing.T) {
	g, err := compile.String(`app S:
    entity Note @softdelete:
        id: int
        body: text
    action write(body: text):
        add Note { body: body }
    view V at "/":
        text "{count(Note)}"
`)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	for _, b := range []string{"a", "b"} {
		if _, err := srv.Run("ada", "member", true, "write", []any{b}); err != nil {
			t.Fatal(err)
		}
	}
	// The peer archived Note 2 and wrote Note 3 as archived in one go.
	srv.store.Save("Note", record{"id": 2, "body": "b", "archived": true})
	srv.store.Save("Note", record{"id": 3, "body": "c", "archived": true})
	srv.applyRemoteChange([]string{"Note"})
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if n := len(srv.entities["Note"]); n != 1 {
		t.Fatalf("working set after the peer's archive = %d rows, want 1", n)
	}
	if srv.nextID["Note"] != 3 {
		t.Fatalf("nextID = %d, want 3 (an archived row's id is still taken)", srv.nextID["Note"])
	}
}

// FACET_CLUSTER=1 configures an app's runtime. A host running a program
// (`facet exec` — the FacetQL engine, the fct runtime) is not that runtime:
// it never joins a cluster, so the variable meant for the program it runs
// cannot stop it from starting, while an in-memory app still refuses it.
func TestProgramHostIgnoresCluster(t *testing.T) {
	t.Setenv("FACET_CLUSTER", "1")
	g, err := compile.String("app P:\n    view V at \"/\":\n        text \"x\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewInMemory(g); err == nil || !strings.Contains(err.Error(), "FACET_CLUSTER=1 requires the facetql:// store") {
		t.Fatalf("an in-memory app with FACET_CLUSTER=1 = %v, want the refusal", err)
	}
	srv, err := NewProgram(g)
	if err != nil {
		t.Fatalf("a program host with FACET_CLUSTER=1: %v", err)
	}
	defer srv.Shutdown()
	if srv.cluster != nil {
		t.Fatal("a program host joined a cluster")
	}
}
