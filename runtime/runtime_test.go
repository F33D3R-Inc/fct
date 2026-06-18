package runtime

import (
	"os/exec"
	"strings"
	"testing"
)

// The client runtime's pure logic is unit-tested in JavaScript (fill_test.js for
// the render-body engine, reactive_test.js for the Brick-4 reactivity core). This
// bridges those under `go test ./...` so CI runs them too; it skips cleanly where
// node is unavailable (e.g. a Go-only dev box).
func TestJSUnitTests(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; run `node runtime/fill_test.js` and `node runtime/reactive_test.js` manually")
	}
	for _, f := range []string{"fill_test.js", "reactive_test.js"} {
		f := f
		t.Run(f, func(t *testing.T) {
			out, err := exec.Command("node", f).CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", f, err, out)
			}
			if !strings.Contains(string(out), "assertions passed") {
				t.Fatalf("%s did not report success:\n%s", f, out)
			}
		})
	}
}
