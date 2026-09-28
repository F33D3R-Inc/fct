package selfhost

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The `facetql` operator CLI, both ways: each case runs `facetql <args>` (the
// Rust binary) and `facet facetql <args>` (fqcli.fct, as the toolchain ships
// it) with the same environment, stdin and working-directory layout, and
// insists on the same stdout, stderr and exit status.

type fqCliResult struct {
	stdout, stderr string
	code           int
}

func (r fqCliResult) String() string {
	return fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", r.code, r.stdout, r.stderr)
}

// fqCliRun runs one invocation in dir. HOME is dir, so the default data
// directory is dir/.facetql; dir itself is written as <DIR> in the result.
func fqCliRun(t *testing.T, which, dir, stdin string, env []string, args ...string) fqCliResult {
	t.Helper()
	var cmd *exec.Cmd
	if which == "rust" {
		cmd = exec.Command(fqFacetqlStart(t), args...)
	} else {
		cmd = exec.Command(fqServerFacet(t), append([]string{"facetql"}, args...)...)
	}
	cmd.Dir = dir
	// The toolchain's own cache (where `facet facetql` unpacks the engine)
	// is kept out of dir: it is not something the command did.
	cmd.Env = append([]string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "XDG_CACHE_HOME=" + fqCliCache(t)}, env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s %q: %v", which, args, err)
		}
		code = ee.ExitCode()
	}
	norm := func(s string) string { return strings.ReplaceAll(s, dir, "<DIR>") }
	return fqCliResult{stdout: norm(out.String()), stderr: norm(errOut.String()), code: code}
}

var (
	fqCliCacheOnce sync.Once
	fqCliCacheDir  string
)

func fqCliCache(t *testing.T) string {
	fqCliCacheOnce.Do(func() {
		d, err := os.MkdirTemp("", "fqcli-cache-")
		if err != nil {
			t.Fatal(err)
		}
		fqCliCacheDir = d
	})
	return fqCliCacheDir
}

type fqCliCase struct {
	args  []string
	env   []string
	stdin string
}

func fqCliCompare(t *testing.T, cases []fqCliCase) {
	t.Helper()
	fqRustReference(t)
	for _, c := range cases {
		c := c
		t.Run(strings.Join(c.args, " ")+"|"+strings.Join(c.env, ","), func(t *testing.T) {
			want := fqCliRun(t, "rust", t.TempDir(), c.stdin, c.env, c.args...)
			got := fqCliRun(t, "fct", t.TempDir(), c.stdin, c.env, c.args...)
			if got != want {
				t.Errorf("facetql %q\n=== facetql\n%s\n=== facet facetql\n%s", c.args, want, got)
			}
		})
	}
}

func fqCliArgs(line string) []string {
	if line == "" {
		return nil
	}
	return strings.Split(line, " ")
}

// TestFqCliCommandLineBothWays: what clap does with a command line before
// any command runs — help screens (long and short, `help <cmd>`, a group
// without its action), --version, and its usage errors: required arguments
// missing, unexpected arguments and values (with their tips), unknown
// commands (with the similar one), invalid numbers and choices, repeated
// options — plus the checks each command makes before it needs a server
// (argument validation, the token, the confirmation prompt, the URL) and
// the deprecated ENOCHIAN_* spellings' warnings.
func TestFqCliCommandLineBothWays(t *testing.T) {
	var cases []fqCliCase
	for _, l := range []string{
		"--help", "-h", "-V", "--version", "-V extra", "help", "help get", "help user create", "help index create",
		"help bogus", "user", "user help", "user help create", "index", "reference", "user bogus", "index crate",
		"bogus", "rout", "-x", "--bogus", "--data-di x", "--data-dir", "--port 5",
		"get", "get a b", "get a --wat", "get a --jso", "get a --url x --jso", "get --json=1 a", "get --json --json a",
		"get a --token", "get a -- -b", "get a -x",
		"put", "put a", "put a --public", "query", "query --kind K --limit x", "query --kind K --limit -1",
		"query --kind K --limit=", "query --kind K --limit 99999999999999999999999", "query --kind K --kind J",
		"reference create n --kind K --field f --parent-kind P --on-delete zap",
		"reference create n --kind K --field f --parent-kind P --on-delete cas",
		"reference create n --kind K --field f --parent-kind P --on-delete",
		"reference create", "index create", "index create n", "index drop", "reference drop",
		"user create", "user delete", "delete", "backup", "restore", "backup a b", "routes x", "stats --bogus",
		"start --port x", "start --port 70000", "start --bogus", "start extra", "start --port",
		"routes", "routes --json", "routes --json --json",
		// before the client: validation, then the token
		"get a", "stats", "user list", "index list --json", "get a/b --token t", "get a --token t --url http://127.0.0.1:1",
		"put a --kind K --data {\"a\":1 --token t", "put a --kind K --data 1 --token t --url http://127.0.0.1:1",
		"index create bad/name --kind K --field f --token t", "index create n.x --kind K --field f",
		"index create n --kind K --field f --unique --text --token t",
		"index create " + strings.Repeat("x", 65) + " --kind K --field f --token t",
		"index create " + strings.Repeat("x", 64) + " --kind K --field f",
		"user create a\tb --token t", "user create ab --admin", "reference create n --kind K --field f --parent-kind P --on-delete set-null",
		"delete a --yes", "index drop n --yes", "reference drop n --yes --token t --url http://127.0.0.1:1",
	} {
		cases = append(cases, fqCliCase{args: fqCliArgs(l)})
	}
	for _, c := range []string{"", "init", "start", "backup", "restore", "user", "user create", "user list", "user delete",
		"index", "index create", "index list", "index drop", "reference", "reference create", "reference list",
		"reference drop", "get", "put", "delete", "query", "stats", "routes"} {
		cases = append(cases, fqCliCase{args: append(fqCliArgs(c), "--help")}, fqCliCase{args: append(fqCliArgs(c), "-h")})
	}
	// help shows the environment's values; ENOCHIAN_* spellings warn
	cases = append(cases,
		fqCliCase{args: []string{"get", "--help"}, env: []string{"FACETQL_URL=http://db:1", "FACETQL_TOKEN=secret", "FACETQL_DATA_DIR=/d"}},
		fqCliCase{args: []string{"start", "-h"}, env: []string{"ENOCHIAN_PORT=9", "ENOCHIAN_DATA_DIR=/e"}},
		fqCliCase{args: []string{"routes"}, env: []string{"ENOCHIAN_TOKENS=a:b", "ENOCHIAN_MASTER_KEY=00"}},
		fqCliCase{args: []string{"get", "a"}, env: []string{"FACETQL_URL=", "FACETQL_TOKEN=t"}},
		fqCliCase{args: []string{"get", "a"}, env: []string{"FACETQL_TOKEN="}},
		fqCliCase{args: []string{"stats"}, env: []string{"FACETQL_TOKEN=t", "FACETQL_URL=http://127.0.0.1:1/"}},
		// the confirmation prompt: only y/yes (any case, trimmed) goes on
		fqCliCase{args: []string{"delete", "a"}, stdin: "n\n"},
		fqCliCase{args: []string{"delete", "a"}, stdin: ""},
		fqCliCase{args: []string{"delete", "a"}, stdin: " YES \n"},
		fqCliCase{args: []string{"user", "delete", "bob"}, stdin: "y\nmore\n"},
		fqCliCase{args: []string{"index", "drop", "n"}, stdin: "sure\n"},
		fqCliCase{args: []string{"reference", "drop", "n"}, stdin: "yes"},
		// start's port, from its flag or the environment, as clap reads it
		fqCliCase{args: []string{"start", "--port=+99999"}},
		fqCliCase{args: []string{"start", "--port=-1"}},
		fqCliCase{args: []string{"start"}, env: []string{"FACETQL_PORT=70000"}},
		fqCliCase{args: []string{"start"}, env: []string{"FACETQL_PORT=x"}},
		fqCliCase{args: []string{"start"}, env: []string{"FACETQL_PORT= 80"}},
		fqCliCase{args: nil, env: []string{"ENOCHIAN_PORT=-5", "ENOCHIAN_TOKENS=a:b"}},
	)
	fqCliCompare(t, cases)
}

// TestFqCliDataDirectoryBothWays: init, backup and restore — each run in
// its own directory laid out the same way, so what they print (relative
// paths) and what they leave behind can be compared.
func TestFqCliDataDirectoryBothWays(t *testing.T) {
	fqRustReference(t)
	layout := func(dir string) {
		for name, body := range map[string]string{
			"data/facetql.wal": "wal", "data/facetql.heap.000000.seg": "\x00\x01heap", "data/facetql.catalog.tmp": "x",
			"data/facetql.lock": "", "data/sub/ignored": "y", "full/facetql.wal": "other",
		} {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	steps := []fqCliCase{
		{args: []string{"init"}},
		{args: []string{"--data-dir", "made/deep", "init"}},
		{args: []string{"init", "--data-dir=made2"}},
		{args: []string{"init"}, env: []string{"FACETQL_DATA_DIR=viaenv"}},
		{args: []string{"init"}, env: []string{"ENOCHIAN_DATA_DIR=legacy"}},
		{args: []string{"--data-dir", "data", "backup", "bk"}},
		{args: []string{"backup", "bk2", "--data-dir", "data"}},
		{args: []string{"--data-dir", "missing", "backup", "bk3"}},
		{args: []string{"--data-dir", "fresh", "restore", "bk"}},
		{args: []string{"--data-dir", "full", "restore", "bk"}},
		{args: []string{"--data-dir", "fresh2", "restore", "nothing-here"}},
		{args: []string{"--data-dir", "data", "backup", "../outside-bk"}},
	}
	root := t.TempDir()
	rdir, fdir := filepath.Join(root, "rust", "w"), filepath.Join(root, "fct", "w")
	for _, d := range []string{rdir, fdir} {
		layout(d)
	}
	for _, s := range steps {
		want := fqCliRun(t, "rust", rdir, s.stdin, s.env, s.args...)
		got := fqCliRun(t, "fct", fdir, s.stdin, s.env, s.args...)
		if got != want {
			t.Errorf("facetql %q %v\n=== facetql\n%s\n=== facet facetql\n%s", s.args, s.env, want, got)
		}
	}
	// what they left behind is the same, byte for byte
	listing := func(dir string) string {
		var b strings.Builder
		_ = filepath.Walk(filepath.Dir(dir), func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(filepath.Dir(dir), p)
			if fi.IsDir() {
				fmt.Fprintf(&b, "%s/\n", rel)
				return nil
			}
			body, _ := os.ReadFile(p)
			fmt.Fprintf(&b, "%s %q\n", rel, body)
			return nil
		})
		return b.String()
	}
	if w, g := listing(rdir), listing(fdir); w != g {
		t.Errorf("the directories differ\n=== facetql\n%s\n=== facet facetql\n%s", w, g)
	}
}

var fqCliTokenRE = regexp.MustCompile(`(token: |"token": ")[A-Za-z0-9_\-]+`)

// TestFqCliClientBothWays: every client command against a running server —
// a fresh fct FacetQL for each side, driven through the same script, so the
// transcripts differ only where the server mints a token.
func TestFqCliClientBothWays(t *testing.T) {
	fqRustReference(t)
	script := [][]string{
		{"user", "create", "alice"}, {"user", "create", "root", "--admin", "--json"}, {"user", "list"}, {"user", "list", "--json"},
		{"user", "create", "alice"},
		{"put", "Client:1", "--kind", "Client", "--data", `{ "name" : "Acme", "n": 1.50 }`, "--public"},
		{"put", "Client:2", "--kind", "Client", "--data", `{"name":"Beta","n":2}`, "--json"},
		{"put", "Note:1", "--kind", "Note", "--data", `"text"`},
		{"get", "Client:1"}, {"get", "Client:2", "--json"}, {"get", "Missing:1"},
		{"query", "--kind", "Client"}, {"query", "--kind", "Client", "--limit", "1", "--desc", "--json"},
		{"query", "--kind", "Nothing"}, {"query", "--kind", "Client", "--order", "n", "--limit", "+01"},
		{"index", "create", "client_n", "--kind", "Client", "--field", "n"},
		{"index", "create", "client_u", "--kind", "Client", "--field", "name", "--unique"},
		{"index", "create", "client_t", "--kind", "Client", "--field", "name", "--text", "--json"},
		{"index", "list"}, {"index", "list", "--json"}, {"index", "create", "client_n", "--kind", "Other", "--field", "n"},
		{"reference", "create", "note_client", "--kind", "Note", "--field", "client", "--parent-kind", "Client", "--on-delete", "set-null"},
		{"reference", "create", "note_client2", "--kind", "Note", "--field", "c2", "--parent-kind", "Client", "--parent-field", "name", "--on-delete", "cascade", "--json"},
		{"reference", "list"}, {"reference", "list", "--json"},
		{"reference", "drop", "note_client2", "--yes"}, {"reference", "drop", "nope", "--yes"},
		{"index", "drop", "client_t", "--yes"}, {"index", "drop", "nope", "--yes"},
		{"stats"}, {"stats", "--json"},
		{"delete", "Note:1", "--yes"}, {"delete", "Note:1", "--yes"},
		{"user", "delete", "alice", "--yes"}, {"user", "delete", "nobody", "--yes"},
		{"get", "Client:1", "--token", "wrong"},
	}
	run := func(which string) string {
		dir := t.TempDir()
		p := fqStartProc(t, "fct", dir, "FACETQL_ENV=development", "FACETQL_TOKENS=tok:admin:admin")
		port := fqWaitListening(t, p)
		env := []string{fmt.Sprintf("FACETQL_URL=http://127.0.0.1:%d/", port), "FACETQL_TOKEN=tok"}
		var b strings.Builder
		for _, args := range script {
			r := fqCliRun(t, which, t.TempDir(), "", env, args...)
			fmt.Fprintf(&b, "## %s\n%s\n", strings.Join(args, " "), fqCliTokenRE.ReplaceAllString(r.String(), "${1}<TOKEN>"))
		}
		return b.String()
	}
	want, got := run("rust"), run("fct")
	if want != got {
		t.Errorf("transcripts differ:\n--- facetql\n%s\n--- facet facetql\n%s\n--- diff\n%s", want, got, fqFirstDiff(want, got))
	}
}
