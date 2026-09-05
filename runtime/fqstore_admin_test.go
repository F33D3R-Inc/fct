package runtime

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"facet/internal/ir"
)

// Declaring FacetQL's secondary indexes is an admin operation — /admin/indexes is
// admin-only, because the shape of the access paths is operational detail and an
// app that had to know about them would be coupled to them (facetql
// api/routes.rs). Booting is not: before indexes existed, Init touched no admin
// endpoint at all, so an app whose FACETQL_TOKEN is a plain identity must still
// start. It briefly did not — Init took Migrate's 403 as fatal and every such
// deployment failed to boot on `list FacetQL indexes: … 403`.
//
// These tests pin the three outcomes that keep that distinction honest, without a
// live engine: the refusal is classified (not message-matched), boot survives it,
// and a refusal that is NOT about privilege still stops boot.

// fqAdminEnts is the app this file migrates: `post` (from sql_test.go) declares
// an index over `likes`, so Migrate has something real to reconcile.
var fqAdminEnts = []ir.Entity{post}

// fqTestStore builds an fqStore against a stub FacetQL served by h. The token is
// a plain (non-admin) identity — what the stub refuses on /admin/indexes.
func fqTestStore(t *testing.T, h http.HandlerFunc) *fqStore {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fqStore{c: newFQClient(srv.URL, "plain-identity-token"), ents: map[string]ir.Entity{}}
}

// fqAdminOnly is a FacetQL that admits reads but refuses the admin endpoints, the
// way it answers any identity that is not an Admin.
func fqAdminOnly(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/admin/"):
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "admin only")
		case r.URL.Path == "/nodes/query":
			io.WriteString(w, `{"nodes":[{"address":"Post:0000000000000000001","kind":"Post",`+
				`"data":"{\"id\":1,\"author\":\"ada\",\"likes\":3}"}],"next":""}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

// A 403 from the index endpoints is one specific fact — this identity is not an
// admin — and Migrate must say so as a value a caller can test, not as text.
// `facet migrate` surfaces it as the actionable failure it is; Init tolerates it.
func TestFQMigrateClassifiesTheAdminRefusal(t *testing.T) {
	s := fqTestStore(t, fqAdminOnly(t))
	_, err := s.Migrate(fqAdminEnts, true)
	if !errors.Is(err, errFQAdminOnly) {
		t.Fatalf("Migrate error = %v, want one matching errFQAdminOnly", err)
	}
	// The message names what to change and where; a 403 must not reach an
	// operator as an opaque low-level string.
	for _, want := range []string{"admin identity", "FACETQL_TOKEN", "/admin/indexes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Migrate error %q does not mention %q", err, want)
		}
	}
}

// Boot must not depend on a privilege boot never required: an identity that
// cannot reconcile indexes starts against whatever the operator has already
// declared, and says so once.
func TestFQInitBootsWithoutIndexAdmin(t *testing.T) {
	s := fqTestStore(t, fqAdminOnly(t))

	// The notice is guarded by a process-wide sync.Once (Init runs on every boot
	// and every dev reload), so this test owns the first refusal in the binary:
	// two Inits, one line.
	restore := fqCaptureStderr(t)
	rows, err := s.Init(fqAdminEnts)
	if err != nil {
		t.Fatalf("Init must boot without admin, got %v", err)
	}
	if _, err := s.Init(fqAdminEnts); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	warned := restore()

	if len(rows["Post"]) != 1 {
		t.Errorf("Init loaded %d Post row(s), want 1 — the app's data must still load", len(rows["Post"]))
	}
	if _, ok := s.ents["Post"]; !ok {
		t.Error("Init must still remember the entity definitions")
	}
	if n := strings.Count(warned, "indexes were not reconciled"); n != 1 {
		t.Errorf("stderr carried the notice %d time(s), want exactly 1:\n%s", n, warned)
	}
	if !strings.Contains(warned, "facet migrate") {
		t.Errorf("the notice must name the command that fixes it:\n%s", warned)
	}
}

// Only the privilege refusal is tolerated. A reconcile that actually broke is
// still fatal — booting on a half-declared index set would hide it.
func TestFQInitStillFailsWhenTheReconcileBreaks(t *testing.T) {
	s := fqTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	})
	_, err := s.Init(fqAdminEnts)
	if err == nil {
		t.Fatal("Init returned no error on a 500 from /admin/indexes")
	}
	if errors.Is(err, errFQAdminOnly) {
		t.Fatalf("a 500 must not be classified as the admin refusal: %v", err)
	}
}

// fqCaptureStderr redirects os.Stderr (where the runtime writes its operator
// notices) until the returned function is called, which restores it and reports
// what was written.
func fqCaptureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	return func() string {
		os.Stderr = orig
		w.Close()
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out)
	}
}
