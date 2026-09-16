package registry

import (
	"sort"
	"strings"
	"testing"

	"facet/internal/ast"
)

// TestToolchainVersionCoversControls is a forcing function, not a feature
// test. ToolchainVersion drifted silently once already (see the comment on
// it): a control keyword (password/newpassword) shipped after the last time
// this constant moved, so a project's `"facet": ">=1.31.0"` manifest range
// was satisfied by a toolchain built before it could parse either one.
//
// This pins the exact set of ast.Controls keys as of the last bump. Add a
// control and forget everything else, and this fails with the new key's
// name — which is the point: the fix is not "update this list", it's "bump
// ToolchainVersion (and explain why, in its own comment) because a manifest
// range just became meaningless for this feature otherwise."
//
// This only catches ast.Controls. It does not catch a new expression
// function (ago/compact/commas' own class), a new node kind, or anything
// else the language can grow — it is a floor, not a guarantee.
func TestToolchainVersionCoversControls(t *testing.T) {
	known := map[string]bool{
		"textarea":    true,
		"checkbox":    true,
		"toggle":      true,
		"radio":       true,
		"password":    true,
		"newpassword": true,
	}
	var unknown []string
	for kind := range ast.Controls {
		if !known[kind] {
			unknown = append(unknown, kind)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("ast.Controls gained %s since ToolchainVersion was last bumped (%s) — "+
			"bump ToolchainVersion, explain why in its comment, then add %s to `known` here",
			strings.Join(unknown, ", "), ToolchainVersion, strings.Join(unknown, ", "))
	}
}
