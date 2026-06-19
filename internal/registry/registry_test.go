package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsRemote(t *testing.T) {
	cases := []struct {
		ref  string
		want bool
	}{
		{"github.com/acme/dislike", true},
		{"github.com/acme/audit/trail.fct", true},
		{"posts.fct", false},
		{"./shared/auth.fct", false},
		{"/abs/path.fct", false},
		{"gitlab.com/acme/x", false},     // only github.com is remote (for now)
		{"Github.com/acme/x", false},     // case-sensitive
		{"notgithub.com/acme/x", false},  // must start with the host prefix
		{"github.com.evil.com/x", false}, // "github.com." is not "github.com/"
	}
	for _, c := range cases {
		if got := IsRemote(c.ref); got != c.want {
			t.Errorf("IsRemote(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

func TestParseRemoteRef(t *testing.T) {
	owner, repo, sub, err := parseRemoteRef("github.com/acme/audit/trail.fct")
	if err != nil || owner != "acme" || repo != "audit" || sub != "trail.fct" {
		t.Fatalf("got %q/%q/%q err=%v", owner, repo, sub, err)
	}
	owner, repo, sub, err = parseRemoteRef("github.com/acme/dislike")
	if err != nil || owner != "acme" || repo != "dislike" || sub != "" {
		t.Fatalf("got %q/%q/%q err=%v", owner, repo, sub, err)
	}
	if _, _, _, err := parseRemoteRef("github.com/acme"); err == nil {
		t.Fatal("expected error for missing repo")
	}
}

func TestSelectTag(t *testing.T) {
	tags := []Tag{
		{Name: "v1.0.0", Commit: "a"},
		{Name: "v1.2.0", Commit: "b"},
		{Name: "v1.3.1", Commit: "c"},
		{Name: "v2.0.0", Commit: "d"},
		{Name: "nightly", Commit: "e"}, // non-semver, ignored
	}
	check := func(form, wantCommit string) {
		t.Helper()
		got, err := selectTag(tags, form)
		if err != nil {
			t.Fatalf("selectTag(%q): %v", form, err)
		}
		if got.Commit != wantCommit {
			t.Errorf("selectTag(%q) = %s, want commit %s", form, got.Commit, wantCommit)
		}
	}
	check("", "d")       // latest overall
	check("latest", "d") // latest overall
	check("v1.2.0", "b") // exact
	check("^1.0.0", "c") // highest in major 1
	check("~1.2.0", "b") // highest in 1.2.x
	check("^2.0.0", "d") // highest in major 2

	if _, err := selectTag(tags, "v9.9.9"); err == nil {
		t.Error("expected error for missing exact tag")
	}
	if _, err := selectTag([]Tag{{Name: "nightly", Commit: "x"}}, "latest"); err == nil {
		t.Error("expected error when no semver tags exist")
	}
}

func TestSatisfiesRange(t *testing.T) {
	v := semver{1, 4, 0}
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{">=1.4.0", true},
		{">=1.5.0", false},
		{"^1.0.0", true},
		{"^2.0.0", false},
		{"~1.4.0", true},
		{"~1.3.0", false},
		{">=1.0.0 <2.0.0", true},
		{">=1.0.0 <1.4.0", false},
		{"1.4.0", true},
	} {
		got, err := satisfiesRange(tc.expr, v)
		if err != nil {
			t.Fatalf("satisfiesRange(%q): %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("satisfiesRange(%q, 1.4.0) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := &Lock{
		LockfileVersion: 1,
		Facet:           "1.4.0",
		Modules: map[string]Module{
			"github.com/acme/dislike": {Version: "v1.2.0", Commit: "abc", Integrity: "sha256-xyz", Main: "dislike.fct"},
			"github.com/acme/audit":   {Version: "v0.9.0", Commit: "def", Integrity: "sha256-uvw", Main: "trail.fct"},
		},
	}
	if err := l.Save(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, lockfileName))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Modules) != 2 || loaded.Modules["github.com/acme/dislike"].Commit != "abc" {
		t.Fatalf("round-trip lost data: %+v", loaded)
	}
	if err := loaded.Save(dir); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, lockfileName))
	if !bytes.Equal(first, second) {
		t.Errorf("lock serialization is not stable:\n%s\n---\n%s", first, second)
	}
}

func TestLoadLockMissing(t *testing.T) {
	l, err := LoadLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if l.Modules == nil || len(l.Modules) != 0 {
		t.Fatalf("missing lock should be empty, got %+v", l)
	}
}

func TestExtractTarSlip(t *testing.T) {
	for _, bad := range []string{"../../evil.txt", "/etc/passwd", "sub/../../escape"} {
		tarball := makeTarGz("repo-deadbeef", map[string]string{bad: "x"})
		err := extractTarGz(tarball, filepath.Join(t.TempDir(), "out"))
		if err == nil {
			t.Errorf("expected rejection for unsafe path %q", bad)
		}
	}
}

func TestExtractBombCap(t *testing.T) {
	tarball := makeTarGz("repo-deadbeef", map[string]string{"big.fct": strings.Repeat("a", 1000)})
	err := extractTarGzLimited(tarball, filepath.Join(t.TempDir(), "out"), 100)
	if err == nil || !strings.Contains(err.Error(), "decompression bomb") {
		t.Fatalf("expected bomb cap error, got %v", err)
	}
}

func TestExtractStripsTopDir(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	tarball := makeTarGz("dislike-abc123", map[string]string{
		"facet.json":  `{"name":"github.com/acme/dislike"}`,
		"dislike.fct": "app Dislike:\n",
	})
	if err := extractTarGz(tarball, dest); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dest, "dislike.fct")) {
		t.Error("expected dislike.fct at the cache root (top dir stripped)")
	}
}

// fakeGitHub stands in for api.github.com + codeload.github.com. It serves one
// repo's tags and tarball and counts tarball downloads so a test can assert the
// second build is offline.
func fakeGitHub(t *testing.T, owner, repo, sha string, files map[string]string) (*httptest.Server, *int) {
	t.Helper()
	tarball := makeTarGz(repo+"-"+sha, files)
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/tags", owner, repo), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "v1.0.0", "commit": map[string]string{"sha": sha}},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/%s/%s/tar.gz/%s", owner, repo, sha), func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(tarball)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestResolver(t *testing.T, projectDir, cacheDir, baseURL string) *Resolver {
	t.Helper()
	lock, err := LoadLock(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{
		projectDir:   projectDir,
		lock:         lock,
		cacheDir:     cacheDir,
		http:         &http.Client{},
		apiBase:      baseURL,
		codeloadBase: baseURL,
	}
}

func TestResolveAndCacheAndOffline(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	files := map[string]string{
		"facet.json":  `{"name":"github.com/acme/dislike","version":"1.0.0","main":"dislike.fct","facet":">=1.0.0"}`,
		"dislike.fct": "app Dislike:\n    state likes: int = 0\n",
	}
	srv, hits := fakeGitHub(t, "acme", "dislike", sha, files)
	proj := t.TempDir()
	cache := t.TempDir()

	res := newTestResolver(t, proj, cache, srv.URL)
	path, err := res.Resolve("github.com/acme/dislike", proj)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "dislike.fct" || !fileExists(path) {
		t.Fatalf("resolved path is wrong: %q", path)
	}
	if err := res.Save(); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("expected 1 tarball download, got %d", *hits)
	}

	// The lock must now pin the repo.
	lock, _ := LoadLock(proj)
	entry, ok := lock.Modules["github.com/acme/dislike"]
	if !ok || entry.Commit != sha || entry.Version != "v1.0.0" || entry.Main != "dislike.fct" {
		t.Fatalf("lock not pinned correctly: %+v", lock.Modules)
	}
	if !strings.HasPrefix(entry.Integrity, "sha256-") {
		t.Fatalf("missing integrity: %q", entry.Integrity)
	}

	// A fresh resolver with the committed lock + warm cache must not hit network.
	res2 := newTestResolver(t, proj, cache, srv.URL)
	if _, err := res2.Resolve("github.com/acme/dislike", proj); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("second build should be offline; tarball downloads = %d", *hits)
	}
}

func TestResolveSubpath(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	files := map[string]string{
		"facet.json": `{"name":"github.com/acme/audit","version":"1.0.0","main":"main.fct"}`,
		"main.fct":   "app Audit:\n",
		"trail.fct":  "app Trail:\n",
	}
	srv, _ := fakeGitHub(t, "acme", "audit", sha, files)
	proj := t.TempDir()
	res := newTestResolver(t, proj, t.TempDir(), srv.URL)
	path, err := res.Resolve("github.com/acme/audit/trail.fct", proj)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "trail.fct" {
		t.Fatalf("subpath import resolved to %q", path)
	}
}

func TestIntegrityMismatch(t *testing.T) {
	const sha = "3333333333333333333333333333333333333333"
	files := map[string]string{
		"facet.json":  `{"name":"github.com/acme/dislike","main":"dislike.fct"}`,
		"dislike.fct": "app Dislike:\n",
	}
	srv, _ := fakeGitHub(t, "acme", "dislike", sha, files)
	proj := t.TempDir()
	res := newTestResolver(t, proj, t.TempDir(), srv.URL)
	// Pre-seed the lock with a tampered integrity for the exact commit.
	res.lock.Modules["github.com/acme/dislike"] = Module{
		Version: "v1.0.0", Commit: sha, Integrity: "sha256-WRONG", Main: "dislike.fct",
	}
	_, err := res.Resolve("github.com/acme/dislike", proj)
	if err == nil || !strings.Contains(err.Error(), "integrity check failed") {
		t.Fatalf("expected integrity error, got %v", err)
	}
}

func TestNameMismatchRejected(t *testing.T) {
	const sha = "4444444444444444444444444444444444444444"
	files := map[string]string{
		"facet.json": `{"name":"github.com/someone/else","main":"x.fct"}`,
		"x.fct":      "app X:\n",
	}
	srv, _ := fakeGitHub(t, "acme", "dislike", sha, files)
	proj := t.TempDir()
	res := newTestResolver(t, proj, t.TempDir(), srv.URL)
	_, err := res.Resolve("github.com/acme/dislike", proj)
	if err == nil || !strings.Contains(err.Error(), "declares name") {
		t.Fatalf("expected name-mismatch error, got %v", err)
	}
}

func TestToolchainTooOld(t *testing.T) {
	const sha = "5555555555555555555555555555555555555555"
	files := map[string]string{
		"facet.json":  `{"name":"github.com/acme/dislike","main":"dislike.fct","facet":">=99.0.0"}`,
		"dislike.fct": "app Dislike:\n",
	}
	srv, _ := fakeGitHub(t, "acme", "dislike", sha, files)
	proj := t.TempDir()
	res := newTestResolver(t, proj, t.TempDir(), srv.URL)
	_, err := res.Resolve("github.com/acme/dislike", proj)
	if err == nil || !strings.Contains(err.Error(), "requires facet") {
		t.Fatalf("expected toolchain-version error, got %v", err)
	}
}

func TestLocalRefUnchanged(t *testing.T) {
	proj := t.TempDir()
	res := newTestResolver(t, proj, t.TempDir(), "http://127.0.0.1:0")
	got, err := res.Resolve("posts.fct", proj)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(proj, "posts.fct") {
		t.Fatalf("local ref resolved to %q", got)
	}
}

func makeTarGz(prefix string, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: prefix + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name:     prefix + "/" + name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(content)),
		})
		_, _ = tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}
