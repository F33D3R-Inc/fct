package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed scaffold
var scaffoldFS embed.FS

// faModule is the framework's own module path — scaffolded apps depend on it.
const faModule = "github.com/F33D3R-Inc/fct"

// runNew writes a runnable FA project into dir with the given Go module path.
// In .tmpl files the sentinel __MODULE__ is replaced with the module path (plain
// string replace, so Go's own {{ }} in the templates is left untouched). The
// .tmpl suffix is stripped on write; "gitignore" becomes ".gitignore".
func runNew(dir, module string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}

	err := fs.WalkDir(scaffoldFS, "scaffold", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("scaffold", path)
		if err != nil {
			return err
		}
		dest := strings.TrimSuffix(rel, ".tmpl")
		if filepath.Base(dest) == "gitignore" {
			dest = filepath.Join(filepath.Dir(dest), ".gitignore")
		}
		destPath := filepath.Join(dir, dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}

		content, err := scaffoldFS.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".tmpl") {
			content = []byte(strings.ReplaceAll(string(content), "__MODULE__", module))
		}
		return os.WriteFile(destPath, content, 0o644)
	})
	if err != nil {
		return err
	}

	// Pin the framework to the newest release and resolve deps now, so the project
	// starts current and `go run .` works immediately (writes go.sum). Best-effort:
	// if the developer is offline or the module isn't published yet, don't fail the
	// scaffold — just tell them to run it themselves.
	fmt.Printf("created %s\n", dir)
	get := exec.Command("go", "get", faModule+"@latest")
	get.Dir, get.Stdout, get.Stderr = dir, os.Stderr, os.Stderr
	tidyOK := get.Run() == nil
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir, tidy.Stdout, tidy.Stderr = dir, os.Stderr, os.Stderr
	if tidy.Run() != nil {
		tidyOK = false
	}
	if !tidyOK {
		fmt.Printf("\nnote: resolving dependencies failed (offline?). Run `go mod tidy` yourself, then `go run .`\n")
	}

	fmt.Printf("\nnext:\n  cd %s\n  go run .\n  open http://localhost:7373\n", dir)
	return nil
}

// runDev builds and runs the project in dir, restarting whenever a .fct file
// changes. It builds to a real binary and manages it by PID, so killing it on
// reload never leaves an orphan holding the port (unlike `go run`).
func runDev(dir string) error {
	facetsDir := filepath.Join(dir, "facets")
	for {
		log.Println("fct dev: building…")
		build := exec.Command("go", "build", "-o", ".fctdev-bin", ".")
		build.Dir, build.Stdout, build.Stderr = dir, os.Stdout, os.Stderr

		var proc *exec.Cmd
		if err := build.Run(); err != nil {
			log.Println("fct dev: build failed; waiting for changes")
		} else {
			proc = exec.Command("./.fctdev-bin")
			proc.Dir, proc.Stdout, proc.Stderr, proc.Env = dir, os.Stdout, os.Stderr, os.Environ()
			if err := proc.Start(); err != nil {
				return err
			}
		}

		snap := mtimeSum(facetsDir)
		for mtimeSum(facetsDir) == snap {
			time.Sleep(400 * time.Millisecond)
		}
		log.Println("fct dev: change detected, restarting")
		if proc != nil {
			_ = proc.Process.Kill()
			_ = proc.Wait()
		}
	}
}

// mtimeSum is a cheap change signal: the summed modification times of *.fct in
// dir. Any edit changes the sum.
func mtimeSum(dir string) int64 {
	var sum int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".fct") {
			if info, err := e.Info(); err == nil {
				sum += info.ModTime().UnixNano()
			}
		}
	}
	return sum
}
