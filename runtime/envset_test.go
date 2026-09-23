package runtime

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"facet/internal/compile"
)

func TestEnvSetTellsUnsetFromEmpty(t *testing.T) {
	t.Setenv("FACET_TEST_ENVSET_EMPTY", "")
	t.Setenv("FACET_TEST_ENVSET_VALUE", "v")
	g, err := compile.File("testdata/envset.fct")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for name, want := range map[string]string{
		"FACET_TEST_ENVSET_EMPTY":         "true|",
		"FACET_TEST_ENVSET_VALUE":         "true|v",
		"FACET_TEST_ENVSET_UNSET_XYZ_123": "false|",
	} {
		d := postJSON(t, ts, "check", fmt.Sprintf(`{"args":[%q]}`, name))
		if got := fmt.Sprint(d["result"]); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}
