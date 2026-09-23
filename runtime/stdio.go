package runtime

// A program as a process: `facet exec file.fct args...` runs the program's
// `proc main(args: [text]) -> int` once, with the process's own argv, stdin,
// stdout and stderr, and exits with the int it returns. This is what a
// command-line tool written in fct (fabric's operator CLI, a daemon's
// `main.rs`) needs and a request/response server does not: there is no HTTP
// surface, no store traffic and no client — main is an ordinary proc run on
// the calling goroutine, exactly as a daemon body runs its procs.
//
// The stdio builtins — writeStdout(text) -> bool, writeStderr(text) -> bool,
// readStdin() -> text — are the io.console capability: proc-only and gated
// by `uses io.console` like every other I/O builtin (internal/ir/build.go's
// builtinCapability), because a server process's stdout is its operator log
// and must not be something any proc can write to by accident. print() stays
// what it is, a tagged debugging line.

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// stdio is the process streams the io.console builtins read and write.
// A Server built for `facet exec` points them at os.Stdin/Stdout/Stderr; a
// test points them at buffers. The mutex keeps two procs (a spawned pair,
// say) from interleaving inside one write.
type stdio struct {
	mu         sync.Mutex
	in         io.Reader
	out        io.Writer
	errOut     io.Writer
	signalSubs []int // signals() channels, each sent every signal's name
}

func newStdio() *stdio {
	return &stdio{in: os.Stdin, out: os.Stdout, errOut: os.Stderr}
}

// SetStdio redirects the io.console builtins (used by tests and embedders).
func (s *Server) SetStdio(in io.Reader, out, errOut io.Writer) {
	s.console.mu.Lock()
	defer s.console.mu.Unlock()
	s.console.in, s.console.out, s.console.errOut = in, out, errOut
}

func (s *Server) ioWriteStdout(text string) (any, error) {
	s.console.mu.Lock()
	defer s.console.mu.Unlock()
	if _, err := io.WriteString(s.console.out, text); err != nil {
		return nil, fmt.Errorf("writeStdout: %v", err)
	}
	return true, nil
}

func (s *Server) ioWriteStderr(text string) (any, error) {
	s.console.mu.Lock()
	defer s.console.mu.Unlock()
	if _, err := io.WriteString(s.console.errOut, text); err != nil {
		return nil, fmt.Errorf("writeStderr: %v", err)
	}
	return true, nil
}

// ioReadStdin reads standard input to its end, once; a second call sees
// what is left, which after the first is nothing.
func (s *Server) ioReadStdin() (any, error) {
	s.console.mu.Lock()
	defer s.console.mu.Unlock()
	b, err := io.ReadAll(s.console.in)
	if err != nil {
		return nil, fmt.Errorf("readStdin: %v", err)
	}
	return string(b), nil
}

// SetDataDir moves the io.file sandbox root. `facet exec` roots it at the
// directory the command was run from (unless FACET_DATA_DIR says otherwise),
// because a command's file arguments are relative to where its operator is.
func (s *Server) SetDataDir(dir string) {
	s.dataDir = dir
}

// RunMain runs the program's `proc main(args: [text]) -> int` with args and
// returns its exit code. A program without that entry point is an error, as
// is a main whose signature is anything else.
func (s *Server) RunMain(args []string) (int, error) {
	p := s.byProc["main"]
	if p == nil {
		return 0, fmt.Errorf("%s has no `proc main(args: [text]) -> int` to execute", s.ir.App)
	}
	if len(p.Params) != 1 || !p.Params[0].List || p.Params[0].Type != "text" || p.Ret != "int" || p.RetList {
		return 0, fmt.Errorf("`main` must be declared `proc main(args: [text]) -> int`")
	}
	s.grants.setArgv(args)
	argv := make([]any, len(args))
	for i, a := range args {
		argv[i] = a
	}
	v, err := s.runProcLocked(p, []any{argv})
	if err != nil {
		return 0, err
	}
	return toInt(v), nil
}

// ioSignals implements `signals() -> int` (io.console): a channel handle
// (the same kind channel() mints) that receives "SIGINT" or "SIGTERM" each
// time the process is sent one. Once a program asks for them, those signals
// no longer end the process on their own — the program decides, which is
// what a daemon that drains before it exits needs.
func (s *Server) ioSignals() (any, error) {
	id := s.channels.create()
	s.console.mu.Lock()
	s.console.signalSubs = append(s.console.signalSubs, id)
	first := len(s.console.signalSubs) == 1
	s.console.mu.Unlock()
	if first {
		c := make(chan os.Signal, 4)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			for sig := range c {
				name := "SIGTERM"
				if sig == syscall.SIGINT {
					name = "SIGINT"
				}
				s.deliverSignal(name)
			}
		}()
	}
	return id, nil
}

// deliverSignal hands one signal's name to every signals() channel.
func (s *Server) deliverSignal(name string) {
	s.console.mu.Lock()
	subs := append([]int(nil), s.console.signalSubs...)
	s.console.mu.Unlock()
	for _, id := range subs {
		s.channels.send(id, name)
	}
}
