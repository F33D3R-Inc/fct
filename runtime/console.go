package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"facet/internal/ir"
)

// `facet console` is an interactive REPL against a live app: evaluate Facet
// expressions (entity reads, aggregates, derives, builtins), run actions, and
// inspect state — all through the same runtime the server uses. With no
// FACET_DATABASE_URL it runs on the in-memory store, so it is a zero-setup
// scratchpad; with one set it is a read/write shell onto real data.
//
// Commands:
//
//	<expression>            evaluate and print a Facet expression
//	:run name(arg, …)       run an action (as the current identity)
//	:as <actor> [role]      act as a user (default: console/admin)
//	:entities               list entities and their row counts
//	:get Entity [n]         print up to n rows of an entity
//	:actions                list the runnable actions
//	:state                  print the current state cells
//	:help                   show this help
//	:quit                   exit (or Ctrl-D)

// Console runs the REPL until EOF or :quit.
func Console(graph *ir.IR) error {
	var srv *Server
	var err error
	if os.Getenv("FACET_DATABASE_URL") != "" {
		srv, err = New(graph)
	} else {
		fmt.Fprintln(os.Stderr, "facet console: FACET_DATABASE_URL not set — using the in-memory store (changes are volatile)")
		srv, err = NewInMemory(graph)
	}
	if err != nil {
		return err
	}
	defer srv.Shutdown()

	actor, role := "console", "admin"
	out := os.Stdout
	fmt.Fprintf(out, "facet console — %s. Type :help for commands, :quit to exit.\n", graph.App)

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	fmt.Fprint(out, "» ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Fprint(out, "» ")
			continue
		}
		if strings.HasPrefix(line, ":") {
			if quit := srv.consoleCmd(out, line, &actor, &role); quit {
				return nil
			}
			fmt.Fprint(out, "» ")
			continue
		}
		// a bare line is an expression to evaluate.
		e, err := ir.CompileExpr(graph, line)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
		} else {
			printVal(out, srv.EvalExpr(e, actor, role, true))
		}
		fmt.Fprint(out, "» ")
	}
	fmt.Fprintln(out)
	return sc.Err()
}

// consoleCmd handles a ":" command. It returns true when the REPL should exit.
func (s *Server) consoleCmd(out io.Writer, line string, actor, role *string) bool {
	args := strings.Fields(line)
	switch args[0] {
	case ":quit", ":q", ":exit":
		return true
	case ":help", ":h", ":?":
		fmt.Fprint(out, consoleHelp)
	case ":as":
		if len(args) < 2 {
			fmt.Fprintf(out, "acting as %s (%s)\n", *actor, *role)
			return false
		}
		*actor = args[1]
		if len(args) >= 3 {
			*role = args[2]
		} else {
			*role = "member"
		}
		fmt.Fprintf(out, "now acting as %s (%s)\n", *actor, *role)
	case ":entities":
		names := s.EntityNames()
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(out, "  %-20s %d row(s)\n", n, len(s.EntityRows(n)))
		}
	case ":actions":
		for _, a := range s.ir.Actions {
			ps := make([]string, len(a.Params))
			for i, p := range a.Params {
				ps[i] = p.Name + ": " + p.Type
			}
			fmt.Fprintf(out, "  %s(%s)  [%s]\n", a.Name, strings.Join(ps, ", "), a.Placement)
		}
	case ":state":
		for _, st := range s.ir.States {
			fmt.Fprintf(out, "  %-20s = ", st.Name)
			printVal(out, s.StateValue(st.Name))
		}
	case ":get":
		if len(args) < 2 {
			fmt.Fprintln(out, "usage: :get Entity [n]")
			return false
		}
		rows := s.EntityRows(args[1])
		limit := len(rows)
		if len(args) >= 3 {
			if n := atoiSafe(args[2]); n > 0 && n < limit {
				limit = n
			}
		}
		for _, r := range rows[:limit] {
			printVal(out, r)
		}
		fmt.Fprintf(out, "  (%d row(s))\n", len(rows))
	case ":run":
		rest := strings.TrimSpace(strings.TrimPrefix(line, ":run"))
		s.consoleRun(out, rest, *actor, *role)
	default:
		fmt.Fprintf(out, "unknown command %q — :help for the list\n", args[0])
	}
	return false
}

// consoleRun parses and executes `name(arg, …)` (or a bare `name`) as an action.
func (s *Server) consoleRun(out io.Writer, call, actor, role string) {
	name := call
	var argSrcs []string
	if open := strings.IndexByte(call, '('); open >= 0 {
		name = strings.TrimSpace(call[:open])
		closeIdx := strings.LastIndexByte(call, ')')
		if closeIdx < open {
			fmt.Fprintln(out, "error: missing `)` in action call")
			return
		}
		inner := strings.TrimSpace(call[open+1 : closeIdx])
		if inner != "" {
			argSrcs = splitArgs(inner)
		}
	}
	var vals []any
	for _, src := range argSrcs {
		e, err := ir.CompileExpr(s.ir, strings.TrimSpace(src))
		if err != nil {
			fmt.Fprintf(out, "error in argument %q: %v\n", src, err)
			return
		}
		vals = append(vals, s.EvalExpr(e, actor, role, true))
	}
	deltas, err := s.Run(actor, role, true, name, vals)
	if err != nil {
		fmt.Fprintf(out, "rejected: %v\n", err)
		return
	}
	if len(deltas) == 0 {
		fmt.Fprintln(out, "ok")
		return
	}
	fmt.Fprintln(out, "ok — changed:")
	for k, v := range deltas {
		fmt.Fprintf(out, "  %-20s = ", k)
		printVal(out, v)
	}
}

// splitArgs splits an argument list on top-level commas (outside quotes/parens).
func splitArgs(s string) []string {
	var out []string
	depth, inStr, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func printVal(out io.Writer, v any) {
	switch t := v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(t)
		fmt.Fprintln(out, string(b))
	case string:
		fmt.Fprintf(out, "%q\n", t)
	case nil:
		fmt.Fprintln(out, "nil")
	default:
		fmt.Fprintf(out, "%v\n", t)
	}
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

const consoleHelp = `commands:
  <expression>          evaluate and print a Facet expression (e.g. count(Post))
  :run name(arg, …)     run an action as the current identity
  :as <actor> [role]    act as a user (default console/admin)
  :entities             list entities and row counts
  :get Entity [n]       print up to n rows
  :actions              list runnable actions
  :state                print state cells
  :help                 this help
  :quit                 exit
`
