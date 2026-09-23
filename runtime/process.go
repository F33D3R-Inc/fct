package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// The process a program runs as (io.console): ending it with a status, and
// measuring it.

// ioExitProcess implements `exitProcess(code: int) -> bool`: the process
// ends now with that exit status — how a service run by `facet exec`
// reports why it stopped (a refusal to start, a shutdown finished) to
// whatever supervises it. A daemon has no caller to return a status to, so
// this is the only way one can. The status is 0-255, as a process's is.
func (s *Server) ioExitProcess(code int) (any, error) {
	if code < 0 || code > 255 {
		return nil, fmt.Errorf("exitProcess: status %d is out of range (0-255)", code)
	}
	os.Stdout.Sync()
	os.Stderr.Sync()
	s.exit(code)
	return true, nil
}

// processStats is the process section of an operator's stats: the fields,
// names and meaning of facetql's metrics::ProcessStats, which a program
// serving that contract reports verbatim. A field this platform cannot
// measure is null.
type processStats struct {
	CPUSecondsTotal   *float64 `json:"cpu_seconds_total"`
	CPUCores          *int     `json:"cpu_cores"`
	ResidentBytes     *uint64  `json:"resident_bytes"`
	MemoryLimitBytes  *uint64  `json:"memory_limit_bytes"`
	MemoryLimitSource *string  `json:"memory_limit_source"`
	MemoryUtilization *float64 `json:"memory_utilization"`
}

// ioProcessStats implements `processStats() -> text`: a JSON object — CPU
// time consumed (user + system, getrusage), the cores the process may run
// on, resident memory (/proc/self/statm), and the memory ceiling it runs
// under (the cgroup's limit when it is below the machine's, else the
// machine's) with where that came from and the fraction in use.
func (s *Server) ioProcessStats() (any, error) {
	var st processStats
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		secs := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
		v := secs(ru.Utime) + secs(ru.Stime)
		st.CPUSecondsTotal = &v
	}
	cores := runtime.NumCPU()
	st.CPUCores = &cores
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 1 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				v := pages * uint64(os.Getpagesize())
				st.ResidentBytes = &v
			}
		}
	}
	if limit, source, ok := memoryLimit(); ok {
		st.MemoryLimitBytes = &limit
		st.MemoryLimitSource = &source
		if st.ResidentBytes != nil && limit > 0 {
			u := float64(*st.ResidentBytes) / float64(limit)
			if u > 1 {
				u = 1
			}
			st.MemoryUtilization = &u
		}
	}
	out, err := json.Marshal(st)
	if err != nil {
		return nil, fmt.Errorf("processStats: %v", err)
	}
	return string(out), nil
}

var (
	memoryLimitOnce   sync.Once
	memoryLimitBytes  uint64
	memoryLimitSource string
	memoryLimitKnown  bool
)

// memoryLimit probes once: the system's MemTotal, and a cgroup v2 (or v1)
// limit, which wins when it is the smaller.
func memoryLimit() (uint64, string, bool) {
	memoryLimitOnce.Do(func() {
		parse := func(path string) (uint64, bool) {
			raw, err := os.ReadFile(path)
			if err != nil {
				return 0, false
			}
			v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
			return v, err == nil
		}
		var system uint64
		haveSystem := false
		if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					if f := strings.Fields(line); len(f) > 1 {
						if kb, err := strconv.ParseUint(f[1], 10, 64); err == nil {
							system, haveSystem = kb*1024, true
						}
					}
				}
			}
		}
		cgroup, haveCgroup := parse("/sys/fs/cgroup/memory.max")
		if !haveCgroup {
			cgroup, haveCgroup = parse("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		}
		switch {
		case haveCgroup && haveSystem && cgroup < system:
			memoryLimitBytes, memoryLimitSource, memoryLimitKnown = cgroup, "cgroup", true
		case haveSystem:
			memoryLimitBytes, memoryLimitSource, memoryLimitKnown = system, "system", true
		case haveCgroup:
			memoryLimitBytes, memoryLimitSource, memoryLimitKnown = cgroup, "cgroup", true
		}
	})
	return memoryLimitBytes, memoryLimitSource, memoryLimitKnown
}

// SetExit replaces how exitProcess ends the process — a test observes the
// status instead of losing its own process to it.
func (s *Server) SetExit(fn func(code int)) {
	s.exit = fn
}
