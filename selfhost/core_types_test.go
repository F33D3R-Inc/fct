package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func loadCoreTypesApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("core_types.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/core_types.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// Expected values cross-checked directly against facetql/src/core/
// {node,edge,user,coordinate,history}.rs: Node::can_read is
// `visibility == Public || owner == requester`, Node::can_write and
// Edge::can_write are both `owner == requester`, EdgeId drops owner,
// HistoryEntry::now's pure half passes address/version/timestamp through
// unchanged, and Coordinate::new performs no validation at all.
func TestCoreTypesDemo(t *testing.T) {
	ts := loadCoreTypesApp(t)
	d := postJSON(t, ts, "runDemoCoreTypes")

	cases := map[string]bool{
		"demoResult1":  true,  // alice (owner) can read her own private node
		"demoResult2":  false, // bob (non-owner) cannot read a private node
		"demoResult3":  true,  // bob can read the SAME node once it's public
		"demoResult4":  false, // bob cannot write alice's node regardless of visibility
		"demoResult5":  true,  // alice (creator) can remove her own edge
		"demoResult6":  false, // bob cannot remove an edge he doesn't own
		"demoResult7":  true,  // EdgeId == {from,to,kind} only, owner dropped
		"demoResult8":  true,  // a UserRecord with role "admin" reports isAdmin
		"demoResult9":  false, // a UserRecord with role "user" does not
		"demoResult10": true,  // HistoryEntry carries address/version/timestamp through
		"demoResult11": true,  // Coordinate::new stores out-of-byte-range axes unchanged
		"demoResult12": true,  // coordinateAxisInByteRange correctly gates 0..=255
	}
	for field, want := range cases {
		got, _ := d[field].(bool)
		if got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}
