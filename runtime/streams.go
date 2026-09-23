package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"facet/internal/ir"
)

// Named event streams (ir.Stream): a server-sent-events route per declared
// stream, carrying the typed events an action `emit`s. A subscriber connects
// under its session (cookie or bearer token), passes the stream's policy if it
// has one, and then receives every event the stream carries — `id: <n>`,
// `event: <name>` and the DTO as JSON — either broadcast or, for `emit … to
// <actor>`, only when signed in as that actor. Events are handed to the
// stream after the emitting action has committed (runActionLocked collects
// them; fanoutEvents delivers), so a rolled-back action emits nothing.
//
// Every connection opens with an unnumbered `hello` frame (HelloEventDTO:
// the connection's id, the contract and schema versions this server
// publishes, its clock). Each stream keeps its most recent numbered frames,
// so a client that reconnects with `Last-Event-ID` (or `?last_event_id=`)
// is first replayed every frame after that id it was entitled to see.

type streamSub struct {
	ch    chan []byte
	actor string
	key   string // a parameterized stream's instance (its path values), "" otherwise
}

// streamKey identifies one instance of a parameterized stream by its path
// values, as text (a path segment and an `emit … on id` agree on it).
func streamKey(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = toStr(v)
	}
	return strings.Join(parts, "\x00")
}

func evalArgs(xs []*ir.Expr, scope map[string]any) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = eval(x, scope)
	}
	return out
}

type emitted struct {
	typ      string
	name     string // `emit name Dto{…}`: that event only; "" = every event carrying typ
	payload  any
	to       string
	targeted bool
	key      string // `emit … on …`: the parameterized stream instance
}

// streamLogCap bounds the frames a stream remembers for replay; a client
// further behind than that resynchronises from the API instead.
const streamLogCap = 512

type loggedFrame struct {
	seq      int64
	frame    []byte
	to       string
	targeted bool
	key      string
}

// streamLog is one stream's numbered frames, oldest first.
type streamLog struct {
	seq    int64
	frames []loggedFrame
}

// visibleTo reports whether a frame may be delivered to a subscriber.
func (f loggedFrame) visibleTo(sub *streamSub) bool {
	return (!f.targeted || f.to == sub.actor) && f.key == sub.key
}

// helloEvent is the payload of a stream's connect frame (HelloEventDTO).
func (s *Server) helloEvent() map[string]any {
	doc := s.contractDocument()
	return map[string]any{
		"session_id":       newConnectionID(),
		"contract_version": doc["info"].(map[string]any)["version"],
		"schema_version":   fmt.Sprint(doc["x-schema-version"]),
		"server_time":      time.Now().UTC().Format(time.RFC3339),
	}
}

func newConnectionID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// streamHandler answers one declared stream's route.
func (s *Server) streamHandler(st ir.Stream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		if lim := s.rateClass(st.Rate); lim != nil && !lim.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			apiError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		sid := s.sidForRequest(r)
		s.mu.Lock()
		scope := s.scope(sid)
		s.mu.Unlock()
		if st.Auth == "session" {
			if sid == "" {
				w.Header().Set("WWW-Authenticate", "Bearer")
				apiError(w, http.StatusUnauthorized, "sign in to subscribe")
				return
			}
			if !s.policyPasses(ir.Require{Name: st.Requires}, scope) {
				apiError(w, http.StatusForbidden, "forbidden: "+st.Requires)
				return
			}
		}
		// The resume point: the header an EventSource sends by itself wins over
		// the query a client reconnecting by hand passes.
		lastID := int64(-1)
		raw := r.Header.Get("Last-Event-ID")
		if raw == "" {
			raw = r.URL.Query().Get("last_event_id")
		}
		if raw != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n >= 0 {
				lastID = n
			}
		}
		// A parameterized stream: this connection's instance, and the path
		// values the connect/disconnect hooks take as their arguments.
		pathVals := map[string]string{}
		var keyVals []any
		for _, p := range st.Params {
			pathVals[p] = r.PathValue(p)
			keyVals = append(keyVals, r.PathValue(p))
		}
		var connectFrames [][]byte
		if sid == "" && (len(st.Connects) > 0 || st.Disconnect != "") {
			sid = s.session(w, r) // a hook runs as someone: an open stream's guest gets a session
		}
		for _, h := range st.Connects {
			value, status, msg := s.runStreamHook(sid, h.Action, pathVals)
			if status != http.StatusOK {
				apiError(w, status, msg)
				return
			}
			if h.Event != "" {
				// To this connection only, unnumbered like the hello: it is
				// this connection's snapshot, not a replayable event.
				data, _ := json.Marshal(value)
				connectFrames = append(connectFrames, []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", h.Event, data)))
			}
		}
		if st.Disconnect != "" {
			defer s.runStreamHook(sid, st.Disconnect, pathVals)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		sub := &streamSub{ch: make(chan []byte, 32), actor: toStr(scope["actor"]), key: streamKey(keyVals)}
		// Register and snapshot the replay under one lock, so a frame fanned
		// out meanwhile is either in the replay or on the channel, never both
		// and never neither.
		var replay [][]byte
		s.streamMu.Lock()
		if s.streamSubs == nil {
			s.streamSubs = map[string]map[*streamSub]bool{}
		}
		if s.streamSubs[st.Path] == nil {
			s.streamSubs[st.Path] = map[*streamSub]bool{}
		}
		s.streamSubs[st.Path][sub] = true
		if lg := s.streamLogs[st.Path]; lg != nil && lastID >= 0 {
			for _, f := range lg.frames {
				if f.seq > lastID && f.visibleTo(sub) {
					replay = append(replay, f.frame)
				}
			}
		}
		s.streamMu.Unlock()
		defer func() {
			s.streamMu.Lock()
			delete(s.streamSubs[st.Path], sub)
			s.streamMu.Unlock()
		}()
		// The hello, unnumbered, so the client knows the stream is open (and
		// what contract it speaks) before any event.
		hello, _ := json.Marshal(s.helloEvent())
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello)
		for _, f := range connectFrames {
			w.Write(f)
		}
		for _, f := range replay {
			w.Write(f)
		}
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-sub.ch:
				w.Write(msg)
				flusher.Flush()
			}
		}
	}
}

// fanoutEvents delivers committed emits to every stream carrying them: an
// unnamed emit goes out under the name each stream carries its type as, a
// named one only as that event. Each delivered frame is numbered per stream
// and remembered for replay.
func (s *Server) fanoutEvents(events []emitted) {
	if len(events) == 0 {
		return
	}
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	for _, ev := range events {
		data, err := json.Marshal(ev.payload)
		if err != nil {
			continue
		}
		for _, st := range s.ir.Streams {
			for _, def := range st.Events {
				if def.Type != ev.typ || (ev.name != "" && def.Name != ev.name) {
					continue
				}
				if s.streamLogs == nil {
					s.streamLogs = map[string]*streamLog{}
				}
				lg := s.streamLogs[st.Path]
				if lg == nil {
					lg = &streamLog{}
					s.streamLogs[st.Path] = lg
				}
				lg.seq++
				f := loggedFrame{seq: lg.seq, to: ev.to, targeted: ev.targeted, key: ev.key,
					frame: []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", lg.seq, def.Name, data))}
				lg.frames = append(lg.frames, f)
				if len(lg.frames) > streamLogCap {
					lg.frames = lg.frames[len(lg.frames)-streamLogCap:]
				}
				for sub := range s.streamSubs[st.Path] {
					if !f.visibleTo(sub) {
						continue
					}
					select {
					case sub.ch <- f.frame:
					default: // a subscriber that cannot keep up drops this event rather than the connection; it can resume by id
					}
				}
			}
		}
	}
}

// runStreamHook runs a stream's connect/disconnect action as the subscriber,
// its parameters bound from the path's values by name.
func (s *Server) runStreamHook(sid, action string, pathVals map[string]string) (any, int, string) {
	act := s.byAction[action]
	if act == nil {
		return nil, http.StatusInternalServerError, "no such action " + action
	}
	args := make([]any, len(act.Params))
	for i, p := range act.Params {
		v, ok := paramArg(pathVals[p.Name], p)
		if !ok {
			return nil, http.StatusNotFound, fmt.Sprintf("parameter %q expects %s", p.Name, paramTypeName(p))
		}
		args[i] = v
	}
	value, _, status, msg, _ := s.runActionReply(sid, act, args)
	return value, status, msg
}
