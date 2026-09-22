package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"facet/internal/ir"
)

// Named event streams (ir.Stream): a server-sent-events route per declared
// stream, carrying the typed events an action `emit`s. A subscriber connects
// under its session (cookie or bearer token), passes the stream's policy if it
// has one, and then receives every event whose type the stream carries —
// `event: <Type>` with the DTO as JSON — either broadcast or, for `emit … to
// <actor>`, only when signed in as that actor. Events are handed to the
// stream after the emitting action has committed (runActionLocked collects
// them; fanoutEvents delivers), so a rolled-back action emits nothing.

type streamSub struct {
	ch    chan []byte
	actor string
}

type emitted struct {
	typ      string
	payload  any
	to       string
	targeted bool
}

// streamHandler answers one declared stream's route.
func (s *Server) streamHandler(st ir.Stream) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		sub := &streamSub{ch: make(chan []byte, 32), actor: toStr(scope["actor"])}
		s.streamMu.Lock()
		if s.streamSubs == nil {
			s.streamSubs = map[string]map[*streamSub]bool{}
		}
		if s.streamSubs[st.Path] == nil {
			s.streamSubs[st.Path] = map[*streamSub]bool{}
		}
		s.streamSubs[st.Path][sub] = true
		s.streamMu.Unlock()
		defer func() {
			s.streamMu.Lock()
			delete(s.streamSubs[st.Path], sub)
			s.streamMu.Unlock()
		}()
		// A hello so the client knows the stream is open before any event.
		fmt.Fprintf(w, "event: hello\ndata: {}\n\n")
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

// fanoutEvents delivers committed emits to every stream carrying their type.
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
		frame := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", ev.typ, data))
		for _, st := range s.ir.Streams {
			carries := false
			for _, t := range st.Events {
				if t == ev.typ {
					carries = true
				}
			}
			if !carries {
				continue
			}
			for sub := range s.streamSubs[st.Path] {
				if ev.targeted && sub.actor != ev.to {
					continue
				}
				select {
				case sub.ch <- frame:
				default: // a subscriber that cannot keep up drops this event rather than the connection
				}
			}
		}
	}
}

var _ sync.Mutex
