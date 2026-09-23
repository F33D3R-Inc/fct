package runtime

import (
	"bytes"
	"strings"
	"testing"

	"facet/internal/compile"
)

// A service run by `facet exec` (RunDaemons) runs its `on start` jobs before
// its daemons, as a server's StartJobs does: the daemon sees the state the
// job set up, never the zero value.
func TestRunDaemonsRunsOnStartJobsFirst(t *testing.T) {
	g, err := compile.File("testdata/service_onstart.fct")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewInMemory(g)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	srv.SetStdio(strings.NewReader(""), &out, &errOut)
	if err := srv.RunDaemons(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "daemon saw: configured\n" {
		t.Fatalf("stdout = %q, want the on-start job's state", got)
	}
}
