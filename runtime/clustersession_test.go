package runtime

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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
