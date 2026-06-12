package fa

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// fakeTracer records spans and propagates parentage through a context value,
// mimicking how an OpenTelemetry tracer + W3C propagator would behave.
type fakeTracer struct {
	mu    sync.Mutex
	spans []fakeSpan
}

type fakeSpan struct {
	name   string
	attrs  map[string]string
	parent string // name of the parent span ("" = root)
	err    error
}

type fakeTraceKey struct{}

func (f *fakeTracer) StartSpan(ctx context.Context, name string, attrs map[string]string) (context.Context, func(error)) {
	parent, _ := ctx.Value(fakeTraceKey{}).(string)
	f.mu.Lock()
	f.spans = append(f.spans, fakeSpan{name: name, attrs: attrs, parent: parent})
	i := len(f.spans) - 1
	f.mu.Unlock()
	return context.WithValue(ctx, fakeTraceKey{}, name), func(err error) {
		f.mu.Lock()
		f.spans[i].err = err
		f.mu.Unlock()
	}
}

func (f *fakeTracer) Inject(ctx context.Context) string {
	s, _ := ctx.Value(fakeTraceKey{}).(string)
	return s
}

func (f *fakeTracer) Extract(ctx context.Context, carrier string) context.Context {
	return context.WithValue(ctx, fakeTraceKey{}, carrier)
}

func (f *fakeTracer) find(name string) (fakeSpan, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.spans {
		if s.name == name {
			return s, true
		}
	}
	return fakeSpan{}, false
}

// TestTracerSpansAcrossBroker drives one client action end to end and asserts
// the dispatch→emit→deliver span chain, including the carrier hop through the
// broker message (the cross-instance propagation path).
func TestTracerSpansAcrossBroker(t *testing.T) {
	tr := &fakeTracer{}
	app := New([]byte(`{}`), WithTracer(tr))
	app.On("ping", func(c Ctx) ([]Event, error) {
		return []Event{{Op: "replace", FacetID: "F:1", Fragment: "<b>pong</b>"}}, nil
	})
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")

	if code := postEvent(t, mux, `{"type":"ping","payload":{},"conn":"`+conn.id+`"}`); code != 204 {
		t.Fatalf("POST /events = %d", code)
	}

	dispatch, ok := tr.find("fa.dispatch")
	if !ok || dispatch.parent != "" || dispatch.attrs["fa.event"] != "ping" {
		t.Fatalf("fa.dispatch span wrong: ok=%v %+v", ok, dispatch)
	}
	emit, ok := tr.find("fa.emit")
	if !ok || emit.parent != "fa.dispatch" || emit.attrs["fa.scope"] != "conn" {
		t.Fatalf("fa.emit span wrong: ok=%v %+v", ok, emit)
	}
	deliver, ok := tr.find("fa.deliver")
	if !ok || deliver.attrs["fa.facet_id"] != "F:1" {
		t.Fatalf("fa.deliver span wrong: ok=%v %+v", ok, deliver)
	}
	// The deliver span's parent arrived via Inject→broker→Extract, not via a
	// shared in-process context: it must equal the emitting span.
	if deliver.parent != "fa.emit" {
		t.Fatalf("fa.deliver parent = %q, want fa.emit (carried through the broker message)", deliver.parent)
	}

	// And the event itself still arrives, signed.
	e, ok := gotEvent(t, conn)
	if !ok || e.FacetID != "F:1" || e.HMAC == "" {
		t.Fatalf("event not delivered: ok=%v %+v", ok, e)
	}
}

// TestTracerGuardDenialEndsSpanWithError asserts a guard denial closes the
// dispatch span with ErrForbidden.
func TestTracerGuardDenialEndsSpanWithError(t *testing.T) {
	tr := &fakeTracer{}
	app := New([]byte(`{}`), WithTracer(tr))
	app.On("admin", func(c Ctx) ([]Event, error) { return nil, nil })
	app.Guard("admin", func(c Ctx) bool { return false })
	mux := http.NewServeMux()
	app.Mount(mux)
	conn := testConn(app.hub, "")

	if code := postEvent(t, mux, `{"type":"admin","payload":{},"conn":"`+conn.id+`"}`); code != 403 {
		t.Fatalf("POST /events = %d, want 403", code)
	}
	dispatch, ok := tr.find("fa.dispatch")
	if !ok || dispatch.err == nil {
		t.Fatalf("fa.dispatch should end with the guard error: ok=%v %+v", ok, dispatch)
	}
}
