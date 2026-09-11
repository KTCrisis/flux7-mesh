package trace

import (
	"encoding/json"
	"testing"
	"time"
)

// The span ID must be derivable from the trace ID, otherwise nothing downstream
// can point at a span as a parent.
func TestSpanIDIsDerivedFromTraceID(t *testing.T) {
	traceID := "0123456789abcdef0123456789abcdef"
	if got := spanIDFor(traceID); got != "0123456789abcdef" {
		t.Errorf("expected the first 16 hex chars, got %q", got)
	}
	if spanIDFor(traceID) != spanIDFor(traceID) {
		t.Error("span ID must be stable for a given trace ID")
	}
}

func TestSpanIDFallsBackOnMalformedTraceID(t *testing.T) {
	for _, bad := range []string{"", "short", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		got := spanIDFor(bad)
		if len(got) != 16 {
			t.Errorf("spanIDFor(%q) = %q, want 16 hex chars", bad, got)
		}
	}
}

// A call authorized by a grant that recorded its origin becomes a child span of
// the call that motivated the grant. This is the edge any OTLP viewer renders.
func TestOTLPCarriesParentSpanAndGrant(t *testing.T) {
	parentTrace := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e := &OTELExporter{service: "flux7-mesh"}
	out := e.toOTLP(Entry{
		TraceID:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		AgentID:       "claude",
		Tool:          "fs.write",
		Policy:        "allow",
		PolicyRule:    "grant:g1",
		GrantID:       "g1",
		ParentTraceID: parentTrace,
		Timestamp:     time.Now(),
	})

	span := out.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.ParentSpanID != spanIDFor(parentTrace) {
		t.Errorf("expected parentSpanId %q, got %q", spanIDFor(parentTrace), span.ParentSpanID)
	}

	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		if kv.Value.StringValue != nil {
			attrs[kv.Key] = *kv.Value.StringValue
		}
	}
	if attrs["grant.id"] != "g1" {
		t.Errorf("expected grant.id attribute, got %q", attrs["grant.id"])
	}
	if attrs["mesh.parent_trace_id"] != parentTrace {
		t.Errorf("expected mesh.parent_trace_id attribute, got %q", attrs["mesh.parent_trace_id"])
	}
}

// A call with no lineage must not emit parentSpanId at all: an empty string
// there is a malformed span for OTLP consumers.
func TestOTLPOmitsParentSpanWhenRootless(t *testing.T) {
	e := &OTELExporter{service: "flux7-mesh"}
	out := e.toOTLP(Entry{
		TraceID:   "cccccccccccccccccccccccccccccccc",
		AgentID:   "claude",
		Tool:      "fs.read",
		Policy:    "allow",
		Timestamp: time.Now(),
	})

	raw, err := json.Marshal(out.ResourceSpans[0].ScopeSpans[0].Spans[0])
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	json.Unmarshal(raw, &decoded)
	if _, present := decoded["parentSpanId"]; present {
		t.Errorf("parentSpanId must be absent on a root span, got %s", raw)
	}
}

func TestOTLPCarriesEndUserWhenPresent(t *testing.T) {
	e := NewOTELExporter("stdout")

	out := e.toOTLP(Entry{TraceID: NewID(), AgentID: "newsletter-agent", UserID: "marc", Tool: "read_file"})
	attrs := map[string]string{}
	for _, kv := range out.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		if kv.Value.StringValue != nil {
			attrs[kv.Key] = *kv.Value.StringValue
		}
	}
	// enduser.id is the semantic-convention key; agent.id stays ours.
	if attrs["enduser.id"] != "marc" {
		t.Errorf("expected enduser.id=marc, got %q", attrs["enduser.id"])
	}
	if attrs["agent.id"] != "newsletter-agent" {
		t.Errorf("agent.id must be untouched, got %q", attrs["agent.id"])
	}

	out = e.toOTLP(Entry{TraceID: NewID(), AgentID: "batch-agent", Tool: "read_file"})
	for _, kv := range out.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		if kv.Key == "enduser.id" {
			t.Error("enduser.id must be absent when the entry carries no user")
		}
	}
}
