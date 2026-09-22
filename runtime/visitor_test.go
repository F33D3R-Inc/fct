package runtime

import (
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

// `session` is the visitor key, not the cookie identifier: it is minted with
// the session, rides through rotateSession unchanged (the rotation that login
// and logout perform against fixation), and round-trips the shared store. A
// guest's cart keyed to it is therefore still theirs after they sign in.
func TestSessionKeySurvivesRotation(t *testing.T) {
	g, err := compile.String("app V:\n    state n: int = 0\n    view Home at \"/\":\n        text \"{n}\"\n")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	srv.mu.Lock()
	srv.ensureSession("cookie-1")
	before := srv.scope("cookie-1")["session"].(string)
	srv.mu.Unlock()
	if before == "" || before == "cookie-1" {
		t.Fatalf("session should be a minted key distinct from the cookie id, got %q", before)
	}

	fresh := srv.rotateSession(httptest.NewRecorder(), "cookie-1")
	if fresh == "cookie-1" {
		t.Fatal("rotation must issue a new cookie id")
	}
	srv.mu.Lock()
	after := srv.scope(fresh)["session"].(string)
	gone := srv.sessions["cookie-1"]
	srv.mu.Unlock()
	if gone != nil {
		t.Fatal("the old cookie id must be retired")
	}
	if after != before {
		t.Fatalf("session key changed across rotation: %q -> %q", before, after)
	}

	// The shared-store round trip keeps it too, and a pre-key row gets one.
	srv.mu.Lock()
	ps := persistedFromSession(srv.sessions[fresh])
	srv.mu.Unlock()
	if got := sessionFromPersisted(ps).visitor; got != before {
		t.Fatalf("persisted round trip changed the key: %q -> %q", before, got)
	}
	ps.Visitor = ""
	if got := sessionFromPersisted(ps).visitor; got == "" {
		t.Fatal("a rehydrated session from before the key existed must be given one")
	}

	// A cookieless caller has no session and therefore no key.
	srv.mu.Lock()
	none := srv.scope("")["session"]
	srv.mu.Unlock()
	if none != "" {
		t.Fatalf("no session should mean an empty key, got %v", none)
	}
}

// The tooling models each identity as its own browser for `session`.
func TestToolSessionsKeyByActor(t *testing.T) {
	g, err := compile.String("app V:\n    entity Line:\n        id: int\n        visitor: text\n    action add():\n        add Line { visitor: session }\n    derive mine: int = count(l in Line where l.visitor == session)\n    view Home at \"/\":\n        text \"{mine}\"\n")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	if _, err := srv.Run("ada", "member", true, "add", nil); err != nil {
		t.Fatal(err)
	}
	mine := func(actor string) any {
		e := srv.ir.Derives[0].Expr
		return srv.EvalExpr(e, actor, "member", true)
	}
	if got := mine("ada"); toInt(got) != 1 {
		t.Fatalf("ada should see her own line, got %v", got)
	}
	if got := mine("bob"); toInt(got) != 0 {
		t.Fatalf("bob must not see ada's line, got %v", got)
	}
}
