package trace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// attrMap flattens the single span of an export into key → string value, so a
// test reads like the dashboard would.
func attrMap(t *testing.T, exp otlpExport) map[string]string {
	t.Helper()
	if len(exp.ResourceSpans) != 1 || len(exp.ResourceSpans[0].ScopeSpans) != 1 || len(exp.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("expected exactly one span, got %+v", exp)
	}
	out := map[string]string{}
	for _, kv := range exp.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		switch {
		case kv.Value.StringValue != nil:
			out[kv.Key] = *kv.Value.StringValue
		case kv.Value.IntValue != nil:
			out[kv.Key] = *kv.Value.IntValue
		}
	}
	return out
}

func TestOTLPSpanCarriesGenAIConventions(t *testing.T) {
	exp := NewOTELExporter("stdout")
	entry := Entry{
		TraceID:               "0123456789abcdef0123456789abcdef",
		Timestamp:             time.Now(),
		AgentID:               "claude",
		Tool:                  "filesystem.read_text_file",
		Policy:                "allow",
		PolicyRule:            "claude",
		StatusCode:            200,
		EstimatedInputTokens:  12,
		EstimatedOutputTokens: 34,
	}
	attrs := attrMap(t, exp.toOTLP(entry))

	// Our own keys stay: nothing downstream that already reads them breaks.
	if attrs["agent.id"] != "claude" || attrs["tool.name"] != "filesystem.read_text_file" {
		t.Fatalf("legacy keys missing: %v", attrs)
	}
	// GenAI semantic conventions sit next to them.
	want := map[string]string{
		"gen_ai.operation.name":      "execute_tool",
		"gen_ai.tool.name":           "filesystem.read_text_file",
		"gen_ai.agent.id":            "claude",
		"gen_ai.usage.input_tokens":  "12",
		"gen_ai.usage.output_tokens": "34",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("%s = %q, want %q", k, attrs[k], v)
		}
	}
}

func TestOTLPSpanOmitsTokenUsageWhenUnknown(t *testing.T) {
	exp := NewOTELExporter("stdout")
	attrs := attrMap(t, exp.toOTLP(Entry{TraceID: "0123456789abcdef0123456789abcdef", Timestamp: time.Now(), Tool: "x"}))
	if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
		t.Fatalf("token usage must not be emitted when unknown: %v", attrs)
	}
}

func TestOTLPExportSendsConfiguredHeaders(t *testing.T) {
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path = %s, want /v1/traces", r.URL.Path)
		}
		var body otlpExport
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("body is not an OTLP export: %v", err)
		}
		got <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := NewOTELExporterWithOptions(srv.URL, OTELOptions{Headers: map[string]string{
		"Authorization": "Bearer s3cr3t",
		"X-Scope-OrgID": "tenant-42",
	}})
	exp.Export(Entry{TraceID: "0123456789abcdef0123456789abcdef", Timestamp: time.Now(), Tool: "x"})

	select {
	case h := <-got:
		if h.Get("Authorization") != "Bearer s3cr3t" {
			t.Errorf("Authorization = %q", h.Get("Authorization"))
		}
		if h.Get("X-Scope-OrgID") != "tenant-42" {
			t.Errorf("X-Scope-OrgID = %q", h.Get("X-Scope-OrgID"))
		}
		if h.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", h.Get("Content-Type"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("collector never received the span")
	}
}

func TestOTLPTransportHonoursInsecureSkipVerify(t *testing.T) {
	// A TLS test server with a self-signed certificate: the default transport
	// must refuse it, the opted-in transport must accept it.
	received := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	entry := Entry{TraceID: "0123456789abcdef0123456789abcdef", Timestamp: time.Now(), Tool: "x"}

	NewOTELExporter(srv.URL).Export(entry)
	select {
	case <-received:
		t.Fatal("default transport accepted a self-signed certificate")
	case <-time.After(300 * time.Millisecond):
	}

	NewOTELExporterWithOptions(srv.URL, OTELOptions{InsecureSkipVerify: true}).Export(entry)
	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("opted-in transport did not reach the collector")
	}
}
