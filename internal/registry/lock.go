package registry

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// lockfileName is the committed, machine-maintained pin file at a project root.
const lockfileName = "facet.lock"

// Module is one resolved dependency in facet.lock: the repo pinned to an
// immutable commit, with the tarball hash that fetch verifies against and the
// resolved entry file (a cache convenience).
type Module struct {
	Version   string `json:"version"`   // resolved tag (or branch/commit name)
	Commit    string `json:"commit"`    // 40-hex commit SHA — the immutable pin
	Integrity string `json:"integrity"` // sha256-<base64> of the downloaded tar.gz
	Main      string `json:"main"`      // resolved entry file inside the module
}

// Lock is the whole facet.lock document: the pinned dependency graph for one
// app, one entry per repo. It is committed so a fresh clone builds identical
// bytes.
type Lock struct {
	LockfileVersion int               `json:"lockfileVersion"`
	Facet           string            `json:"facet"` // toolchain that last wrote it
	Modules         map[string]Module `json:"modules"`
}

// LoadLock reads facet.lock from projectDir. A missing lock is not an error — it
// returns an empty, ready-to-populate lock (a project with no remote deps).
func LoadLock(projectDir string) (*Lock, error) {
	b, err := os.ReadFile(filepath.Join(projectDir, lockfileName))
	if errors.Is(err, fs.ErrNotExist) {
		return &Lock{LockfileVersion: 1, Modules: map[string]Module{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	if l.LockfileVersion == 0 {
		l.LockfileVersion = 1
	}
	if l.Modules == nil {
		l.Modules = map[string]Module{}
	}
	return &l, nil
}

// Save writes facet.lock back to projectDir. json.Marshal sorts map keys, so the
// committed file has a stable, diff-friendly ordering for free.
func (l *Lock) Save(projectDir string) error {
	if l.LockfileVersion == 0 {
		l.LockfileVersion = 1
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(projectDir, lockfileName), b, 0o644)
}
