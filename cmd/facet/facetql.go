package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"facet/selfhost"
)

// `facet facetql [command]` — FacetQL, run by the toolchain itself.
//
// FacetQL is written in fct and its sources are embedded in this binary
// (selfhost.Engine), the way the client runtime is: a machine with the facet
// binary has a database with nothing else installed, and a release binary — a
// copy of this one with an app appended — carries it too (`./myapp facetql`),
// so a deployment's app image and its database image are the same image.
//
// It is the `facetql` command line, argument for argument: `start` (and no
// command at all) runs the server, selfhost/fqserver.fct; every other command
// — init, backup, restore, user, index, reference, get, put, delete, query,
// stats, routes, help, --version — and every malformed command line is
// selfhost/fqcli.fct, which renders clap's help and errors itself. This file
// decides only which of the two a command line is, and hands each what only
// the launcher can: the directories it may use outside its sandbox.
func cmdFacetQL(args []string) int {
	entry, err := unpackEngine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet facetql: unpacking the engine: %v\n", err)
		return 1
	}
	dir := filepath.Dir(entry)
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet facetql: %v\n", err)
		return 1
	}
	if start, ok := facetqlStartArgs(args); ok {
		return facetqlServe(filepath.Join(dir, selfhost.EngineEntry), cwd, start)
	}
	// The CLI names its default data directory by this variable, so that
	// directory — like any other its operator names — can be granted to it.
	os.Setenv("FACETQL_DEFAULT_DATA_DIR", facetqlDefaultDataDir())
	return execProgram(filepath.Join(dir, selfhost.CLIEntry), args, execOptions{root: cwd})
}

// facetqlStart is a `facetql start` command line's options (a missing one "").
type facetqlStart struct {
	dataDir, port, tlsIdentity, tlsPassword string
	has                                     map[string]bool
}

// facetqlStartArgs recognises exactly the command lines clap parses as
// `facetql start` (or as no command, which is start): `--data-dir` anywhere,
// `start` once, then start's own options, each at most once with a value —
// `--name value` (a value not starting with '-') or `--name=value` — and a
// port u16::from_str accepts. Anything else is fqcli.fct's to answer, help
// and errors included.
func facetqlStartArgs(args []string) (facetqlStart, bool) {
	st := facetqlStart{has: map[string]bool{}}
	started := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "start" && !started {
			started = true
			continue
		}
		if !strings.HasPrefix(a, "--") {
			return st, false
		}
		name, value, inline := strings.Cut(a[2:], "=")
		switch name {
		case "data-dir":
		case "port", "tls-identity", "tls-identity-password":
			if !started {
				return st, false
			}
		default:
			return st, false
		}
		if st.has[name] {
			return st, false
		}
		if !inline {
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return st, false
			}
			i++
			value = args[i]
		}
		st.has[name] = true
		switch name {
		case "data-dir":
			st.dataDir = value
		case "port":
			if !rustU16(value) {
				return st, false
			}
			st.port = value
		case "tls-identity":
			st.tlsIdentity = value
		case "tls-identity-password":
			st.tlsPassword = value
		}
	}
	return st, true
}

// rustU16 is whether clap's u16 parser accepts s: i64::from_str (an
// optional sign, then digits), then 0..=65535.
func rustU16(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n >= 0 && n <= 65535
}

// facetqlServe runs the server: start's options reach it as FACETQL_ARG_*
// (a flag over its FACETQL_* variable, as clap orders them), its sandbox is
// the directory it was started in — where a relative data directory or
// identity file is, as for any command — and the data directory and TLS
// identity it will use are granted wherever they are.
func facetqlServe(entry, cwd string, st facetqlStart) int {
	flags := map[string]string{"data-dir": "FACETQL_ARG_DATA_DIR", "port": "FACETQL_ARG_PORT",
		"tls-identity": "FACETQL_ARG_TLS_IDENTITY", "tls-identity-password": "FACETQL_ARG_TLS_IDENTITY_PASSWORD"}
	values := map[string]string{"data-dir": st.dataDir, "port": st.port, "tls-identity": st.tlsIdentity, "tls-identity-password": st.tlsPassword}
	for name, env := range flags {
		os.Unsetenv(env)
		if st.has[name] {
			os.Setenv(env, values[name])
		}
	}
	opts := execOptions{root: cwd, dirs: []string{facetqlSetting(st, "data-dir", "FACETQL_DATA_DIR", facetqlDefaultDataDir())}}
	if id := facetqlSetting(st, "tls-identity", "FACETQL_TLS_IDENTITY", ""); id != "" {
		opts.files = append(opts.files, id)
	}
	return execProgram(entry, nil, opts)
}

// facetqlSetting is what the server will read for one of start's options:
// the flag, else the variable (or its deprecated ENOCHIAN_* spelling when
// only that is set), else dflt.
func facetqlSetting(st facetqlStart, flag, env, dflt string) string {
	if st.has[flag] {
		switch flag {
		case "data-dir":
			return st.dataDir
		case "tls-identity":
			return st.tlsIdentity
		}
	}
	if v, ok := os.LookupEnv(env); ok && v != "" {
		return v
	}
	if v, ok := os.LookupEnv("ENOCHIAN_" + strings.TrimPrefix(env, "FACETQL_")); ok && v != "" {
		return v
	}
	return dflt
}

// facetqlDefaultDataDir is config.rs's default_data_dir: ~/.facetql (HOME,
// else USERPROFILE), else the current directory.
func facetqlDefaultDataDir() string {
	for _, v := range []string{"HOME", "USERPROFILE"} {
		if h := os.Getenv(v); h != "" {
			return filepath.Join(h, ".facetql")
		}
	}
	return "."
}

// unpackEngine writes the embedded engine sources to a directory named by
// their content hash (under the user's cache directory, else the temp
// directory) and answers the entry file's path. A directory already there is
// reused; a new one is written beside it and renamed into place, so two
// engines starting at once never see a half-written copy.
func unpackEngine() (string, error) {
	var names []string
	if err := fs.WalkDir(selfhost.Engine, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return err
	}); err != nil {
		return "", err
	}
	sort.Strings(names)
	h := sha256.New()
	contents := map[string][]byte{}
	for _, n := range names {
		b, err := selfhost.Engine.ReadFile(n)
		if err != nil {
			return "", err
		}
		contents[n] = b
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(b))
		h.Write(b)
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	root := filepath.Join(base, "facet")
	final := filepath.Join(root, "facetql-"+hex.EncodeToString(h.Sum(nil))[:16])
	entry := filepath.Join(final, selfhost.EngineEntry)
	if _, err := os.Stat(entry); err == nil {
		return entry, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		root = filepath.Join(os.TempDir(), "facet")
		final = filepath.Join(root, filepath.Base(final))
		entry = filepath.Join(final, selfhost.EngineEntry)
		if _, err := os.Stat(entry); err == nil {
			return entry, nil
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return "", err
		}
	}
	tmp, err := os.MkdirTemp(root, ".facetql-unpack-")
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(tmp, n), contents[n], 0o644); err != nil {
			os.RemoveAll(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		// another process won the rename: its copy is the same bytes
		if _, statErr := os.Stat(entry); statErr == nil {
			return entry, nil
		}
		return "", err
	}
	pruneEngineCopies(root, final)
	return entry, nil
}

// pruneEngineCopies removes the unpacked copies of other engine versions
// that have not been touched for a day: each toolchain build whose engine
// sources differ unpacks its own, and nothing else would ever remove the
// old ones. A day is far past the moment a running engine last read its
// copy (it compiles its sources once, at start).
func pruneEngineCopies(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if p == keep || !e.IsDir() {
			continue
		}
		// a copy, idle for a day; or an unpack a killed process left half-written
		maxAge := time.Duration(0)
		switch {
		case strings.HasPrefix(e.Name(), "facetql-"):
			maxAge = 24 * time.Hour
		case strings.HasPrefix(e.Name(), ".facetql-unpack-"):
			maxAge = time.Hour
		default:
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			os.RemoveAll(p)
		}
	}
}
