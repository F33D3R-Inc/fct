package runtime

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"facet/internal/compile"
	"facet/internal/ir"
)

// The dev server is `facet dev`: it runs the app and watches its source file, so
// a save recompiles, hot-swaps the running graph, and live-reloads the browser —
// no restart, no manual refresh. If FACET_DATABASE_URL is set it uses Postgres;
// otherwise it spins up the in-memory backend so a new project runs with zero
// setup. A compile error is shown as an overlay in the browser and logged, and
// the last good build keeps serving until the source is valid again.

// devHub fans "reload"/"error" notices out to every browser holding the dev
// live-reload stream.
type devHub struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

func newDevHub() *devHub { return &devHub{subs: map[chan string]bool{}} }

func (h *devHub) publish(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *devHub) add() chan string {
	ch := make(chan string, 4)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *devHub) remove(ch chan string) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// EnableDev turns on the live-reload hub so the page serves the dev client and
// /dev/reload streams reload notices.
func (s *Server) EnableDev() { s.dev = newDevHub() }

// handleDevReload is the dev live-reload SSE stream: it pushes a "reload" event
// after a successful rebuild, or an "error" event (with the compile message) when
// a rebuild fails, so the browser can refresh or show an overlay.
func (s *Server) handleDevReload(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := s.dev.add()
	defer s.dev.remove(ch)
	fmt.Fprint(w, "data: ready\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// Reload swaps in a freshly compiled graph without restarting: it rebuilds the
// lookup maps and reverse-relation graph, reconciles the store schema (additive),
// and reloads the working set. Errors leave the previous graph serving.
func (s *Server) Reload(graph *ir.IR) error {
	// rebuild the storeless half against the new graph.
	ns := newServer(graph)

	s.mu.Lock()
	defer s.mu.Unlock()

	loaded, err := s.store.Init(graph.Entities)
	if err != nil {
		return err
	}
	s.ir = graph
	s.byAction = ns.byAction
	s.byBind = ns.byBind
	s.byPolicy = ns.byPolicy
	s.byComponent = ns.byComponent
	s.children = ns.children

	for _, e := range graph.Entities {
		rows := loaded[e.Name]
		if rows == nil {
			rows = []any{}
		}
		s.entities[e.Name] = rows
		max := 0
		for _, r := range rows {
			if m, ok := r.(record); ok {
				if id := toInt(m["id"]); id > max {
					max = id
				}
			}
		}
		s.nextID[e.Name] = max
	}
	return nil
}

// RunDev compiles file, serves it, and watches the file for changes. On a save it
// recompiles and hot-swaps the graph (live-reloading browsers) or, on a compile
// error, surfaces the message without taking the server down.
func RunDev(file, addr string) error {
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	graph, err := compile.String(string(src))
	if err != nil {
		return fmt.Errorf("compile error: %w", err)
	}

	var srv *Server
	if os.Getenv("FACET_DATABASE_URL") != "" {
		srv, err = New(graph)
	} else {
		fmt.Fprintln(os.Stderr, "facet dev: FACET_DATABASE_URL not set — using the in-memory store (data is volatile)")
		srv, err = NewInMemory(graph)
	}
	if err != nil {
		return err
	}
	srv.EnableDev()
	srv.StartJobs()

	go srv.watch(file)

	fmt.Printf("facet dev: %s on http://localhost%s — watching %s (edit & save to hot-reload)\n", graph.App, addr, file)
	return srv.Serve(addr)
}

// watch polls the source file's modification time and recompiles on change.
// Polling needs no third-party file-watching dependency and is robust across
// editors that write-then-rename.
func (s *Server) watch(file string) {
	last := modTime(file)
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		mt := modTime(file)
		if mt.Equal(last) {
			continue
		}
		last = mt
		src, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		graph, err := compile.String(string(src))
		if err != nil {
			fmt.Fprintf(os.Stderr, "facet dev: compile error: %v\n", err)
			s.dev.publish("error: " + err.Error())
			continue
		}
		if err := s.Reload(graph); err != nil {
			fmt.Fprintf(os.Stderr, "facet dev: reload failed: %v\n", err)
			s.dev.publish("error: " + err.Error())
			continue
		}
		fmt.Fprintln(os.Stderr, "facet dev: reloaded")
		s.dev.publish("reload")
	}
}

func modTime(file string) time.Time {
	if fi, err := os.Stat(file); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// devScript is injected into every page in dev mode. It subscribes to the
// reload stream and refreshes the page on a rebuild, or shows a compile-error
// overlay until the next good build.
const devScript = `<script>
(function(){
  if(!window.EventSource) return;
  var es = new EventSource("/dev/reload");
  es.onmessage = function(e){
    var d = e.data || "";
    if(d === "reload"){ location.reload(); return; }
    if(d.indexOf("error:") === 0){ overlay(d.slice(6).trim()); }
  };
  function overlay(msg){
    var id="fa-dev-overlay", el=document.getElementById(id);
    if(!el){ el=document.createElement("div"); el.id=id;
      el.style.cssText="position:fixed;inset:0;background:rgba(20,20,20,.92);color:#f8f8f8;font:14px/1.5 ui-monospace,monospace;padding:2rem;z-index:99999;white-space:pre-wrap;overflow:auto";
      document.body.appendChild(el); }
    el.textContent = "Facet compile error\n\n" + msg + "\n\n(fix the file and save — this clears automatically)";
  }
})();
</script>
`
