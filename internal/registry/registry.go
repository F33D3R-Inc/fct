// Package registry resolves remote facet imports — `import "github.com/owner/repo"`
// — into local files the existing compile pipeline already understands.
//
// GitHub is the registry: there is no central server. A facet is a public (or
// private) repo of `.fct` files with a facet.json manifest and vX.Y.Z release
// tags. The resolver fetches a repo as an immutable tarball pinned to a commit
// SHA, caches it on disk content-addressed by that SHA, records the exact
// commit + tarball hash in a committed facet.lock, and hands back a path inside
// the cache. From there a remote facet is just files on disk, identical to a
// local module — so merge, dedup, cycle detection, and placement are unchanged.
//
// This is deliberately the Go-modules model: tags are versions, commits are
// immutable, the lock makes builds reproducible and offline after first fetch.
package registry

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ToolchainVersion is the running `facet` version, used to check a facet's
// `facet` manifest range and to stamp facet.lock. cmd/facet/main sets it from
// its own version var at startup; the default keeps tests self-contained.
var ToolchainVersion = "1.9.0"

// host is the only registry host this build understands. Adding another (e.g.
// gitlab.com) is a new prefix + a new client, not a rewrite — IsRemote and
// parseRemoteRef are the seams.
const host = "github.com/"

// Resolver carries the per-build registry state: the project root (where
// facet.lock lives), the loaded lock, the on-disk cache, and the HTTP endpoints
// (overridable in tests). dirty tracks whether Resolve added or changed an entry
// so Save only rewrites the lock when something actually moved.
type Resolver struct {
	projectDir   string
	lock         *Lock
	cacheDir     string
	http         *http.Client
	apiBase      string
	codeloadBase string
	token        string
	dirty        bool
}

// New builds a Resolver rooted at an entry file's project directory, loading
// facet.lock if it is present.
func New(projectDir string) (*Resolver, error) {
	lock, err := LoadLock(projectDir)
	if err != nil {
		return nil, err
	}
	cacheDir := os.Getenv("FACET_CACHE")
	if cacheDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cacheDir = filepath.Join(home, ".facet", "cache")
		} else {
			// No home (some sandboxes) — a local-only app never touches this; a
			// remote import still gets a usable, if ephemeral, cache.
			cacheDir = filepath.Join(os.TempDir(), "facet", "cache")
		}
	}
	return &Resolver{
		projectDir:   projectDir,
		lock:         lock,
		cacheDir:     cacheDir,
		http:         &http.Client{Timeout: 30 * time.Second},
		apiBase:      "https://api.github.com",
		codeloadBase: "https://codeload.github.com",
		token:        os.Getenv("FACET_GITHUB_TOKEN"),
	}, nil
}

// IsRemote reports whether an import string is a remote (github.com/...) ref.
// Everything else is a local filesystem path, preserving prior behavior exactly.
func IsRemote(ref string) bool { return strings.HasPrefix(ref, host) }

// RepoKey returns the canonical "github.com/owner/repo" key for a remote ref
// (dropping any subpath), and whether the ref was remote and well-formed.
func RepoKey(ref string) (string, bool) {
	if !IsRemote(ref) {
		return "", false
	}
	owner, repo, _, err := parseRemoteRef(ref)
	if err != nil {
		return "", false
	}
	return host + owner + "/" + repo, true
}

// Resolve turns an import string into a local absolute file path. A local ref is
// joined against the importing file's directory (unchanged behavior). A remote
// ref is fetched, cached, and pinned in the lock if needed, then resolved to a
// file inside the cache (the manifest entry file, or the requested subpath).
func (r *Resolver) Resolve(ref, fromDir string) (string, error) {
	if !IsRemote(ref) {
		p := ref
		if !filepath.IsAbs(p) {
			p = filepath.Join(fromDir, ref)
		}
		return filepath.Abs(p)
	}

	owner, repo, subpath, err := parseRemoteRef(ref)
	if err != nil {
		return "", err
	}
	key := host + owner + "/" + repo

	entry, ok := r.lock.Modules[key]
	if !ok {
		// First time we see this repo: resolve the latest release and pin it.
		entry, err = r.resolveAndPin(key, owner, repo, "latest")
		if err != nil {
			return "", err
		}
	}

	dir, err := r.ensureCached(key, owner, repo, entry)
	if err != nil {
		return "", err
	}
	if subpath != "" {
		return filepath.Join(dir, filepath.FromSlash(subpath)), nil
	}
	if entry.Main == "" {
		return "", fmt.Errorf("%s has no `main` in facet.json and no single root .fct — import a specific file (github.com/owner/repo/path.fct)", key)
	}
	return filepath.Join(dir, entry.Main), nil
}

// Add resolves a ref at a given selection form (exact tag, ^/~ range, latest,
// branch, or commit) and writes/updates its facet.lock entry. An explicit add
// overrides any existing pin, including across majors — the user is choosing.
func (r *Resolver) Add(ref, form string) error {
	owner, repo, err := r.repoOf(ref)
	if err != nil {
		return err
	}
	key := host + owner + "/" + repo
	delete(r.lock.Modules, key)
	_, err = r.resolveAndPin(key, owner, repo, form)
	return err
}

// Update re-resolves one locked repo to the latest allowed version, rewriting
// its pin.
func (r *Resolver) Update(ref string) error { return r.Add(ref, "latest") }

// UpdateAll re-resolves every locked repo to its latest release.
func (r *Resolver) UpdateAll() error {
	for key := range r.lock.Modules {
		owner, repo, err := r.repoOf(key)
		if err != nil {
			return err
		}
		delete(r.lock.Modules, key)
		if _, err := r.resolveAndPin(key, owner, repo, "latest"); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAll makes sure every locked module is present in the cache, fetching any
// that are missing. It is what `facet get` runs to prime a fresh clone.
func (r *Resolver) EnsureAll() error {
	for key, entry := range r.lock.Modules {
		owner, repo, err := r.repoOf(key)
		if err != nil {
			return err
		}
		if _, err := r.ensureCached(key, owner, repo, entry); err != nil {
			return err
		}
	}
	return nil
}

// Modules returns the resolved dependency set (read-only view) for reporting.
func (r *Resolver) Modules() map[string]Module { return r.lock.Modules }

// Vendor copies every resolved module into the project's facet_modules/
// directory so builds can run fully offline. Returns "<key>@<version>" for each
// vendored module. The resolver prefers a vendored copy over the global cache.
func (r *Resolver) Vendor() ([]string, error) {
	var done []string
	for key, entry := range r.lock.Modules {
		owner, repo, err := r.repoOf(key)
		if err != nil {
			return nil, err
		}
		src, err := r.ensureCached(key, owner, repo, entry)
		if err != nil {
			return nil, err
		}
		dst := r.vendorPath(key, entry.Commit)
		if src != dst {
			if err := copyTree(src, dst); err != nil {
				return nil, err
			}
		}
		done = append(done, key+"@"+entry.Version)
	}
	return done, nil
}

// copyTree recursively copies the regular files and directories under src into
// dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// Save writes facet.lock back if Resolve/Add/Update changed entries.
func (r *Resolver) Save() error {
	if !r.dirty {
		return nil
	}
	r.lock.Facet = ToolchainVersion
	if err := r.lock.Save(r.projectDir); err != nil {
		return err
	}
	r.dirty = false
	return nil
}

// resolveAndPin selects a concrete commit for a form, fetches + validates it,
// and records the lock entry. It is the only place that pins a new version.
func (r *Resolver) resolveAndPin(key, owner, repo, form string) (Module, error) {
	var version, commit string
	switch {
	case isHexSHA(form):
		version, commit = form, form // a commit is reproducible; no warning.
	case form == "" || form == "latest" || isTagForm(form):
		tags, err := r.listTags(owner, repo)
		if err != nil {
			return Module{}, err
		}
		tag, err := selectTag(tags, form)
		if err != nil {
			return Module{}, fmt.Errorf("%s: %w", key, err)
		}
		version, commit = tag.Name, tag.Commit
	default: // a branch name — reproducible only by the commit we pin now.
		sha, err := r.resolveRef(owner, repo, form)
		if err != nil {
			return Module{}, err
		}
		version, commit = form, sha
		fmt.Fprintf(os.Stderr, "facet: warning: %s pinned to branch %q — not reproducible; prefer a vX.Y.Z tag\n", key, form)
	}

	integrity, main, err := r.fetchValidate(key, owner, repo, commit)
	if err != nil {
		return Module{}, err
	}
	entry := Module{Version: version, Commit: commit, Integrity: integrity, Main: main}
	if err := r.pin(key, entry); err != nil {
		return Module{}, err
	}
	fmt.Fprintf(os.Stderr, "facet: resolved %s@%s\n", key, version)
	return entry, nil
}

// pin records a lock entry, enforcing the one-version-per-repo rule: importing
// the same repo at two different majors is a hard error.
func (r *Resolver) pin(key string, entry Module) error {
	if old, ok := r.lock.Modules[key]; ok {
		ov, ook := parseSemver(old.Version)
		nv, nook := parseSemver(entry.Version)
		if ook && nook && ov.major != nv.major {
			return fmt.Errorf("%s is required at %s and %s by different modules — pick one", key, old.Version, entry.Version)
		}
	}
	r.lock.Modules[key] = entry
	r.dirty = true
	return nil
}

// fetchValidate downloads a commit's tarball, caches it (if not already), checks
// the manifest, and resolves the entry file. It returns the tarball integrity
// and resolved main.
func (r *Resolver) fetchValidate(key, owner, repo, commit string) (integrity, main string, err error) {
	data, err := r.downloadTarball(owner, repo, commit)
	if err != nil {
		return "", "", err
	}
	integrity = computeIntegrity(data)
	cacheDir := r.cachePath(key, commit)
	if !dirExists(cacheDir) {
		if err := r.extractToCache(data, cacheDir); err != nil {
			return "", "", err
		}
	}
	manifest, err := readManifest(cacheDir)
	if err != nil {
		return "", "", err
	}
	if manifest != nil {
		if manifest.Name != "" && manifest.Name != key {
			return "", "", fmt.Errorf("%s declares name %q (expected %q)", key, manifest.Name, key)
		}
		if manifest.Facet != "" {
			tv, ok := parseSemver(ToolchainVersion)
			if ok {
				sat, rerr := satisfiesRange(manifest.Facet, tv)
				if rerr != nil {
					return "", "", fmt.Errorf("%s has an invalid `facet` range %q: %w", key, manifest.Facet, rerr)
				}
				if !sat {
					return "", "", fmt.Errorf("facet %s requires facet %s — upgrade the toolchain (have %s)", key, manifest.Facet, ToolchainVersion)
				}
			}
		}
	}
	main, err = resolveMain(cacheDir, manifest)
	if err != nil {
		return "", "", err
	}
	return integrity, main, nil
}

// ensureCached guarantees a module's files are on disk for a locked entry,
// preferring a vendored copy, then the cache, then a verified network fetch.
func (r *Resolver) ensureCached(key, owner, repo string, entry Module) (string, error) {
	if vp := r.vendorPath(key, entry.Commit); dirExists(vp) {
		return vp, nil
	}
	cacheDir := r.cachePath(key, entry.Commit)
	if dirExists(cacheDir) {
		if os.Getenv("FACET_VERIFY") == "1" {
			data, err := r.downloadTarball(owner, repo, entry.Commit)
			if err != nil {
				return "", err
			}
			if entry.Integrity != "" && computeIntegrity(data) != entry.Integrity {
				return "", fmt.Errorf("integrity check failed for %s@%s — refusing (the published bytes changed)", key, entry.Version)
			}
		}
		return cacheDir, nil
	}
	// Cache miss: fetch by the pinned commit and verify against the lock hash.
	data, err := r.downloadTarball(owner, repo, entry.Commit)
	if err != nil {
		return "", fmt.Errorf("%s@%s is not cached and the network is unavailable — run `facet get` while online (%v)", key, entry.Version, err)
	}
	if entry.Integrity != "" && computeIntegrity(data) != entry.Integrity {
		return "", fmt.Errorf("integrity check failed for %s@%s — refusing (the published bytes changed)", key, entry.Version)
	}
	if err := r.extractToCache(data, cacheDir); err != nil {
		return "", err
	}
	return cacheDir, nil
}

// cachePath is the content-addressed directory for a repo at a commit.
func (r *Resolver) cachePath(key, commit string) string {
	return filepath.Join(r.cacheDir, filepath.FromSlash(key), commit)
}

// vendorPath is the in-project mirror location used by `facet vendor` for fully
// offline/air-gapped builds.
func (r *Resolver) vendorPath(key, commit string) string {
	return filepath.Join(r.projectDir, "facet_modules", filepath.FromSlash(key), commit)
}

// repoOf parses owner/repo out of a ref or a lock key.
func (r *Resolver) repoOf(ref string) (owner, repo string, err error) {
	owner, repo, _, err = parseRemoteRef(ref)
	return owner, repo, err
}

// parseRemoteRef splits "github.com/owner/repo[/subpath...]" into its parts.
func parseRemoteRef(ref string) (owner, repo, subpath string, err error) {
	if !IsRemote(ref) {
		return "", "", "", fmt.Errorf("%q is not a github.com/ reference", ref)
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(ref, host), "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("remote import must be github.com/owner/repo[/path.fct], got %q", ref)
	}
	owner, repo = parts[0], parts[1]
	if len(parts) > 2 {
		subpath = strings.Join(parts[2:], "/")
	}
	return owner, repo, subpath, nil
}

// resolveMain picks a module's entry file: the manifest `main`, else main.fct,
// else the sole `.fct` in the repo root. An ambiguous root returns "" (the
// caller errors only if the repo root is imported without a subpath).
func resolveMain(dir string, manifest *Manifest) (string, error) {
	if manifest != nil && manifest.Main != "" {
		if fileExists(filepath.Join(dir, manifest.Main)) {
			return manifest.Main, nil
		}
		return "", fmt.Errorf("facet.json `main` is %q but that file is not in the module", manifest.Main)
	}
	if fileExists(filepath.Join(dir, "main.fct")) {
		return "main.fct", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var fcts []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
			fcts = append(fcts, e.Name())
		}
	}
	if len(fcts) == 1 {
		return fcts[0], nil
	}
	return "", nil
}

// readManifest loads a module's facet.json. A missing manifest is allowed (the
// entry file then falls back to main.fct / the sole root .fct).
func readManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "facet.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseManifest(b)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
