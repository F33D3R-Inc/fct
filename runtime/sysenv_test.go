package runtime

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

func TestEnvVarAndRandomBytes(t *testing.T) {
	src := "app E:\n" +
		"    proc main(args: [text]) -> int uses io.env, io.console:\n" +
		"        let v = envVar(\"FACET_TEST_ENV_VALUE\")\n" +
		"        let missing = envVar(\"FACET_TEST_ENV_UNSET_XYZ\")\n" +
		"        let a = randomBytes(16)\n" +
		"        let b = randomBytes(16)\n" +
		"        let mut same = 0\n" +
		"        let mut i = 0\n" +
		"        loop i < 16:\n" +
		"            if a[i] == b[i]:\n" +
		"                same = same + 1\n" +
		"            i = i + 1\n" +
		"        let w = writeStdout(v + \"|\" + missing + \"|\" + len(a) + \"|\" + len(randomBytes(0)))\n" +
		"        return same\n"
	g, err := compile.String(src)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FACET_TEST_ENV_VALUE", "configured")
	var out bytes.Buffer
	srv.SetStdio(strings.NewReader(""), &out, &bytes.Buffer{})
	same, err := srv.RunMain(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "configured||16|0" {
		t.Fatalf("stdout = %q", got)
	}
	if same == 16 {
		t.Fatal("two randomBytes(16) draws were identical")
	}
	if _, err := randomBytes(1 << 20); err == nil {
		t.Fatal("an oversized randomBytes was accepted")
	}
}

func TestEnvVarNeedsEnvCapability(t *testing.T) {
	_, err := compile.String("app E:\n    proc f() -> text:\n        return envVar(\"HOME\")\n")
	if err == nil || !strings.Contains(err.Error(), "io.env") {
		t.Fatalf("want a missing io.env capability error, got %v", err)
	}
}
