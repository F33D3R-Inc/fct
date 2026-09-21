package selfhost

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"facet/internal/compile"
	"facet/runtime"
)

func loadBTreeApp(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := compile.File(filepath.Join("btree.fct"))
	if err != nil {
		t.Fatalf("compile selfhost/btree.fct: %v", err)
	}
	srv, err := runtime.NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// intSlice decodes a delta value into []int. The action/delta wire only
// reports a state field when it actually CHANGED from its own declared
// default, so a result that happens to equal that default (e.g.
// s3EmptiedRootKeys' declared `[]`) is simply absent from the JSON —
// nil here means "still the default," not "missing."
func intSlice(t *testing.T, v any) []int {
	t.Helper()
	if v == nil {
		return []int{}
	}
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a list, got %#v", v)
	}
	out := make([]int, len(raw))
	for i, x := range raw {
		f, ok := x.(float64)
		if !ok {
			t.Fatalf("expected a number at index %d, got %#v", i, x)
		}
		out[i] = int(f)
	}
	return out
}

// intField decodes a delta value into int, treating a nil (the field's
// computed value coincided with its own declared default) as that
// default explicitly, rather than a missing/error case.
func intField(v any, deflt int) int {
	f, ok := v.(float64)
	if !ok {
		return deflt
	}
	return int(f)
}

func assertIntSlice(t *testing.T, got []int, want []int, name string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}

// TestBTreeDemo cross-checks all three of btree.fct's own hand-traced
// scenarios (insert-causing-leaf-split, insert-causing-a-root-growing-a-
// level branch-split, and delete-causing-a-branch-to-leaf-collapse) by
// independently hand-tracing the same maxKeys=3 insert/delete sequence
// against btreeInsertInto/btreeRemoveFrom's actual algorithm (lower/upper
// bound routing, split-point = n/2, promote-the-split-point-key) and
// confirming this file's own documented expected shapes are what the
// algorithm as written actually produces — not just re-asserting the
// comment's claim.
func TestBTreeDemo(t *testing.T) {
	ts := loadBTreeApp(t)
	d := postJSON(t, ts, "runDemoBtree")

	// Scenario 1: insert 10,20,30,40,50 (values = key*10) into an empty
	// tree. 40 overflows the root leaf (4 > maxKeys=3): split at n/2=2
	// gives left=[10,20], right=[30,40], separator=30 (right's first
	// key), root becomes a branch. 50 then lands in the right leaf
	// (30<=50), which still has room.
	assertIntSlice(t, intSlice(t, d["s1RootKeys"]), []int{30}, "s1RootKeys")
	if n, _ := d["s1RootChildCount"].(float64); int(n) != 2 {
		t.Errorf("s1RootChildCount = %v, want 2", d["s1RootChildCount"])
	}
	assertIntSlice(t, intSlice(t, d["s1LeftLeafKeys"]), []int{10, 20}, "s1LeftLeafKeys")
	assertIntSlice(t, intSlice(t, d["s1RightLeafKeys"]), []int{30, 40, 50}, "s1RightLeafKeys")
	for field, want := range map[string]int{
		"s1Get10": 100, "s1Get30": 300, "s1Get50": 500, "s1Get25": -1, "s1Get60": -1,
	} {
		if got := intField(d[field], -1); got != want {
			t.Errorf("%s = %v, want %d", field, d[field], want)
		}
	}

	// Scenario 2: continuing with 60,70,80,90,100. The last insert (100)
	// overflows the rightmost leaf a third time, and promoting that
	// split's separator (90) into the root — which already holds 3
	// separators [30,50,70] — overflows the ROOT too, so the root itself
	// splits and the tree grows a third level: root keys=[70], 2
	// children; left child is a branch keys=[30,50] with 3 leaf children
	// ([10,20]/[30,40]/[50,60]); right child is a branch keys=[90] with 2
	// leaf children ([70,80]/[90,100]).
	assertIntSlice(t, intSlice(t, d["s2RootKeys"]), []int{70}, "s2RootKeys")
	if n, _ := d["s2RootChildCount"].(float64); int(n) != 2 {
		t.Errorf("s2RootChildCount = %v, want 2", d["s2RootChildCount"])
	}
	assertIntSlice(t, intSlice(t, d["s2LeftBranchKeys"]), []int{30, 50}, "s2LeftBranchKeys")
	assertIntSlice(t, intSlice(t, d["s2RightBranchKeys"]), []int{90}, "s2RightBranchKeys")
	for field, want := range map[string]int{
		"s2Get60": 600, "s2Get90": 900, "s2Get45": -1, "s2Get15": -1,
	} {
		if got := intField(d[field], -1); got != want {
			t.Errorf("%s = %v, want %d", field, d[field], want)
		}
	}

	// Scenario 3: starting fresh from scenario 1's 5-entry tree, delete(10)
	// shrinks the left leaf to [20]; delete(20) then empties it entirely.
	// Since it's children[0] of a branch holding exactly 1 separator, the
	// branch drops that separator+child (btreeRemoveFrom's slot==0 arm),
	// coming back as keys=[]/children=[rightLeaf] — which
	// btreeCollapseRoot immediately collapses straight down to the right
	// leaf itself: the root becomes a LEAF, not just a smaller branch.
	if isLeaf, _ := d["s3RootIsLeaf"].(bool); !isLeaf {
		t.Errorf("s3RootIsLeaf = %v, want true (branch-to-leaf collapse)", d["s3RootIsLeaf"])
	}
	assertIntSlice(t, intSlice(t, d["s3RootKeys"]), []int{30, 40, 50}, "s3RootKeys")
	for field, want := range map[string]int{
		"s3Get10": -1, "s3Get20": -1, "s3Get30": 300,
	} {
		if got := intField(d[field], -1); got != want {
			t.Errorf("%s = %v, want %d", field, d[field], want)
		}
	}
	assertIntSlice(t, intSlice(t, d["s3EmptiedRootKeys"]), []int{}, "s3EmptiedRootKeys")
	if unchanged, _ := d["s3DeleteMissingUnchanged"].(bool); !unchanged {
		t.Errorf("s3DeleteMissingUnchanged = %v, want true (deleting an absent key is a no-op)", d["s3DeleteMissingUnchanged"])
	}
}
