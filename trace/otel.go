package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// OTELExporter converts trace entries to OTLP JSON spans and exports them.
type OTELExporter struct {
	endpoint string // "stdout", "http://...", or file path ending in ".jsonl"
	client   *http.Client
	service  string

	// JSONL file output
	file   *os.File
	fileMu sync.Mutex
}

// NewOTELExporter creates an exporter.
// endpoint: "stdout", "http://..." (OTLP HTTP), or a file path (e.g. "/path/traces-otel.jsonl").
func NewOTELExporter(endpoint string) *OTELExporter {
	exp := &OTELExporter{
		endpoint: endpoint,
		client:   &http.Client{Timeout: 5 * time.Second},
		service:  "flux7-mesh",
	}

	// If not stdout and not http, treat as file path
	if endpoint != "stdout" && !isHTTP(endpoint) {
		f, err := os.OpenFile(endpoint, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			slog.Error("otel: failed to open file", "path", endpoint, "error", err)
		} else {
			exp.file = f
		}
	}

	return exp
}

func isHTTP(s string) bool {
	return len(s) > 7 && (s[:7] == "http://" || s[:8] == "https://")
}

// Export sends a trace entry as an OTLP span.
func (e *OTELExporter) Export(entry Entry) {
	span := e.toOTLP(entry)
	data, err := json.Marshal(span)
	if err != nil {
		slog.Error("otel: marshal failed", "error", err)
		return
	}

	if e.endpoint == "stdout" {
		fmt.Fprintln(os.Stderr, string(data))
		return
	}

	// JSONL file
	if e.file != nil {
		e.fileMu.Lock()
		e.file.Write(data)
		e.file.Write([]byte("\n"))
		e.fileMu.Unlock()
		return
	}

	// OTLP HTTP
	url := e.endpoint + "/v1/traces"
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		slog.Error("otel: request failed", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		slog.Warn("otel: export failed", "endpoint", url, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("otel: export rejected", "status", resp.StatusCode)
	}
}

// OTLP JSON structures (minimal, spec-compliant subset).

type otlpExport struct {
	ResourceSpans []otlpResourceSpan `json:"resourceSpans"`
}

type otlpResourceSpan struct {
	Resource  otlpResource   `json:"resource"`
	ScopeSpans []otlpScopeSpan `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScopeSpan struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpSpan struct {
	TraceID            string     `json:"traceId"`
	SpanID             string     `json:"spanId"`
	Name               string     `json:"name"`
	Kind               int        `json:"kind"` // 3 = SERVER
	StartTimeUnixNano  string     `json:"startTimeUnixNano"`
	EndTimeUnixNano    string     `json:"endTimeUnixNano"`
	Attributes         []otlpKV   `json:"attributes"`
	Status             otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code    int    `json:"code"` // 0=UNSET, 1=OK, 2=ERROR
	Message string `json:"message,omitempty"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string `json:"stringValue,omitempty"`
	IntValue    *string `json:"intValue,omitempty"`
}

func strVal(s string) otlpValue  { return otlpValue{StringValue: &s} }
func intVal(n int64) otlpValue   { v := fmt.Sprintf("%d", n); return otlpValue{IntValue: &v} }

func (e *OTELExporter) toOTLP(entry Entry) otlpExport {
	endTime := entry.Timestamp
	startTime := endTime.Add(-time.Duration(entry.LatencyMs) * time.Millisecond)

	// Trace ID must be exactly 32 hex chars (16 bytes) per W3C.
	// NewID() already produces this; if the entry ID is malformed, generate a fresh one
	// rather than zero-padding (which destroys entropy and corrupts trace correlation).
	traceID := entry.TraceID
	if len(traceID) != 32 || !isHex(traceID) {
		traceID = randomTraceID()
	}
	// Span ID: 8 random bytes (16 hex chars) per OTEL spec
	spanID := randomSpanID()

	attrs := []otlpKV{
		{Key: "agent.id", Value: strVal(entry.AgentID)},
		{Key: "tool.name", Value: strVal(entry.Tool)},
		{Key: "policy.action", Value: strVal(entry.Policy)},
		{Key: "policy.rule", Value: strVal(entry.PolicyRule)},
		{Key: "http.status_code", Value: intVal(int64(entry.StatusCode))},
	}

	if entry.SessionID != "" {
		attrs = append(attrs, otlpKV{Key: "session.id", Value: strVal(entry.SessionID)})
	}
	if entry.Error != "" {
		attrs = append(attrs, otlpKV{Key: "error.message", Value: strVal(entry.Error)})
	}
	if entry.ApprovalID != "" {
		attrs = append(attrs, otlpKV{Key: "approval.id", Value: strVal(entry.ApprovalID)})
		attrs = append(attrs, otlpKV{Key: "approval.status", Value: strVal(entry.ApprovalStatus)})
		if entry.ApprovalMs > 0 {
			attrs = append(attrs, otlpKV{Key: "approval.duration_ms", Value: intVal(entry.ApprovalMs)})
		}
	}
	if entry.EstimatedInputTokens > 0 {
		attrs = append(attrs, otlpKV{Key: "llm.token.input", Value: intVal(int64(entry.EstimatedInputTokens))})
		attrs = append(attrs, otlpKV{Key: "llm.token.output", Value: intVal(int64(entry.EstimatedOutputTokens))})
	}

	status := otlpStatus{Code: 1} // OK
	if entry.Error != "" || entry.Policy == "deny" {
		status = otlpStatus{Code: 2, Message: entry.Error}
	}

	return otlpExport{
		ResourceSpans: []otlpResourceSpan{{
			Resource: otlpResource{
				Attributes: []otlpKV{
					{Key: "service.name", Value: strVal(e.service)},
				},
			},
			ScopeSpans: []otlpScopeSpan{{
				Scope: otlpScope{Name: "flux7-mesh", Version: "0.6.0"},
				Spans: []otlpSpan{{
					TraceID:           traceID,
					SpanID:            spanID,
					Name:              entry.Tool,
					Kind:              3,
					StartTimeUnixNano: fmt.Sprintf("%d", startTime.UnixNano()),
					EndTimeUnixNano:   fmt.Sprintf("%d", endTime.UnixNano()),
					Attributes:        attrs,
					Status:            status,
				}},
			}},
		}},
	}
}

// EntriesToOTLP converts a batch of trace entries into a single OTLP export.
// Useful for HTTP endpoints that serve trace history in OTLP format on demand,
// without requiring a configured OTEL endpoint.
func EntriesToOTLP(entries []Entry, service string) any {
	if service == "" {
		service = "flux7-mesh"
	}
	exp := &OTELExporter{service: service}

	spans := make([]otlpSpan, 0, len(entries))
	for _, e := range entries {
		otlp := exp.toOTLP(e)
		if len(otlp.ResourceSpans) > 0 && len(otlp.ResourceSpans[0].ScopeSpans) > 0 {
			spans = append(spans, otlp.ResourceSpans[0].ScopeSpans[0].Spans...)
		}
	}

	return otlpExport{
		ResourceSpans: []otlpResourceSpan{{
			Resource: otlpResource{
				Attributes: []otlpKV{
					{Key: "service.name", Value: strVal(service)},
				},
			},
			ScopeSpans: []otlpScopeSpan{{
				Scope: otlpScope{Name: "flux7-mesh", Version: "0.6.0"},
				Spans: spans,
			}},
		}},
	}
}

// Close flushes and closes the OTEL file if any.
func (e *OTELExporter) Close() error {
	if e.file != nil {
		return e.file.Close()
	}
	return nil
}

// randomSpanID returns 8 random bytes as a 16-char hex string.
// Falls back to a timestamp-derived ID if the RNG fails (should never happen).
func randomSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// randomTraceID returns 16 random bytes as a 32-char hex string.
func randomTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
