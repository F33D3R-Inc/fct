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
	// dirs: directories granted by grantDir — every file under one is
	// readable and writable, as the data directory's are (see ioGrantDir).
	dirs map[string]bool
}

// coversDir reports whether full (absolute and clean) is a granted
// directory or lies under one.
func (g *readGrants) coversDir(full string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for d := range g.dirs {
		if full == d || strings.HasPrefix(full, d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (g *readGrants) setArgv(args []string) {
	g.mu.Lock()
	g.argv = append([]string(nil), args...)
	g.mu.Unlock()
}

// operatorNamed reports whether path is one of the process's arguments (or
// the value of a `--name=value` one) or the value of one of its environment
// variables.
func (g *readGrants) operatorNamed(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.argv {
		if a == path {
			return true
		}
		// `--name=value` names value as surely as `--name value` does.
		if strings.HasPrefix(a, "-") {
			if _, v, ok := strings.Cut(a, "="); ok && v == path {
				return true
			}
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
			if granted || s.grants.coversDir(full) {
				return full, nil
			}
		}
	}
	return s.resolveDataPath(path)
}

// ioGrantDir implements `grantDir(path: text) -> bool` (io.file, main
// only): a directory the operator named — an argument or an environment
// variable's value, exactly as grantRead's file — becomes a second place
// the io.file builtins may read and write, for the life of the process.
// It is what a command that takes a directory from its operator needs (a
// backup's destination, a restore's source): the data directory stays the
// sandbox for everything else, and a path the program computed can never
// widen it. false when the operator did not name path.
func (s *Server) ioGrantDir(path string) (any, error) {
	if path == "" || !s.grants.operatorNamed(path) {
		return false, nil
	}
	full, err := s.grantedPath(path)
	if err != nil {
		return nil, fmt.Errorf("grantDir: %w", err)
	}
	s.grants.mu.Lock()
	if s.grants.dirs == nil {
		s.grants.dirs = map[string]bool{}
	}
	s.grants.dirs[full] = true
	s.grants.mu.Unlock()
	return true, nil
}

// GrantDir is grantDir for the embedder: the toolchain launching a program
// it ships (`facet facetql` running the FacetQL engine) grants the
// directory it resolved for it — the database's data directory, which the
// engine names by the path its operator configured.
func (s *Server) GrantDir(path string) error {
	full, err := s.grantedPath(path)
	if err != nil {
		return err
	}
	s.grants.mu.Lock()
	defer s.grants.mu.Unlock()
	if s.grants.dirs == nil {
		s.grants.dirs = map[string]bool{}
	}
	s.grants.dirs[full] = true
	return nil
}

// GrantFile is grantRead for the embedder: one file made readable (a TLS
// identity the operator configured, wherever it lives).
func (s *Server) GrantFile(path string) error {
	full, err := s.grantedPath(path)
	if err != nil {
		return err
	}
	s.grants.mu.Lock()
	defer s.grants.mu.Unlock()
	if s.grants.files == nil {
		s.grants.files = map[string]bool{}
	}
	s.grants.files[full] = true
	return nil
}
