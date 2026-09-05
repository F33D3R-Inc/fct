package lsp

import "testing"

// The advice has to reach the author, and the editor is the only place it can:
// there is no warning channel in the compiler. These two properties are what
// make it useful rather than decorative — it is a warning and not an error, and
// it survives a file that does not build on its own, which is what a component
// library is made of.
func TestAMissingAltIsAWarningAndSurvivesABuildError(t *testing.T) {
	// `use Nope(...)` does not resolve, so ir.Build fails — as it does for most
	// files in a component library, which are fragments.
	src := "app A:\n    view Home at \"/\":\n        box:\n            image \"/a.png\"\n            use Nope()\n"
	diags := diagnose(src)
	if len(diags) != 2 {
		t.Fatalf("got %d diagnostics, want the advice and the build error: %+v", len(diags), diags)
	}
	if diags[0].Severity != 2 {
		t.Errorf("the missing alt is severity %d, want 2 (Warning) — an error would stop the build", diags[0].Severity)
	}
	if diags[0].Range.Start.Line != 3 {
		t.Errorf("the advice is on line %d (0-based), want 3", diags[0].Range.Start.Line)
	}
	if diags[1].Severity != 1 {
		t.Errorf("the build error is severity %d, want 1 (Error)", diags[1].Severity)
	}
}

// An author who has said the picture is decorative has answered; a warning they
// cannot answer is one they turn off.
func TestAnExplicitlyDecorativeImageIsSilent(t *testing.T) {
	src := "app A:\n    view Home at \"/\":\n        box:\n            image \"/a.png\" alt \"\"\n"
	if diags := diagnose(src); len(diags) != 0 {
		t.Fatalf("got %+v, want no diagnostics", diags)
	}
}
