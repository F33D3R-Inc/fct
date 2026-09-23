package runtime

// The I/O capability builtins: readFile/writeFile (io.file) and httpGet/
// httpPost (io.net) — ROADMAP.md's "keystone" effects system. A proc may call
// these only if it declares the matching capability in its own `uses` clause
// (internal/ir/build.go's checkProcCapabilities enforces that at compile
// time); this file is purely the runtime execution side, reached only through
// runtime/eval.go's callProcBuiltin.
//
// File access is sandboxed to a single configured root directory — the same
// convention runtime/upload.go already established for uploaded files
// (FACET_UPLOAD_DIR, defaulting to a directory beside the running server),
// just for arbitrary author-declared paths instead of a fixed set of
// server-minted upload names, so path-traversal prevention has to be a real
// clean+prefix check (upload.go's own separator/".." substring reject is
// enough for a flat, server-chosen filename; readFile/writeFile paths are
// author-supplied, data-dependent at runtime, and may be nested).

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dataDirFromEnv resolves the sandbox root a proc's readFile/writeFile may
// read and write under, defaulting to ./facet-data beside the running
// server — mirrors uploadDirFromEnv exactly (see runtime/upload.go).
func dataDirFromEnv() string {
	if d := os.Getenv("FACET_DATA_DIR"); d != "" {
		return d
	}
	return "facet-data"
}

// resolveDataPath turns a proc-supplied, author-facing path (e.g. "config.json"
// or "reports/2026.csv") into a real filesystem path guaranteed to stay inside
// s.dataDir — refusing an absolute path outright and, for a relative one that
// tries to climb out with "..", refusing it once resolved (filepath.Clean) if
// it no longer has the sandbox root as its prefix. This is the standard
// clean-then-prefix-check shape (LANGUAGE.md/ROADMAP.md's own suggested
// convention where no fixed-filename precedent like upload.go's applies), and
// it is the one gate both readFile and writeFile pass through — a proc can
// never reach a path outside its sandbox by construction, not by review.
func (s *Server) resolveDataPath(reqPath string) (string, error) {
	if reqPath == "" {
		return "", fmt.Errorf("empty file path")
	}
	root, err := filepath.Abs(s.dataDir)
	if err != nil {
		return "", fmt.Errorf("resolving data directory: %w", err)
	}
	var full string
	if filepath.IsAbs(reqPath) {
		// An absolute path is accepted only when it already names a place
		// inside the sandbox (a command run with `facet exec` is handed paths
		// by its operator, who writes them either way); the prefix check below
		// is the same gate a relative path passes.
		full = filepath.Clean(reqPath)
	} else {
		full = filepath.Clean(filepath.Join(root, reqPath))
	}
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the sandboxed data directory", reqPath)
	}
	return full, nil
}

// ioReadFile implements the `readFile(path: text) -> text` builtin (io.file).
// Every failure mode (path escapes the sandbox, file missing, permission
// denied, or it names a directory) is a clean error string, never a Go panic
// — the same "ends the request as a proper error response" contract
// runtime/array_test.go's TestArrayOutOfBoundsIsCleanError already proves for
// an out-of-bounds array read.
func (s *Server) ioReadFile(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("readFile: %w", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("readFile: %q not found", path)
		}
		if os.IsPermission(err) {
			return nil, fmt.Errorf("readFile: %q: permission denied", path)
		}
		return nil, fmt.Errorf("readFile: %q: %v", path, err)
	}
	return string(data), nil
}

// ioWriteFile implements the `writeFile(path: text, content: text) -> bool`
// builtin (io.file): creates any missing parent directories inside the
// sandbox (so `writeFile("reports/2026.csv", ...)` works the first time,
// matching what an author would expect of a general-purpose write), then
// writes content, overwriting any existing file at path. Returns true on
// success; every failure (sandbox escape, permission denied, disk full, path
// names an existing directory) is a clean error instead.
func (s *Server) ioWriteFile(path, content string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("writeFile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("writeFile: %q: %v", path, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		if os.IsPermission(err) {
			return nil, fmt.Errorf("writeFile: %q: permission denied", path)
		}
		return nil, fmt.Errorf("writeFile: %q: %v", path, err)
	}
	return true, nil
}

// ioAppendFile implements `appendFile(path: text, content: text) -> bool`
// (io.file): opens (creating) the file in append mode, writes content at
// end-of-file, and fsyncs before returning — so a true result is a
// durability boundary, the way a write-ahead log's append must be. The
// directory entry is fsynced too when the file was just created, since a
// new name is metadata a crash could otherwise lose. Same sandbox and
// error shape as ioWriteFile; parent directories are created as needed.
func (s *Server) ioAppendFile(path, content string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("appendFile: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("appendFile: %q: %v", path, err)
	}
	_, statErr := os.Stat(full)
	created := os.IsNotExist(statErr)
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		if os.IsPermission(err) {
			return nil, fmt.Errorf("appendFile: %q: permission denied", path)
		}
		return nil, fmt.Errorf("appendFile: %q: %v", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil, fmt.Errorf("appendFile: %q: %v", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("appendFile: %q: sync: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("appendFile: %q: %v", path, err)
	}
	if created {
		if err := syncDir(filepath.Dir(full)); err != nil {
			return nil, fmt.Errorf("appendFile: %q: sync directory: %v", path, err)
		}
	}
	return true, nil
}

// ioFileExists implements `fileExists(path: text) -> bool` (io.file): whether
// a regular file is at path inside the sandbox. A missing file is an answer,
// not an error — the one question readFile cannot be asked without failing
// (a first boot has no log yet). A sandbox escape is still an error.
func (s *Server) ioFileExists(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("fileExists: %w", err)
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return nil, fmt.Errorf("fileExists: %q: %v", path, err)
	}
	return info.Mode().IsRegular(), nil
}

// ioTruncateFile implements `truncateFile(path: text, size: int) -> bool`
// (io.file): cuts the file to size bytes, then fsyncs the file and its
// directory so the new length itself survives a crash — the repair a torn
// log tail needs before anything may be appended after it. A negative size
// or a missing file is an error.
func (s *Server) ioTruncateFile(path string, size int) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("truncateFile: %w", err)
	}
	if size < 0 {
		return nil, fmt.Errorf("truncateFile: %q: negative size %d", path, size)
	}
	f, err := os.OpenFile(full, os.O_WRONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("truncateFile: %q not found", path)
		}
		return nil, fmt.Errorf("truncateFile: %q: %v", path, err)
	}
	if err := f.Truncate(int64(size)); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncateFile: %q: %v", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncateFile: %q: sync: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("truncateFile: %q: %v", path, err)
	}
	if err := syncDir(filepath.Dir(full)); err != nil {
		return nil, fmt.Errorf("truncateFile: %q: sync directory: %v", path, err)
	}
	return true, nil
}

// syncDir fsyncs a directory so a change to one of its entries (a created
// name, a file's new length) is durable, the POSIX way: open it read-only
// and Sync the handle.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ioFileSize implements `fileSize(path: text) -> int`: the file's length in
// bytes, or -1 when there is no such file — the paged-file counterpart of
// fileExists, since a pager needs the length (how many whole pages a file
// holds) and not merely whether it is there.
func (s *Server) ioFileSize(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("fileSize: %w", err)
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, nil
		}
		return nil, fmt.Errorf("fileSize: %q: %v", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("fileSize: %q is a directory", path)
	}
	return int(info.Size()), nil
}

// ioReadFileAt implements `readFileAt(path: text, offset: int, n: int) ->
// bytes`: exactly n bytes starting at offset, as a byte buffer. A read that
// cannot supply all n bytes is an error naming the offset — never a short
// buffer a caller could mistake for a whole page.
func (s *Server) ioReadFileAt(path string, offset, n int) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("readFileAt: %w", err)
	}
	if offset < 0 || n < 0 {
		return nil, fmt.Errorf("readFileAt: %q: negative offset %d or length %d", path, offset, n)
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("readFileAt: %q not found", path)
		}
		return nil, fmt.Errorf("readFileAt: %q: %v", path, err)
	}
	defer f.Close()
	data := make([]byte, n)
	if _, err := f.ReadAt(data, int64(offset)); err != nil {
		return nil, fmt.Errorf("readFileAt: %q: failed to read %d bytes at offset %d: %v", path, n, offset, err)
	}
	buf := make([]any, n)
	for i, b := range data {
		buf[i] = int(b)
	}
	return buf, nil
}

// ioWriteFileAt implements `writeFileAt(path: text, offset: int, data: bytes)
// -> bool`: write the byte buffer at offset, creating the file (and its
// directory) when absent and never truncating it. Not synced — a paged file
// is written page by page and made durable once, by syncFile, the way a
// buffer pool flushes before it fsyncs.
func (s *Server) ioWriteFileAt(path string, offset int, content any) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("writeFileAt: %w", err)
	}
	if offset < 0 {
		return nil, fmt.Errorf("writeFileAt: %q: negative offset %d", path, offset)
	}
	arr, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("writeFileAt: %q: content is not a byte buffer", path)
	}
	data := make([]byte, len(arr))
	for i, v := range arr {
		b := toInt(v)
		if b < 0 || b > 255 {
			return nil, fmt.Errorf("writeFileAt: %q: byte value %d out of range (must be 0-255)", path, b)
		}
		data[i] = byte(b)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("writeFileAt: %q: %v", path, err)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("writeFileAt: %q: %v", path, err)
	}
	if _, err := f.WriteAt(data, int64(offset)); err != nil {
		f.Close()
		return nil, fmt.Errorf("writeFileAt: %q: failed to write %d bytes at offset %d: %v", path, len(data), offset, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("writeFileAt: %q: %v", path, err)
	}
	return true, nil
}

// ioSyncFile implements `syncFile(path: text) -> bool`: fsync the file and
// then its directory, so both its bytes and its name are on stable storage.
func (s *Server) ioSyncFile(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("syncFile: %w", err)
	}
	f, err := os.OpenFile(full, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("syncFile: %q not found", path)
		}
		return nil, fmt.Errorf("syncFile: %q: %v", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("syncFile: %q: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("syncFile: %q: %v", path, err)
	}
	if err := syncDir(filepath.Dir(full)); err != nil {
		return nil, fmt.Errorf("syncFile: %q: sync directory: %v", path, err)
	}
	return true, nil
}

// ioRenameFile implements `renameFile(from: text, to: text) -> bool`: an
// atomic rename within the data directory (replacing `to`), then an fsync of
// the directory so the new name survives a crash — the publish step of a
// write-temp-then-rename update.
func (s *Server) ioRenameFile(from, to string) (any, error) {
	src, err := s.resolveDataPath(from)
	if err != nil {
		return nil, fmt.Errorf("renameFile: %w", err)
	}
	dst, err := s.resolveDataPath(to)
	if err != nil {
		return nil, fmt.Errorf("renameFile: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("renameFile: %q not found", from)
		}
		return nil, fmt.Errorf("renameFile: %q -> %q: %v", from, to, err)
	}
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return nil, fmt.Errorf("renameFile: %q: sync directory: %v", to, err)
	}
	return true, nil
}

// ioRemoveFile implements `removeFile(path: text) -> bool`: the file under
// the data directory deleted, then an fsync of the directory so the removal
// survives a crash. A file already absent is not an error — the goal state
// (no such file) holds — so a retried cleanup after a crash between "stop
// referencing it" and "delete it" converges instead of failing.
func (s *Server) ioRemoveFile(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("removeFile: %w", err)
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return nil, fmt.Errorf("removeFile: %q: %v", path, err)
	}
	if err := syncDir(filepath.Dir(full)); err != nil {
		return nil, fmt.Errorf("removeFile: %q: sync directory: %v", path, err)
	}
	return true, nil
}

// ioReadFileBytes is ioReadFile's counterpart for a declared `file ... bytes
// at ...` resource (a `let x = read Name()` statement over a bytes-typed
// file — see internal/ir/build.go's ast.FileOp case): reads the file's raw
// bytes and hands them back as a byte-buffer array ([]any of 0-255 ints), the
// exact representation bytes(n)/index-read/len() already work over (see
// runtime/eval.go's "bytes" case in callBuiltin) — so a bytes file's content
// is usable with every existing byte-buffer mechanic with no special-casing
// anywhere else. Same sandbox and error shape as ioReadFile.
func (s *Server) ioReadFileBytes(path string) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("read: %q not found", path)
		}
		if os.IsPermission(err) {
			return nil, fmt.Errorf("read: %q: permission denied", path)
		}
		return nil, fmt.Errorf("read: %q: %v", path, err)
	}
	buf := make([]any, len(data))
	for i, b := range data {
		buf[i] = int(b)
	}
	return buf, nil
}

// ioWriteFileBytes is ioWriteFile's counterpart for a bytes-typed `file`
// resource (a `write Name(content)` statement, content a byte-buffer array):
// every element that can ever reach here was already range-checked to 0-255
// by whatever produced it (bytes(n)'s zero-fill, or an index-write's own
// 0-255 check in runtime/proccompile.go "indexset" case), so the
// re-check below is defensive rather than load-bearing. Same sandbox,
// parent-dir creation, and overwrite semantics as ioWriteFile.
func (s *Server) ioWriteFileBytes(path string, content any) (any, error) {
	full, err := s.resolveDataPath(path)
	if err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	arr, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("write: %q: content is not a byte buffer", path)
	}
	data := make([]byte, len(arr))
	for i, v := range arr {
		n := toInt(v)
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("write: %q: byte value %d out of range (must be 0-255)", path, n)
		}
		data[i] = byte(n)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("write: %q: %v", path, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		if os.IsPermission(err) {
			return nil, fmt.Errorf("write: %q: permission denied", path)
		}
		return nil, fmt.Errorf("write: %q: %v", path, err)
	}
	return true, nil
}

// ioHTTPClient is the client every httpGet/httpPost call shares — a 5-second
// timeout, matching this codebase's existing convention for an authority's
// own outbound HTTP call (runtime/server.go's callService/callServiceSync,
// which post to a declared `service` brain): a proc calling out is exactly
// that same shape of egress, just to an author-supplied URL instead of a
// declared service's, so it gets the same bound rather than a bespoke one.
// Without it, a slow or hung remote host would block the request — and, since
// runActionLocked holds the store lock for a `do` call's whole duration, every
// other request — indefinitely.
var ioHTTPClient = &http.Client{Timeout: 5 * time.Second}

// validHTTPURL rejects anything that isn't an absolute http(s) URL before it
// ever reaches net/http — a clean compile-adjacent error ("invalid URL") for
// author mistakes (a relative path, a bare host with no scheme, a `file://`
// or other scheme this builtin has no business fetching) rather than
// whatever confusing transport error net/http would otherwise produce.
func validHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %v", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid URL %q: must be an absolute http:// or https:// URL", raw)
	}
	return nil
}

// ioHTTPGet implements the `httpGet(url: text) -> text` builtin (io.net):
// GETs url and returns its response body. A non-2xx response is a clean
// error naming the status code (consistent with callServiceSync's own stance
// on a service brain's non-2xx reply) rather than handing the proc a body it
// has no way to know is an error page; a transport failure (DNS, connection
// refused, timeout) is likewise a clean error, never a panic.
func (s *Server) ioHTTPGet(rawURL string) (any, error) {
	if err := validHTTPURL(rawURL); err != nil {
		return nil, fmt.Errorf("httpGet: %v", err)
	}
	resp, err := ioHTTPClient.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("httpGet %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpGet %s: reading response: %v", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("httpGet %s: returned %d", rawURL, resp.StatusCode)
	}
	return string(body), nil
}

// ioHTTPPost implements the `httpPost(url: text, body: text) -> text` builtin
// (io.net): POSTs body (as plain text — the builtin's whole signature is
// text-in/text-out, so it takes no stance on JSON vs. form-encoded vs.
// anything else; an author building a JSON API call constructs the JSON
// string themselves the same way they would for `call Service.op`'s body) and
// returns the response body. Same non-2xx-is-an-error and
// transport-failure-is-a-clean-error stance as ioHTTPGet.
func (s *Server) ioHTTPPost(rawURL, body string) (any, error) {
	if err := validHTTPURL(rawURL); err != nil {
		return nil, fmt.Errorf("httpPost: %v", err)
	}
	resp, err := ioHTTPClient.Post(rawURL, "text/plain; charset=utf-8", strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("httpPost %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("httpPost %s: reading response: %v", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("httpPost %s: returned %d", rawURL, resp.StatusCode)
	}
	return string(respBody), nil
}
