package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fct.dev/internal/codegen"
	"fct.dev/internal/parser"
)

// PkgManifest (fct.pkg.json) describes a publishable facet package — the unit a
// developer creates, saves, and submits to the community registry.
type PkgManifest struct {
	Name        string   `json:"name"`    // e.g. "social/post-card"
	Version     string   `json:"version"` // semver
	Author      string   `json:"author,omitempty"`
	Description string   `json:"description,omitempty"`
	License     string   `json:"license,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Facets      []string `json:"facets,omitempty"` // facet names the package provides
}

const manifestName = "fct.pkg.json"

// runInit scaffolds a new facet package: a manifest + a sample facet.
func runInit(dir, name string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	man := PkgManifest{
		Name: name, Version: "0.1.0", License: "MIT",
		Description: "A Facet Architecture package.",
		Tags:        []string{}, Facets: []string{"Hello"},
	}
	data, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, manifestName), append(data, '\n'), 0o644); err != nil {
		return err
	}
	sample := "facet Hello:\n    what:\n        name: str\n    looks:\n        <p class=\"hello\">Hello, {name}!</p>\n"
	if err := os.WriteFile(filepath.Join(dir, "hello.fct"), []byte(sample), 0o644); err != nil {
		return err
	}
	fmt.Printf("created package %s in %s\n  edit fct.pkg.json + the .fct files, then:\n  fct pack %s   (build)\n  fct publish %s  (submit)\n", name, dir, dir, dir)
	return nil
}

// loadManifest reads and validates fct.pkg.json in dir.
func loadManifest(dir string) (PkgManifest, error) {
	var m PkgManifest
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return m, fmt.Errorf("no %s in %s (run `fct init`)", manifestName, dir)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s: %w", manifestName, err)
	}
	if m.Name == "" || m.Version == "" {
		return m, fmt.Errorf("%s needs a name and version", manifestName)
	}
	return m, nil
}

// packDir validates that a package's facets compile and returns a .tgz of the
// manifest + every .fct file, plus the manifest.
func packDir(dir string) ([]byte, PkgManifest, error) {
	man, err := loadManifest(dir)
	if err != nil {
		return nil, man, err
	}
	// Validate: all facets in the package must compile together.
	src, err := readFctSource(dir)
	if err != nil {
		return nil, man, err
	}
	facets, err := parser.Parse(src)
	if err != nil {
		return nil, man, fmt.Errorf("package does not compile: %w", err)
	}
	if _, err := codegen.Generate(facets); err != nil {
		return nil, man, fmt.Errorf("package does not compile: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, content []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			return err
		}
		_, err := tw.Write(content)
		return err
	}
	manData, _ := json.MarshalIndent(man, "", "  ")
	if err := add(manifestName, manData); err != nil {
		return nil, man, err
	}
	files, _ := fctFiles(dir)
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return nil, man, err
		}
		if err := add(filepath.Base(f), content); err != nil {
			return nil, man, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, man, err
	}
	if err := gz.Close(); err != nil {
		return nil, man, err
	}
	return buf.Bytes(), man, nil
}

// unpackTgz extracts a package archive into its manifest and facet files.
func unpackTgz(data []byte) (PkgManifest, map[string]string, error) {
	var man PkgManifest
	files := map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return man, nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return man, nil, err
		}
		content, err := io.ReadAll(io.LimitReader(tr, 4<<20))
		if err != nil {
			return man, nil, err
		}
		if h.Name == manifestName {
			_ = json.Unmarshal(content, &man)
		} else if strings.HasSuffix(h.Name, ".fct") {
			files[filepath.Base(h.Name)] = string(content)
		}
	}
	return man, files, nil
}

// runPack builds a package archive on disk.
func runPack(dir string) error {
	tgz, man, err := packDir(dir)
	if err != nil {
		return err
	}
	out := pkgFileName(man)
	if err := os.WriteFile(out, tgz, 0o644); err != nil {
		return err
	}
	fmt.Printf("packed %s@%s → %s (%d bytes)\n", man.Name, man.Version, out, len(tgz))
	return nil
}

func pkgFileName(m PkgManifest) string {
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(m.Name)
	return fmt.Sprintf("%s-%s.tgz", safe, m.Version)
}

// indexEntry is one package's metadata in the registry index.
type indexEntry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Author      string   `json:"author,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func sortedIndex(m map[string]indexEntry) []indexEntry {
	out := make([]indexEntry, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
