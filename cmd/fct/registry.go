package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/F33D3R-Inc/fct/internal/parser"
)

// runAdd installs a facet package into destDir. A source is a registry package
// name (resolved against FA_REGISTRY), an http(s) URL (a .tgz package or a .fct
// file), or a local path (a .fct file, a directory, or a .tgz). Fetched facets
// are validated (must parse) before anything is written.
func runAdd(source, destDir string) error {
	files, err := fetchPackage(source)
	if err != nil {
		return err
	}
	for name, content := range files {
		if _, err := parser.Parse(content); err != nil {
			return fmt.Errorf("package facet %s does not parse: %w", name, err)
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for name, content := range files {
		dest := filepath.Join(destDir, name)
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Println("added", dest)
	}
	fmt.Printf("installed %d facet file(s) from %s\n", len(files), source)
	return nil
}

func fetchPackage(source string) (map[string]string, error) {
	// Local path: a .tgz package, a .fct file, or a directory of .fct files.
	if info, err := os.Stat(source); err == nil {
		if !info.IsDir() && strings.HasSuffix(source, ".tgz") {
			data, err := os.ReadFile(source)
			if err != nil {
				return nil, err
			}
			_, files, err := unpackTgz(data)
			return files, err
		}
		if info.IsDir() {
			return localDirFacets(source)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		return map[string]string{filepath.Base(source): string(data)}, nil
	}

	// Remote URL.
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return fetchURL(source)
	}

	// Registry package name → {FA_REGISTRY}/p/{name} (a .tgz).
	return fetchURL(registryBase() + "/p/" + source)
}

func localDirFacets(dir string) (map[string]string, error) {
	out := map[string]string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			out[e.Name()] = string(data)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no .fct files in %s", dir)
	}
	return out, nil
}

func fetchURL(url string) (map[string]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	// gzip magic → it's a .tgz package; otherwise a bare .fct file.
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		_, files, err := unpackTgz(data)
		return files, err
	}
	name := path.Base(url)
	if !strings.HasSuffix(name, ".fct") {
		name += ".fct"
	}
	return map[string]string{name: string(data)}, nil
}

// runPublish packs a package directory and submits it to the registry.
func runPublish(dir string) error {
	tgz, man, err := packDir(dir)
	if err != nil {
		return err
	}
	url := registryBase() + "/publish"
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(url, "application/gzip", bytes.NewReader(tgz))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("publish %s@%s: %s — %s", man.Name, man.Version, resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("published %s@%s to %s\n", man.Name, man.Version, registryBase())
	return nil
}

// runSearch queries the registry.
func runSearch(query string) error {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(registryBase() + "/search?q=" + url(query))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var hits []indexEntry
	if err := json.NewDecoder(resp.Body).Decode(&hits); err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no packages found")
		return nil
	}
	for _, e := range hits {
		fmt.Printf("%-28s %-8s %s\n", e.Name, e.Version, e.Description)
	}
	return nil
}

func url(s string) string { return strings.ReplaceAll(s, " ", "%20") }
