package fa

import "context"

// Tracer is the hook FA calls around its dispatch→render→emit pipeline. FA
// itself stays dependency-free: implement this with OpenTelemetry (or anything
// else) and pass it via WithTracer. The spans FA opens:
//
//	fa.dispatch — one client action at /events: guard + handler + emit
//	              (attrs: fa.event = action type, fa.identity.present)
//	fa.emit     — signing + publishing one batch of events to the broker
//	              (attrs: fa.scope, fa.target, fa.events = batch size)
//	fa.deliver  — one broker message applied to this instance's local
//	              connections, including render-for-native
//	              (attrs: fa.scope, fa.op, fa.facet_id)
//
// Cross-instance propagation: at emit time the context is serialized with
// Inject and carried inside the broker message; the delivering instance — this
// one or any other — restores it with Extract, so fa.deliver is a child of the
// originating fa.dispatch and a request is traceable across the broker. With
// OpenTelemetry, Inject/Extract are one call each on a W3C TraceContext
// propagator ("traceparent").
type Tracer interface {
	// StartSpan opens a span as a child of any span in ctx. The returned end
	// function closes it; a non-nil err marks the span failed.
	StartSpan(ctx context.Context, name string, attrs map[string]string) (context.Context, func(err error))
	// Inject serializes the trace context in ctx (e.g. a W3C traceparent).
	// Return "" when there is nothing to propagate.
	Inject(ctx context.Context) string
	// Extract restores a trace context serialized by Inject on another (or this)
	// instance. carrier may be "" — return a usable context regardless.
	Extract(ctx context.Context, carrier string) context.Context
}

// WithTracer installs the app's Tracer (see the Tracer docs for the spans FA
// opens and how they propagate across the broker). Default: no tracing.
func WithTracer(t Tracer) Option { return func(c *appConfig) { c.tracer = t } }

// span opens a span on the hub's tracer; with no tracer installed it returns
// the context unchanged and a no-op end, so call sites stay unconditional.
func (h *Hub) span(ctx context.Context, name string, attrs map[string]string) (context.Context, func(error)) {
	if h.tracer == nil {
		return ctx, func(error) {}
	}
	return h.tracer.StartSpan(ctx, name, attrs)
}
