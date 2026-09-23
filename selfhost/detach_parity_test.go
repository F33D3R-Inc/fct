package selfhost

// `detach P(args)` through the self-hosted compiler (stmt.fct parses it,
// proc_lower.fct lowers it to the "$detach" intrinsic) against the real one:
// the same whole IR for a daemon that detaches a handler, and a refusal from
// both for a detach outside a daemon and for an unknown proc.

import (
	"strings"
	"testing"

	"facet/internal/compile"
)

func TestDetachParityWithTheCompiler(t *testing.T) {
	ts, root := loadDriverFileApp(t)
	want := driverWant(t, "testdata/detach_parity/ok.fct")
	got, perr := driverGot(t, ts, root, "testdata/detach_parity/ok.fct")
	if perr != "" {
		t.Fatalf("driver: %s", perr)
	}
	if diff := driverDiff(want, got); diff != "" {
		t.Fatalf("IR mismatch:\n%s", diff)
	}
	for file, msg := range map[string]string{
		"testdata/detach_parity/in_proc.fct": "detach is only valid inside a daemon body",
		"testdata/detach_parity/unknown.fct": `detach calls unknown proc "nowhere"`,
	} {
		if _, err := compile.File(file); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: the compiler said %v, want %q", file, err, msg)
		}
		_, perr := driverGot(t, ts, root, file)
		if !strings.Contains(perr, msg) {
			t.Errorf("%s: the driver said %q, want %q", file, perr, msg)
		}
	}
}
