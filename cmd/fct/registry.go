package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"fct.dev/internal/parser"
)

// runAdd installs a facet package into destDir. A source is a local path (a .fct
// file or a directory of them), an http(s) URL to a .fct file, or a registry
// package name (resolved against FA_REGISTRY, default https://registry.fct.dev).
// Fetched facets are validated (must parse) before anything is written.
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
	// Local path: a .fct file or a directory of them.
	if info, err := os.Stat(source); err == nil {
		if info.IsDir() {
			out := map[string]string{}
			entries, _ := os.ReadDir(source)
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".fct") {
					data, err := os.ReadFile(filepath.Join(source, e.Name()))
					if err != nil {
						return nil, err
					}
					out[e.Name()] = string(data)
				}
			}
			if len(out) == 0 {
				return nil, fmt.Errorf("no .fct files in %s", source)
			}
			return out, nil
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

	// Registry package name → {FA_REGISTRY}/{name}.fct
	base := os.Getenv("FA_REGISTRY")
	if base == "" {
		base = "https://registry.fct.dev"
	}
	return fetchURL(strings.TrimRight(base, "/") + "/" + source + ".fct")
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	name := path.Base(url)
	if !strings.HasSuffix(name, ".fct") {
		name += ".fct"
	}
	return map[string]string{name: string(data)}, nil
}
