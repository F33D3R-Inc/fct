package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"facet/internal/ir"
	"facet/internal/registry"
	"facet/runtime"
)

// `facet build --release <file.fct>` — turn an app into one self-contained
// executable, and `<that executable>` — the app it produced.
//
// # What a built app is
//
// The toolchain is a single static binary that carries the runtime and the
// client (`assets/facet.js`, embedded with go:embed) inside it. The only thing
// it does not carry is the app: `facet run app.fct` compiles the source on
// every boot, which means production needs the source tree, the compiler, and
// the import graph resolved — three things a deployment should not need.
//
// A release binary closes that gap by carrying the *compiled IR* too. The IR is
// the compiler's terminal artifact and the runtime's only input (runtime.New
// takes an *ir.IR and nothing else), so an executable that holds the IR holds
// the whole app. No source, no compiler, no interpreter, no facet.lock, no
// network fetch at boot.
//
// # Why appending, and not `go build`
//
// The obvious way to embed a user's IR in a Go binary is go:embed plus a
// compile — which would make a Go toolchain a hard requirement for shipping a
// Facet app. It is not acceptable for `facet build --release` to work only on
// machines with Go installed: the whole promise is that the toolchain is one
// binary you download.
//
// So the IR is not compiled in, it is *appended*. The artifact is a byte-for-
// byte copy of the facet binary that is already running, followed by the gzipped
// bundle and a fixed-width trailer naming its length. Executable formats ignore
// trailing bytes (an ELF or PE loader maps the sections its headers describe and
// never looks past them), so the copy still runs; on startup it reads its own
// file, finds the trailer, and serves the app it carries instead of behaving as
// the toolchain. Producing a release therefore needs no compiler at all — only
// the binary the user already has.
//
// Two consequences worth knowing:
//
//   - Cross-building is a base swap, not a compile: --base dist/facet-linux-amd64
//     produces a Linux artifact from any host, because the base is what supplies
//     the machine code.
//   - macOS signs its binaries, and appending invalidates that signature (the
//     kernel kills an unsigned-but-modified Mach-O). writeRelease detects a
//     Mach-O base and re-signs with an ad-hoc signature when `codesign` is
//     available — that is a macOS host tool, not a Go toolchain — and says so
//     plainly when it is not.

// bundleMagic ends the file of every release artifact. It is 16 bytes, begins
// and ends with a newline so it is visible in a hex dump, and contains a NUL so
// it cannot be produced by a text editor writing to the end of the file.
const bundleMagic = "\n\x00facet-app-v1\x00\n"

// bundleTrailerLen is the fixed footer: an 8-byte big-endian payload length
// followed by the magic. Fixed width is what makes the payload findable from the
// end of a file whose beginning is an arbitrary executable.
const bundleTrailerLen = 8 + len(bundleMagic)

// appBundle is what a release binary carries: the compiled application graph,
// plus the provenance needed to answer "what is this file?" without running it.
//
// The IR is the only field the runtime needs. The rest exists so `<app> version`
// can say which toolchain built it and from what, which is the first question
// asked of any artifact found on a server.
type appBundle struct {
	Format    int    `json:"format"`    // bundle format version (1)
	Toolchain string `json:"toolchain"` // registry.ToolchainVersion of the builder
	App       string `json:"app"`       // the declared app identifier
	Entry     string `json:"entry"`     // the entry .fct it was compiled from
	Built     string `json:"built"`     // RFC3339 build time
	IR        *ir.IR `json:"ir"`        // the compiled application graph
}

// openBundle reads the bundle appended to path, if there is one. It returns the
// bundle (nil when the file is a plain executable), and baseLen: how many bytes
// at the head of the file are the executable itself. baseLen is the whole file
// when there is no bundle, which is exactly what a caller wants when using the
// file as a base — so building a release *from* a release binary rebases cleanly
// instead of stacking payloads.
func openBundle(path string) (b *appBundle, baseLen int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	if size < int64(bundleTrailerLen) {
		return nil, size, nil
	}
	tr := make([]byte, bundleTrailerLen)
	if _, err := f.ReadAt(tr, size-int64(bundleTrailerLen)); err != nil {
		return nil, size, err
	}
	if string(tr[8:]) != bundleMagic {
		return nil, size, nil // a plain binary: usable as a base, carries no app
	}
	n := int64(binary.BigEndian.Uint64(tr[:8]))
	start := size - int64(bundleTrailerLen) - n
	if n <= 0 || start <= 0 {
		return nil, size, fmt.Errorf("bundle trailer claims a %d-byte payload in a %d-byte file", n, size)
	}
	payload := make([]byte, n)
	if _, err := f.ReadAt(payload, start); err != nil {
		return nil, start, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, start, fmt.Errorf("bundle payload is not readable: %w", err)
	}
	defer gz.Close()
	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, start, fmt.Errorf("bundle payload is truncated: %w", err)
	}
	b = &appBundle{}
	if err := json.Unmarshal(raw, b); err != nil {
		return nil, start, fmt.Errorf("bundle payload is not a bundle: %w", err)
	}
	if b.IR == nil {
		return nil, start, fmt.Errorf("bundle carries no application graph")
	}
	if b.Format != 1 {
		return nil, start, fmt.Errorf("bundle format %d — this facet (%s) understands format 1",
			b.Format, registry.ToolchainVersion)
	}
	return b, start, nil
}

// writeRelease copies the first baseLen bytes of basePath into outPath and
// appends b as a gzipped payload plus the trailer. The result is the base
// executable, plus the app, and is marked executable.
func writeRelease(basePath string, baseLen int64, outPath string, b *appBundle) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&payload, gzip.BestCompression)
	if _, err := gz.Write(raw); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	src, err := os.Open(basePath)
	if err != nil {
		return err
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	// Written to a temp name and renamed into place: a half-copied executable
	// that is 15MB of correct machine code and no trailer is a binary that runs
	// as the toolchain, which is a far more confusing failure than a missing file.
	tmp := outPath + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, io.LimitReader(src, baseLen)); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := dst.Write(payload.Bytes()); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	var trailer [8]byte
	binary.BigEndian.PutUint64(trailer[:], uint64(payload.Len()))
	if _, err := dst.Write(trailer[:]); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := dst.WriteString(bundleMagic); err != nil {
		dst.Close()
		os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return resign(basePath, outPath)
}

// resign repairs the code signature appending necessarily broke. Only Mach-O
// (macOS) validates it: a Go binary for darwin carries an ad-hoc signature, and
// the kernel refuses to exec a Mach-O whose contents no longer match it, so
// without this step a macOS release artifact dies with "Killed: 9".
//
// `codesign` ships with macOS, so this succeeds on the machine where it matters
// and is simply absent when cross-building from Linux — in which case the
// artifact must be signed on a Mac before it will run, and saying so is the only
// honest thing to do.
func resign(basePath, outPath string) error {
	if !isMachO(basePath) {
		return nil
	}
	tool, err := exec.LookPath("codesign")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"facet: warning — %s is a macOS binary and appending the app invalidated its signature.\n"+
				"       `codesign` is not on this machine, so it could not be re-signed here. On a Mac, run:\n"+
				"         codesign --force --sign - %s\n", filepath.Base(outPath), outPath)
		return nil
	}
	cmd := exec.Command(tool, "--force", "--sign", "-", outPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("re-signing %s failed (a macOS binary will not run unsigned): %w", outPath, err)
	}
	return nil
}

// isMachO reports whether path begins with a Mach-O (or universal) magic number.
// The base file's own bytes are asked rather than the host's GOOS, because a
// cross-build's target is the base, not the machine doing the building.
func isMachO(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var m [4]byte
	if _, err := io.ReadFull(f, m[:]); err != nil {
		return false
	}
	switch string(m[:]) {
	case "\xcf\xfa\xed\xfe", "\xce\xfa\xed\xfe", // 64- and 32-bit Mach-O, little-endian
		"\xca\xfe\xba\xbe", "\xbe\xba\xfe\xca": // universal ("fat") binaries
		return true
	}
	return false
}

// isPE reports whether path is a Windows executable, so the artifact can be
// given the .exe suffix Windows needs to run it.
func isPE(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var m [2]byte
	if _, err := io.ReadFull(f, m[:]); err != nil {
		return false
	}
	return string(m[:]) == "MZ"
}

// cmdRelease implements `facet build --release <file.fct> [-o out] [--base bin]`.
// graph is already compiled by main; this only packages it.
func cmdRelease(graph *ir.IR, entry string, args []string) int {
	out, base := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "--out":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "facet: -o needs a path")
				return 2
			}
			i++
			out = args[i]
		case strings.HasPrefix(a, "--out="):
			out = strings.TrimPrefix(a, "--out=")
		case a == "--base":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "facet: --base needs a path to a facet binary")
				return 2
			}
			i++
			base = args[i]
		case strings.HasPrefix(a, "--base="):
			base = strings.TrimPrefix(a, "--base=")
		}
	}

	if base == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "facet:", err)
			return 1
		}
		// A symlinked `facet` on PATH is the norm; the bytes are what get copied.
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		base = exe
	}
	// Reading the base tells us two things at once: how much of it is executable
	// (so an already-released binary rebases instead of stacking payloads), and
	// whether it is a facet binary at all.
	existing, baseLen, err := openBundle(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet: base %s: %v\n", base, err)
		return 1
	}
	if existing != nil && existing.Toolchain != registry.ToolchainVersion {
		fmt.Fprintf(os.Stderr, "facet: note — base %s carries an app built by facet %s; only its executable half is used\n",
			base, existing.Toolchain)
	}

	if out == "" {
		out = filepath.Join("dist", strings.ToLower(graph.App))
		if isPE(base) {
			out += ".exe"
		}
	}

	bundle := &appBundle{
		Format:    1,
		Toolchain: registry.ToolchainVersion,
		App:       graph.App,
		Entry:     filepath.Base(entry),
		Built:     time.Now().UTC().Format(time.RFC3339),
		IR:        graph,
	}
	if err := writeRelease(base, baseLen, out, bundle); err != nil {
		fmt.Fprintln(os.Stderr, "facet:", err)
		return 1
	}

	// Prove it: the artifact is read back the way it will read itself at startup.
	// A release that cannot be opened is worse than a failed build, because it
	// fails on the server instead of here.
	back, _, err := openBundle(out)
	if err != nil || back == nil {
		fmt.Fprintf(os.Stderr, "facet: wrote %s but could not read the app back out of it: %v\n", out, err)
		return 1
	}
	fi, _ := os.Stat(out)

	fmt.Printf("facet: release built — %s (%s)\n", out, humanSize(fi.Size()))
	fmt.Printf("  app         %s — %d entities, %d actions, %d views, %d routes\n",
		back.IR.App, len(back.IR.Entities), len(back.IR.Actions), len(back.IR.Pages), len(back.IR.Routes))
	fmt.Printf("  toolchain   facet %s (runtime + client embedded)\n", back.Toolchain)
	fmt.Printf("  carries     the compiled graph — no source tree, no compiler, no interpreter at run time\n")
	fmt.Println("\nrun it:")
	fmt.Printf("  %s serve --port 7373      serve the app (needs FACET_DATABASE_URL + FACET_SECRET)\n", runPath(out))
	fmt.Printf("  %s doctor                 production readiness of this artifact, here\n", runPath(out))
	fmt.Printf("  %s migrate                reconcile the datastore schema\n", runPath(out))
	fmt.Println("\ncontainer + systemd for it:")
	fmt.Printf("  facet deploy %s --production\n", entry)
	return 0
}

// runPath renders a path the way it has to be typed to execute it.
func runPath(p string) string {
	if filepath.IsAbs(p) || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
		return p
	}
	return "./" + p
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ── the artifact's own command line ──────────────────────────────────────────

// embeddedApp returns the app this executable carries, if it carries one. It is
// the first thing main asks, because the answer decides which program this is:
// the toolchain, or one built app.
func embeddedApp() (*appBundle, bool) {
	exe, err := os.Executable()
	if err != nil {
		return nil, false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	b, _, err := openBundle(exe)
	if err != nil {
		// The trailer is present and the payload is not readable: this binary was
		// built as an app and is corrupt. Falling back to the toolchain CLI would
		// hide that behind a usage message.
		fmt.Fprintln(os.Stderr, "facet:", err)
		os.Exit(1)
	}
	return b, b != nil
}

// runApp is the whole command line of a built app. It is deliberately narrow:
// an artifact serves, reports what it is, and performs the two operational tasks
// that would otherwise send an operator back for the source tree — reconciling
// the schema, and diagnosing the deployment.
func runApp(b *appBundle, args []string) int {
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "", "serve", "run":
		return serveGraph(b.IR, args, describeBundle(b))
	case "version", "-v", "--version":
		fmt.Printf("%s — built by facet %s from %s on %s\n", b.App, b.Toolchain, b.Entry, b.Built)
		return 0
	case "routes":
		report := RouteReport{App: b.IR.App, Routes: buildRoutes(b.IR, hasFlag(args, "--all", "-all"))}
		if hasFlag(args, "--json", "-json") {
			out, _ := json.MarshalIndent(report, "", "  ")
			fmt.Println(string(out))
			return 0
		}
		writeRoutesText(os.Stdout, report)
		return 0
	case "ir":
		out, err := marshalIR(b.IR, hasFlag(args, "--compact", "-compact"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "facet:", err)
			return 1
		}
		fmt.Println(string(out))
		return 0
	case "inspect":
		report := buildInspect(b.IR)
		if hasFlag(args, "--json", "-json") {
			out, _ := json.MarshalIndent(report, "", "  ")
			fmt.Println(string(out))
			return 0
		}
		writeInspectText(os.Stdout, report)
		return 0
	case "doctor":
		// An artifact has no source tree, so the checks that read one (the
		// manifest, the lock, .gitignore) have nothing to read and are skipped;
		// everything about the *deployment* still applies, and production is the
		// only mode an artifact is ever in.
		return runDoctor(doctorOpts{subject: describeBundle(b), graph: b.IR, production: true})
	case "config":
		fmt.Print(runtime.ResolveConfig().Report())
		return 0
	case "migrate":
		return migrateGraph(b.IR, !hasFlag(args, "--plan", "-plan"))
	case "healthcheck":
		return healthcheck(args)
	default:
		appUsage(b)
		return 2
	}
}

// describeBundle names the artifact for banners and diagnostics.
func describeBundle(b *appBundle) string {
	return fmt.Sprintf("%s (built by facet %s from %s)", b.App, b.Toolchain, b.Entry)
}

// migrateGraph reconciles the datastore schema with a graph — the same call
// `facet migrate <file.fct>` makes, reached here without a source file because
// the artifact carries the graph.
func migrateGraph(graph *ir.IR, apply bool) int {
	plan, err := runtime.Migrate(graph, apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "facet:", err)
		return 1
	}
	if len(plan) == 0 {
		fmt.Println("facet: schema is up to date — nothing to migrate")
		return 0
	}
	if apply {
		fmt.Printf("facet: applied %d schema change(s):\n", len(plan))
	} else {
		fmt.Printf("facet: %d pending schema change(s) (dry run):\n", len(plan))
	}
	for _, stmt := range plan {
		fmt.Printf("  %s\n", stmt)
	}
	return 0
}

// healthcheck probes this app's own /healthz over the loopback interface and
// exits 0 or 1 — the shape `HEALTHCHECK CMD` and a systemd watchdog want.
//
// It exists because the production image is distroless: there is no curl, no
// wget and no shell in it, so the only program available to make an HTTP request
// is the app itself. It resolves the address exactly as `serve` does, so a
// container that moved its port does not need its health check edited too.
func healthcheck(args []string) int {
	addr := resolveAddr(args)
	url := "http://" + localAddr(addr) + "/healthz"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	fmt.Println("healthy")
	return 0
}

// localAddr turns a listen address into one that can be dialed from inside the
// same container: a bare or wildcard host is this machine.
func localAddr(addr string) string {
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return "127.0.0.1:" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return host + ":" + port
}

func appUsage(b *appBundle) {
	fmt.Fprintf(os.Stderr, "%s — a Facet app, built by facet %s. One binary: no source, no toolchain.\n\n", b.App, b.Toolchain)
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  serve [--port N] [--addr host:port]  serve the app (the default with no arguments)")
	fmt.Fprintln(os.Stderr, "  migrate [--plan]                     reconcile the datastore schema")
	fmt.Fprintln(os.Stderr, "  doctor                               production readiness of this deployment")
	fmt.Fprintln(os.Stderr, "  routes [--json] [--all]              every route this app serves")
	fmt.Fprintln(os.Stderr, "  inspect [--json] | ir [--compact]    what it compiles to / the graph itself")
	fmt.Fprintln(os.Stderr, "  config                               the resolved configuration")
	fmt.Fprintln(os.Stderr, "  healthcheck                          probe /healthz; exit 0 when healthy")
	fmt.Fprintln(os.Stderr, "  version                              what this artifact is and what built it")
}
