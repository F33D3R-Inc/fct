package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"

	"facet/internal/compile"
	"facet/runtime"
)

// cmdExec runs a program as a command: `facet exec file.fct [args...]`
// compiles file.fct, calls its `proc main(args: [text]) -> int` with the
// remaining arguments, and exits with the int main returns. A program with
// no main but with daemons is a service: its daemons run until they end. main talks to the
// world through the io.console builtins (writeStdout/writeStderr/readStdin,
// runtime/stdio.go) and whatever other capabilities it declares; no HTTP
// server is started and no data directory is opened.
func cmdExec(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: facet exec <file.fct> [args...]")
		return 2
	}
	return execProgram(args[0], args[1:], execOptions{})
}

// execOptions is what a toolchain command that runs a program it ships
// (`facet facetql`) decides for it beyond `facet exec`'s defaults.
type execOptions struct {
	// root, when set, is the io.file sandbox root whatever FACET_DATA_DIR says.
	root string
	// dirs and files are granted to the program as its operator would grant
	// them (runtime.Server.GrantDir / GrantFile).
	dirs, files []string
}

// execProgram is `facet exec file args...` with opts applied.
func execProgram(file string, args []string, opts execOptions) int {
	graph, err := compile.File(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile error: %v\n", err)
		return 1
	}
	srv, err := runtime.NewProgram(graph)
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
		return 1
	}
	// FACET_CPUPROFILE=<path>: a CPU profile of the whole run (the
	// evaluator and the program it runs), written when the program ends —
	// by returning from main, by exitProcess, or by its daemons ending.
	// How the interpretive cost of a long-running program (the self-hosted
	// FacetQL engine, say) is measured, with `go tool pprof`.
	if path := os.Getenv("FACET_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "facet exec: FACET_CPUPROFILE: %v\n", err)
			return 1
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "facet exec: FACET_CPUPROFILE: %v\n", err)
			return 1
		}
		stop := func() {
			pprof.StopCPUProfile()
			f.Close()
			// FACET_MEMPROFILE=<path>, beside it: the allocation profile of
			// the same run (`go tool pprof -sample_index=alloc_space`).
			if mp := os.Getenv("FACET_MEMPROFILE"); mp != "" {
				if mf, err := os.Create(mp); err == nil {
					_ = pprof.Lookup("allocs").WriteTo(mf, 0)
					mf.Close()
				}
			}
		}
		defer stop()
		srv.SetProfiling(true)
		srv.SetExit(func(code int) {
			stop()
			os.Exit(code)
		})
	}
	// FACET_PROC_LABELS=1: every goroutine carries the fct proc it is
	// running as its `proc` label (what a CPU profile's -tags reads), so a
	// goroutine dump names the program's tasks rather than the evaluator's
	// frames. SIGUSR1 then writes that dump — every goroutine, grouped, with
	// its labels — to stderr and the program runs on: how a long-running
	// program (the FacetQL engine, a daemon) is asked what it is doing while
	// it is doing it.
	if os.Getenv("FACET_PROC_LABELS") == "1" {
		srv.SetProfiling(true)
	}
	dumpGoroutinesOnSIGUSR1()
	if opts.root != "" {
		srv.SetDataDir(opts.root)
	} else if os.Getenv("FACET_DATA_DIR") == "" {
		if cwd, err := os.Getwd(); err == nil {
			srv.SetDataDir(cwd)
		}
	}
	for _, d := range opts.dirs {
		if err := srv.GrantDir(d); err != nil {
			fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
			return 1
		}
	}
	for _, f := range opts.files {
		if err := srv.GrantFile(f); err != nil {
			fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
			return 1
		}
	}
	if !srv.HasMain() && len(graph.Daemons) > 0 {
		// A service: its daemons run for the life of the process.
		if err := srv.RunDaemons(); err != nil {
			fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
			return 1
		}
		return 0
	}
	code, err := srv.RunMain(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
		return 1
	}
	return code
}

// dumpGoroutinesOnSIGUSR1 answers every SIGUSR1 with a goroutine dump on
// stderr, between marker lines, and nothing else: the process carries on.
func dumpGoroutinesOnSIGUSR1() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	go func() {
		for range c {
			fmt.Fprintf(os.Stderr, "=== goroutine dump (SIGUSR1) at %s ===\n", time.Now().Format("15:04:05.000"))
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
			fmt.Fprintln(os.Stderr, "=== end of goroutine dump ===")
		}
	}()
}
