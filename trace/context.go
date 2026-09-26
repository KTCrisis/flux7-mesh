package trace

import "strings"

// Context is the W3C trace context of one call through the mesh: the trace it
// belongs to, the span the mesh opens for it, and the caller's span when the
// caller sent a traceparent.
type Context struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
}

// NewContext builds the context of an incoming call from its Traceparent and
// X-Trace-Id headers, in that order of preference.
//
// With a valid traceparent the mesh joins the caller's trace: the caller's span
// becomes the parent, and the mesh span gets a fresh random ID, because several
// calls may share the trace (deriving the span from the trace ID would give
// them all the same one). Without one, the trace is the mesh's own and the span
// is derived from it (spanIDFor), which keeps it addressable from the trace ID
// alone, as grant lineage needs.
func NewContext(traceparent, xTraceID string) Context {
	if t, p, ok := ParseTraceparent(traceparent); ok {
		return Context{TraceID: t, SpanID: randomSpanID(), ParentSpanID: p}
	}
	if len(xTraceID) == 32 && isHex(xTraceID) && !allZero(xTraceID) {
		id := strings.ToLower(xTraceID)
		return Context{TraceID: id, SpanID: spanIDFor(id)}
	}
	id := NewID()
	return Context{TraceID: id, SpanID: spanIDFor(id)}
}

// ParseTraceparent validates a W3C traceparent ("00-<32 hex>-<16 hex>-<2 hex>")
// and returns its trace ID and parent span ID. All-zero IDs are invalid per spec.
func ParseTraceparent(h string) (traceID, parentSpanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) < 4 {
		return "", "", false
	}
	v, t, p, f := parts[0], parts[1], parts[2], parts[3]
	if len(v) != 2 || !isHex(v) || v == "ff" || len(f) != 2 || !isHex(f) {
		return "", "", false
	}
	if len(t) != 32 || !isHex(t) || allZero(t) || len(p) != 16 || !isHex(p) || allZero(p) {
		return "", "", false
	}
	return strings.ToLower(t), strings.ToLower(p), true
}

// Traceparent is the header the mesh sends to a backend: the backend's parent
// is the mesh span, never an all-zero ID.
func (c Context) Traceparent() string {
	span := c.SpanID
	if len(span) != 16 || !isHex(span) {
		span = spanIDFor(c.TraceID)
	}
	return "00-" + c.TraceID + "-" + span + "-01"
}

func allZero(s string) bool {
	return strings.Trim(s, "0") == ""
}
