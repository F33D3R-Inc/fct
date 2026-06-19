package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Facet talks to GitHub over HTTPS with the standard library only — no `git`
// binary, so the toolchain stays a single static executable. The only egress is
// to api.github.com (tags, ref→SHA) and codeload.github.com (immutable tarball
// downloads, pinned by commit SHA).

// maxExtractedSize caps the total bytes written when unpacking a facet tarball,
// a guard against a decompression bomb.
const maxExtractedSize = 64 << 20 // 64 MiB

// maxDownloadSize caps the compressed tarball read from codeload.
const maxDownloadSize = 128 << 20 // 128 MiB

// Tag is a published version: a tag name and the commit it points at.
type Tag struct {
	Name   string
	Commit string
}

// doRequest issues an authenticated GET with the project's headers, retrying a
// few times with backoff on transport errors and 5xx responses.
func (r *Resolver) doRequest(url, accept string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("User-Agent", "facet/"+ToolchainVersion)
		if r.token != "" {
			req.Header.Set("Authorization", "Bearer "+r.token)
		}
		resp, err := r.http.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("github returned status %d", resp.StatusCode)
		} else {
			return resp, nil
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	return nil, lastErr
}

// listTags returns every vX.Y.Z (and other) tag in a repo, following pagination.
func (r *Resolver) listTags(owner, repo string) ([]Tag, error) {
	var tags []Tag
	for page := 1; page <= 20; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100&page=%d", r.apiBase, owner, repo, page)
		resp, err := r.doRequest(url, "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if err := githubStatusError(resp.StatusCode, owner, repo); err != nil {
			return nil, err
		}
		var batch []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, fmt.Errorf("github tags: %w", err)
		}
		for _, t := range batch {
			tags = append(tags, Tag{Name: t.Name, Commit: t.Commit.SHA})
		}
		if len(batch) < 100 {
			break
		}
	}
	return tags, nil
}

// resolveRef turns a branch name (or any committish) into a commit SHA.
func (r *Resolver) resolveRef(owner, repo, ref string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", r.apiBase, owner, repo, ref)
	resp, err := r.doRequest(url, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	if err := githubStatusError(resp.StatusCode, owner, repo); err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commit); err != nil {
		return "", fmt.Errorf("github commit: %w", err)
	}
	if commit.SHA == "" {
		return "", fmt.Errorf("could not resolve %q in github.com/%s/%s", ref, owner, repo)
	}
	return commit.SHA, nil
}

// downloadTarball fetches the immutable gzip tarball for an exact commit.
func (r *Resolver) downloadTarball(owner, repo, commit string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/%s/tar.gz/%s", r.codeloadBase, owner, repo, commit)
	resp, err := r.doRequest(url, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if err := githubStatusError(resp.StatusCode, owner, repo); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("github download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadSize {
		return nil, fmt.Errorf("download for github.com/%s/%s exceeds %d bytes", owner, repo, maxDownloadSize)
	}
	return data, nil
}

// githubStatusError maps the well-known failure codes to actionable messages.
func githubStatusError(code int, owner, repo string) error {
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("facet github.com/%s/%s not found, or private — set FACET_GITHUB_TOKEN", owner, repo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("GitHub rate limit hit; set FACET_GITHUB_TOKEN")
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token (FACET_GITHUB_TOKEN) for github.com/%s/%s", owner, repo)
	default:
		return fmt.Errorf("github returned status %d for github.com/%s/%s", code, owner, repo)
	}
}

// computeIntegrity is the lockfile integrity string for a downloaded tarball.
func computeIntegrity(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// extractToCache unpacks a tarball into dest atomically: it extracts into a temp
// sibling directory, then renames into place so a reader never sees a partial
// module. A concurrent writer that wins the rename race is tolerated.
func (r *Resolver) extractToCache(data []byte, dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-extract-*")
	if err != nil {
		return err
	}
	if err := extractTarGz(data, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.RemoveAll(tmp)
		if dirExists(dest) {
			return nil // another build cached the same commit first — fine.
		}
		return err
	}
	return nil
}

// extractTarGz unpacks a gzip tarball into dest, capping total extracted bytes
// at maxExtractedSize.
func extractTarGz(data []byte, dest string) error {
	return extractTarGzLimited(data, dest, maxExtractedSize)
}

// extractTarGzLimited unpacks a gzip tarball into dest. GitHub wraps everything
// in a single top-level "<repo>-<sha>/" directory, which is stripped. Path
// traversal (tar-slip), absolute paths, and symlinks are rejected; total
// extracted size is capped at maxSize (a decompression-bomb guard).
func extractTarGzLimited(data []byte, dest string, maxSize int64) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := stripFirstComponent(hdr.Name)
		if name == "" {
			continue
		}
		clean := filepath.Clean(name)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("refusing unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(dest, clean)
		if !withinDir(dest, target) {
			return fmt.Errorf("refusing path escaping the cache: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			remaining := maxSize - total
			if remaining <= 0 {
				return fmt.Errorf("archive exceeds %d bytes — refusing (possible decompression bomb)", maxSize)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
			f.Close()
			if err != nil {
				return err
			}
			total += n
			if total > maxSize {
				return fmt.Errorf("archive exceeds %d bytes — refusing (possible decompression bomb)", maxSize)
			}
		default:
			// symlinks, devices, fifos — skip; a facet is plain files.
		}
	}
	return nil
}

// stripFirstComponent drops the leading "<repo>-<sha>/" path element that GitHub
// tarballs wrap their contents in.
func stripFirstComponent(name string) string {
	name = strings.TrimPrefix(name, "./")
	i := strings.IndexByte(name, '/')
	if i < 0 {
		return ""
	}
	return name[i+1:]
}

// withinDir reports whether target is dir itself or lies underneath it.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
