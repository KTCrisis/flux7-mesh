package trace

import "testing"

func TestParseTraceparent(t *testing.T) {
	const tid = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct {
		in     string
		ok     bool
		parent string
	}{
		{"00-" + tid + "-00f067aa0ba902b7-01", true, "00f067aa0ba902b7"},
		{"00-" + tid + "-00F067AA0BA902B7-00", true, "00f067aa0ba902b7"},
		{"00-" + tid + "-0000000000000000-01", false, ""},                      // all-zero parent
		{"00-00000000000000000000000000000000-00f067aa0ba902b7-01", false, ""}, // all-zero trace
		{"ff-" + tid + "-00f067aa0ba902b7-01", false, ""},                      // forbidden version
		{"00-" + tid + "-00f067aa0ba902-01", false, ""},                        // short parent
		{"00-" + tid, false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		gotT, gotP, ok := ParseTraceparent(c.in)
		if ok != c.ok || (ok && (gotT != tid || gotP != c.parent)) {
			t.Errorf("ParseTraceparent(%q) = %q %q %v", c.in, gotT, gotP, ok)
		}
	}
}

// Two calls in one caller trace: same trace, same parent, distinct mesh spans.
func TestNewContextJoinsCallerTrace(t *testing.T) {
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	a, b := NewContext(tp, ""), NewContext(tp, "")
	if a.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || a.ParentSpanID != "00f067aa0ba902b7" {
		t.Fatalf("context = %+v", a)
	}
	if a.SpanID == b.SpanID || len(a.SpanID) != 16 {
		t.Errorf("spans %q and %q must be distinct 16-hex IDs", a.SpanID, b.SpanID)
	}
}

func TestNewContextOwnTrace(t *testing.T) {
	c := NewContext("garbage", "")
	if len(c.TraceID) != 32 || c.SpanID != spanIDFor(c.TraceID) || c.ParentSpanID != "" {
		t.Errorf("context = %+v", c)
	}
	x := NewContext("", "ABC1230000000000A4AFF75F4F850582")
	if x.TraceID != "abc1230000000000a4aff75f4f850582" || x.ParentSpanID != "" {
		t.Errorf("x-trace-id context = %+v", x)
	}
}

func TestTraceparentNeverZeroParent(t *testing.T) {
	c := Context{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"}
	if got := c.Traceparent(); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-4bf92f3577b34da6-01" {
		t.Errorf("traceparent = %q", got)
	}
}

// Grant lineage resolves to the originating call's real span, even when that
// call joined a caller trace and got a random span.
func TestRecordResolvesLineageSpan(t *testing.T) {
	s := NewStore(10)
	s.Record(Entry{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "1111111111111111", Tool: "a"})
	s.Record(Entry{TraceID: NewID(), ParentTraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Tool: "b"})
	got := s.Query("", "b", 1)
	if len(got) != 1 || got[0].ParentSpanID != "1111111111111111" {
		t.Fatalf("entry = %+v", got)
	}
	span := (&OTELExporter{service: "t"}).toOTLP(got[0]).ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.ParentSpanID != "1111111111111111" {
		t.Errorf("otlp parent = %q", span.ParentSpanID)
	}
}
