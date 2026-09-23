package registry

import (
	"sort"
	"strings"
	"testing"

	"facet/internal/ast"
	"facet/internal/parser"
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

// TestToolchainVersionCoversBuiltins is TestToolchainVersionCoversControls for
// the builtin table (internal/parser/builtins.go): a builtin added — or moved
// to a different site — without bumping ToolchainVersion makes a manifest's
// `facet` range claim a toolchain that cannot compile the program. The fix is
// to bump ToolchainVersion (explaining why in its comment), then update this
// list.
func TestToolchainVersionCoversBuiltins(t *testing.T) {
	known := map[string]parser.BuiltinSite{}
	for _, n := range strings.Fields(`abs ago byteLen charAt commas compact contains day first floor fromIso fromJson
		iso join len lower max min money month now rand replace round slice slug split take toFloat toInt toMoney trim upper year`) {
		known[n] = parser.SiteEverywhere
	}
	for _, n := range strings.Fields(`canonicalJson ecdsaP256Verify ed25519Verify fileDigest floatBits floatFromBits formatIn
		fromLocal given print randomToken sha256Hex shuffleOrder totpSecret totpValid verifyPassword zoneValid
		u64Cmp u64Min u64Max u64SatSub u64Div u64Rem u64Text u64Parse u64ParseError u64ToFloat`) {
		known[n] = parser.SiteAuthority
	}
	for _, n := range strings.Fields(`accept aesGcmAuthentic aesGcmOpen aesGcmSeal append appendFile bytes bytesToText channel
		closeConn connError connOpen connect envSet envVar fileExists fileSize httpGet httpPost listen listenOn monoMs nowMs signals
		awaitAny closeChannel exitProcess processStats listenTls connPeer connectTls
		closeListener listenError grantRead
		pollBytes randomBytes readBytes readFile readFileAt readStdin recv removeFile renameFile send setTimeoutMs
		shutdownConn sleepMs syncFile textToBytes truncateFile writeBytes writeFile writeFileAt writeStderr writeStdout`) {
		known[n] = parser.SiteProc
	}
	var changed []string
	for n, want := range known {
		if got, ok := parser.BuiltinSiteOf(n); !ok || got != want {
			changed = append(changed, n)
		}
	}
	for _, n := range parser.Builtins() {
		if _, ok := known[n]; !ok {
			changed = append(changed, n)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		t.Fatalf("the builtin table changed (%s) since ToolchainVersion was last bumped (%s) — "+
			"bump ToolchainVersion, explain why in its comment, then update `known` here",
			strings.Join(changed, ", "), ToolchainVersion)
	}
}
