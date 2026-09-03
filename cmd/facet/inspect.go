package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"facet/internal/ir"
	"facet/internal/parser"
)

// This file adds three developer-facing, read-only commands that expose what the
// compiler already produces, plus machine-readable diagnostics — all backed by
// real capability (the IR is the compiler's terminal artifact):
//
//	facet check   <file.fct> [--json]   compile-only; report success or diagnostics
//	facet ir      <file.fct> [--compact] dump the compiled IR (JSON, the terminal artifact)
//	facet inspect <file.fct> [--json]   structured summary of what the app compiles to
//
// None of them start a server, touch the database, or write files. `check` and
// the shared compile-error path can emit diagnostics as JSON (--json) for editors
// and automation, reusing the compiler's own parser.Error / ir.BuildError types —
// there is no second diagnostics system.

// Diagnostic is one compiler message with its source location. It is built from
// the compiler's structured error types (parser.Error, ir.BuildError); it is a
// serialization shape, not a new error system.
type Diagnostic struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // always "error" today (the compiler stops at the first)
	Message  string `json:"message"`
}

// diagnosticsFor turns a compile error into structured diagnostics. The compiler
// reports one error at a time, so this yields a single-element slice; returning a
// slice keeps the JSON shape stable if that ever changes. file is the entry path
// as the user typed it, attached for editor/automation consumption.
func diagnosticsFor(file string, err error) []Diagnostic {
	if err == nil {
		return nil
	}
	line, msg := errLocation(err)
	return []Diagnostic{{File: file, Line: line, Severity: "error", Message: msg}}
}

// errLocation extracts a 1-based line and bare message from a compiler error,
// unwrapping compile.File's "<file>: <err>" wrapping via errors.As so the typed
// line survives. It mirrors the LSP's errLine, kept local so the CLI does not
// depend on the lsp package.
func errLocation(err error) (int, string) {
	var pe *parser.Error
	if errors.As(err, &pe) {
		return pe.Line, pe.Msg
	}
	var be *ir.BuildError
	if errors.As(err, &be) {
		return be.Line, be.Msg
	}
	// Fall back: strip a leading "line N: " prefix the error string may carry.
	msg := err.Error()
	if rest, ok := strings.CutPrefix(msg, "line "); ok {
		if colon := strings.IndexByte(rest, ':'); colon > 0 {
			n := 0
			if _, e := fmt.Sscanf(strings.TrimSpace(rest[:colon]), "%d", &n); e == nil && n > 0 {
				return n, strings.TrimSpace(rest[colon+1:])
			}
		}
	}
	return 0, msg
}

// InspectReport is a structured summary of a compiled app: what it compiles to,
// derived entirely from the IR. It is the human-and-machine view of `facet build`'s
// raw graph — counts and placements without the full node trees.
type InspectReport struct {
	App      string           `json:"app"`
	Auth     bool             `json:"auth"`
	Entities []EntitySummary  `json:"entities"`
	States   []StateSummary   `json:"states"`
	Actions  []ActionSummary  `json:"actions"`
	Views    []ViewSummary    `json:"views"`
	Routes   []ir.Route       `json:"routes,omitempty"`
	Services []ServiceSummary `json:"services,omitempty"`
	Jobs     []JobSummary     `json:"jobs,omitempty"`
	Policies []string         `json:"policies,omitempty"`
	Counts   map[string]int   `json:"counts"`
}

type EntitySummary struct {
	Name       string `json:"name"`
	Fields     int    `json:"fields"`
	SoftDelete bool   `json:"softDelete,omitempty"`
}

type StateSummary struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Placement string `json:"placement"`
	Private   bool   `json:"private,omitempty"`
}

type ActionSummary struct {
	Name      string   `json:"name"`
	Placement string   `json:"placement"`
	Reason    string   `json:"reason,omitempty"`
	Reads     []string `json:"reads,omitempty"`
	Writes    []string `json:"writes,omitempty"`
}

type ViewSummary struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Requires string   `json:"requires,omitempty"`
	Params   []string `json:"params,omitempty"`
}

type ServiceSummary struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Ops  int    `json:"ops"`
}

type JobSummary struct {
	Name    string `json:"name"`
	Action  string `json:"action"`
	Every   int    `json:"every,omitempty"`
	OnStart bool   `json:"onStart,omitempty"`
}

// buildInspect projects the IR into a summary. It reads only fields the IR already
// computed (placement, reasons, reads/writes, routes) — it derives nothing new.
func buildInspect(g *ir.IR) InspectReport {
	r := InspectReport{App: g.App, Auth: g.Auth, Routes: g.Routes}
	for _, e := range g.Entities {
		r.Entities = append(r.Entities, EntitySummary{Name: e.Name, Fields: len(e.Fields), SoftDelete: e.SoftDelete})
	}
	for _, s := range g.States {
		r.States = append(r.States, StateSummary{Name: s.Name, Type: stateType(s), Placement: s.Placement, Private: s.Private})
	}
	for _, a := range g.Actions {
		r.Actions = append(r.Actions, ActionSummary{Name: a.Name, Placement: a.Placement, Reason: a.Reason, Reads: a.Reads, Writes: a.Writes})
	}
	for _, p := range g.Pages {
		r.Views = append(r.Views, ViewSummary{Name: p.Name, Path: p.Path, Requires: p.Requires, Params: p.Params})
	}
	for _, s := range g.Services {
		r.Services = append(r.Services, ServiceSummary{Name: s.Name, URL: s.URL, Ops: len(s.Ops)})
	}
	for _, j := range g.Jobs {
		r.Jobs = append(r.Jobs, JobSummary{Name: j.Name, Action: j.Action, Every: j.Every, OnStart: j.OnStart})
	}
	for _, p := range g.Policies {
		r.Policies = append(r.Policies, p.Name)
	}
	sort.Strings(r.Policies)
	r.Counts = map[string]int{
		"entities": len(g.Entities),
		"states":   len(g.States),
		"actions":  len(g.Actions),
		"views":    len(g.Pages),
		"services": len(g.Services),
		"jobs":     len(g.Jobs),
		"policies": len(g.Policies),
		"webhooks": len(g.Webhooks),
		"triggers": len(g.Triggers),
	}
	return r
}

// stateType renders a state cell's type for display: `[elem]` for a list cell,
// otherwise the scalar type, with a trailing `?` for an optional cell.
func stateType(s ir.State) string {
	t := s.Type
	if s.List {
		t = "[" + s.Elem + "]"
	}
	if s.Optional {
		t += "?"
	}
	return t
}

// writeInspectText prints the summary in a readable, columnar form — the same
// house style as `facet explain`.
func writeInspectText(w io.Writer, r InspectReport) {
	fmt.Fprintf(w, "%s — what this app compiles to\n", r.App)
	auth := "off"
	if r.Auth {
		auth = "on (built-in users, actor/role)"
	}
	fmt.Fprintf(w, "  auth: %s\n", auth)

	if len(r.Entities) > 0 {
		fmt.Fprintln(w, "\nENTITIES (durable data)")
		for _, e := range r.Entities {
			soft := ""
			if e.SoftDelete {
				soft = "  (soft-delete)"
			}
			fmt.Fprintf(w, "  %-20s %d field(s)%s\n", e.Name, e.Fields, soft)
		}
	}
	if len(r.States) > 0 {
		fmt.Fprintln(w, "\nSTATE (placement)")
		for _, s := range r.States {
			tag := ""
			if s.Private {
				tag = "  @private"
			}
			fmt.Fprintf(w, "  %-20s %-8s %s%s\n", s.Name, strings.ToUpper(s.Placement), s.Type, tag)
		}
	}
	if len(r.Actions) > 0 {
		fmt.Fprintln(w, "\nACTIONS (placement + why)")
		for _, a := range r.Actions {
			fmt.Fprintf(w, "  %-20s %-8s %s\n", a.Name, strings.ToUpper(a.Placement), a.Reason)
		}
	}
	if len(r.Views) > 0 {
		fmt.Fprintln(w, "\nVIEWS (routes)")
		for _, v := range r.Views {
			req := ""
			if v.Requires != "" {
				req = "  requires " + v.Requires
			}
			fmt.Fprintf(w, "  %-20s %s%s\n", v.Name, v.Path, req)
		}
	}
	if len(r.Services) > 0 {
		fmt.Fprintln(w, "\nSERVICES (external brains)")
		for _, s := range r.Services {
			fmt.Fprintf(w, "  %-20s %s  (%d op)\n", s.Name, s.URL, s.Ops)
		}
	}
	if len(r.Jobs) > 0 {
		fmt.Fprintln(w, "\nJOBS (scheduled)")
		for _, j := range r.Jobs {
			when := fmt.Sprintf("every %ds", j.Every)
			if j.Every == 0 {
				when = "on start"
			}
			fmt.Fprintf(w, "  %-20s -> %s  (%s)\n", j.Name, j.Action, when)
		}
	}
	fmt.Fprintln(w, "\nTOTALS")
	for _, k := range []string{"entities", "states", "actions", "views", "policies", "services", "jobs", "webhooks", "triggers"} {
		if n := r.Counts[k]; n > 0 {
			fmt.Fprintf(w, "  %-10s %d\n", k, n)
		}
	}
}

// marshalIR renders the IR as JSON — indented by default, single-line with
// compact. It is the shared marshaler for `facet ir` (and mirrors `facet build`).
func marshalIR(g *ir.IR, compact bool) ([]byte, error) {
	if compact {
		return json.Marshal(g)
	}
	return json.MarshalIndent(g, "", "  ")
}
