package main

import (
	"fmt"
	"os"
	"runtime/pprof"

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
	graph, err := compile.File(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile error: %v\n", err)
		return 1
	}
	srv, err := runtime.NewInMemory(graph)
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
	if os.Getenv("FACET_DATA_DIR") == "" {
		if cwd, err := os.Getwd(); err == nil {
			srv.SetDataDir(cwd)
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
	code, err := srv.RunMain(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "facet exec: %v\n", err)
		return 1
	}
	return code
}
