package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Read grants: how a command reads a file its operator named outside the
// data directory.
//
// The io.file sandbox (io.go's resolveDataPath) exists so that a program
// can never reach a path its author did not intend — a request handler that
// splices a client's string into a path cannot climb out with "..", and an
// absolute path is refused outright. Widening the sandbox would give that
// up for every program; a command still needs to read the file its
// operator hands it (`fabricd --config /etc/fabric/fabric.json`), which is
// usually not under the directory it was started from.
//
// The model: a file becomes readable when the OPERATOR names it, and only
// the process entry point may say so.
//   - grantRead(path) is legal only in `proc main` (internal/ir/build.go
//     refuses it anywhere else): the one body that runs before any request,
//     connection or client input exists, so no data an outside party
//     controls can have reached it.
//   - path must be, character for character, one of the process's own
//     arguments or the value of one of its environment variables — the two
//     channels by which an operator configures a command. A path the
//     program computed, or read from a file or a socket, is refused: the
//     grant answers false and nothing becomes readable.
//   - the grant is for that one file, for reading (readFile, fileExists,
//     fileSize, readFileAt), for the life of the process. Writing, and every
//     other path, stays inside the sandbox.
type readGrants struct {
	mu    sync.Mutex
	argv  []string
	files map[string]bool
}

func (g *readGrants) setArgv(args []string) {
	g.mu.Lock()
	g.argv = append([]string(nil), args...)
	g.mu.Unlock()
}

// operatorNamed reports whether path is one of the process's arguments or
// the value of one of its environment variables.
func (g *readGrants) operatorNamed(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.argv {
		if a == path {
			return true
		}
	}
	for _, kv := range os.Environ() {
		if _, v, ok := strings.Cut(kv, "="); ok && v == path {
			return true
		}
	}
	return false
}

// ioGrantRead implements `grantRead(path: text) -> bool` (io.file, main
// only): makes path readable for this process when the operator named it
// (see readGrants), answering whether it did.
func (s *Server) ioGrantRead(path string) (any, error) {
	if path == "" {
		return false, nil
	}
	if !s.grants.operatorNamed(path) {
		return false, nil
	}
	full, err := s.grantedPath(path)
	if err != nil {
		return nil, fmt.Errorf("grantRead: %w", err)
	}
	s.grants.mu.Lock()
	if s.grants.files == nil {
		s.grants.files = map[string]bool{}
	}
	s.grants.files[full] = true
	s.grants.mu.Unlock()
	return true, nil
}

// grantedPath is path as a grant names it: absolute and clean, a relative
// one taken from the data directory (where a command's operator stands).
func (s *Server) grantedPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	root, err := filepath.Abs(s.dataDir)
	if err != nil {
		return "", fmt.Errorf("resolving data directory: %w", err)
	}
	return filepath.Clean(filepath.Join(root, path)), nil
}

// resolveReadPath is resolveDataPath for a read: a granted file is
// readable wherever it is; anything else passes the sandbox as usual.
func (s *Server) resolveReadPath(path string) (string, error) {
	if path != "" {
		if full, err := s.grantedPath(path); err == nil {
			s.grants.mu.Lock()
			granted := s.grants.files[full]
			s.grants.mu.Unlock()
			if granted {
				return full, nil
			}
		}
	}
	return s.resolveDataPath(path)
}
