package main

import "testing"

// `facet lang` exists to never go stale the way wiki/Language-Reference.md
// did, so the one thing worth proving is that its source-derivation actually
// finds this repo's real compiler source and pulls genuine facts out of it —
// not a hand-typed stand-in that merely looks similar.
func TestLangDerivesFromRealCompilerSource(t *testing.T) {
	src, err := compilerSourceDir()
	if err != nil {
		t.Fatalf("compilerSourceDir: %v (expected to find this repo's own internal/ast next to the test binary)", err)
	}

	astFiles, err := parseGoDir(src + "/internal/ast")
	if err != nil {
		t.Fatalf("parseGoDir(internal/ast): %v", err)
	}
	parserFiles, err := parseGoDir(src + "/internal/parser")
	if err != nil {
		t.Fatalf("parseGoDir(internal/parser): %v", err)
	}

	kinds := set(nodeKinds(astFiles))
	for _, want := range []string{"Box", "Text", "Button", "Link", "Form", "Control"} {
		if !kinds[want] {
			t.Errorf("expected node kind %q to be derived from internal/ast, got %v", want, kinds)
		}
	}

	builtins := set(builtinNames(parserFiles))
	for _, want := range []string{"now", "abs", "upper", "trim"} {
		if !builtins[want] {
			t.Errorf("expected builtin %q to be derived from isBuiltinCall, got %v", want, builtins)
		}
	}
	// `count` is an aggregate over a range, a different grammar position from
	// a call-position builtin — isBuiltinCall does not claim it, so this
	// derivation must not invent it either.
	if builtins["count"] {
		t.Error("count is an aggregate, not an isBuiltinCall entry — derivation picked up something it shouldn't have")
	}

	mods := set(modifierNames(append(astFiles, parserFiles...)))
	for _, want := range []string{"@required", "@client", "@server"} {
		if !mods[want] {
			t.Errorf("expected modifier %q to be derived from source literals, got %v", want, mods)
		}
	}
}

// TestModifierPattern proves the shape check used to tell a real `@name`
// annotation literal apart from an unrelated string that merely starts with
// "@" (an email-shaped fixture value, say).
func TestModifierPattern(t *testing.T) {
	cases := map[string]bool{
		"@required": true,
		"@e2e":      true,
		"@":         false,
		"@ ":        false,
		"a@b.com":   false,
		"required":  false,
	}
	for in, want := range cases {
		if got := modifierPattern(in); got != want {
			t.Errorf("modifierPattern(%q) = %v, want %v", in, got, want)
		}
	}
}

func set(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
