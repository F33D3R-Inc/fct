package runtime

import (
	"fmt"
	"os"
	"sort"
)

// Directory builtins (io.file): what a command that manages a directory of
// files — a database's backup and restore — needs beyond reading and
// writing one file it already knows the name of. Both resolve their path
// the way the file builtins do: inside the data directory, or inside a
// directory the operator granted (grantDir, grants.go).

// ioListDir implements `listDir(path: text) -> [text]`: the names of the
// regular files directly in path, sorted by their bytes — subdirectories,
// symlinks to directories and other entries left out. A directory that
// does not exist has no files ([]); any other failure is an error.
func (s *Server) ioListDir(path string) (any, error) {
	full, err := s.resolveReadPath(path)
	if err != nil {
		return nil, fmt.Errorf("listDir: %w", err)
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return []any{}, nil
		}
		return nil, fmt.Errorf("listDir: %q: %v", path, err)
	}
	var names []string
	for _, e := range entries {
		info, err := os.Stat(full + string(os.PathSeparator) + e.Name())
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out, nil
}

// ioMakeDir implements `makeDir(path: text) -> bool`: creates path and any
// missing parents (a directory already there is success), with the
// permissions the process's umask allows — std::fs::create_dir_all's
// behaviour. A path that names an existing file is an error.
func (s *Server) ioMakeDir(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("makeDir: %w", err)
	}
	if err := os.MkdirAll(full, 0o777); err != nil {
		return nil, fmt.Errorf("makeDir: %q: %v", path, err)
	}
	return true, nil
}
