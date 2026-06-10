package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func registryBase() string {
	if b := os.Getenv("FA_REGISTRY"); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://registry.fct.dev"
}

func safeName(name string) string { return strings.ReplaceAll(name, "/", "__") }

// runRegistry runs a file-backed facet registry server — the community hub.
// POST /publish stores a package; GET /p/{name} serves the latest tarball
// (or ?v=x.y.z); GET /index and GET /search?q= list/find packages. Self-host it
// (or point apps at a hosted one via FA_REGISTRY).
func runRegistry(dir, addr string) error {
	store := filepath.Join(dir, "packages")
	if err := os.MkdirAll(store, 0o755); err != nil {
		return err
	}
	idxPath := filepath.Join(dir, "index.json")
	var mu sync.Mutex

	loadIdx := func() map[string]indexEntry {
		m := map[string]indexEntry{}
		if data, err := os.ReadFile(idxPath); err == nil {
			_ = json.Unmarshal(data, &m)
		}
		return m
	}
	saveIdx := func(m map[string]indexEntry) {
		data, _ := json.MarshalIndent(m, "", "  ")
		_ = os.WriteFile(idxPath, data, 0o644)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("POST /publish", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		man, files, err := unpackTgz(data)
		if err != nil || man.Name == "" || man.Version == "" {
			http.Error(w, "invalid package", http.StatusBadRequest)
			return
		}
		// reject packages whose facets don't parse (don't host broken code)
		var src strings.Builder
		for _, c := range files {
			src.WriteString(c)
			src.WriteByte('\n')
		}
		if _, perr := compileCheck(src.String()); perr != nil {
			http.Error(w, "package facets do not compile: "+perr.Error(), http.StatusBadRequest)
			return
		}
		pdir := filepath.Join(store, safeName(man.Name))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(filepath.Join(pdir, man.Version+".tgz"), data, 0o644); err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		idx := loadIdx()
		idx[man.Name] = indexEntry{Name: man.Name, Version: man.Version, Description: man.Description, Author: man.Author, Tags: man.Tags}
		saveIdx(idx)
		mu.Unlock()
		fmt.Fprintf(w, "published %s@%s\n", man.Name, man.Version)
	})

	mux.HandleFunc("GET /p/{name...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		version := r.URL.Query().Get("v")
		if version == "" {
			if e, ok := loadIdx()[name]; ok {
				version = e.Version
			}
		}
		if version == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, filepath.Join(store, safeName(name), version+".tgz"))
	})

	mux.HandleFunc("GET /index", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sortedIndex(loadIdx()))
	})

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.ToLower(r.URL.Query().Get("q"))
		var hits []indexEntry
		for _, e := range sortedIndex(loadIdx()) {
			hay := strings.ToLower(e.Name + " " + e.Description + " " + strings.Join(e.Tags, " "))
			if q == "" || strings.Contains(hay, q) {
				hits = append(hits, e)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hits)
	})

	fmt.Printf("fct registry on http://%s  (store: %s)\n", addr, dir)
	return http.ListenAndServe(addr, mux)
}
