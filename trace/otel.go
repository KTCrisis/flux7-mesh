package trace

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OTLP/HTTP delivery: spans wait in a bounded queue and leave in batches.
// A full queue drops the newest span rather than block a tool call, and a
// collector that is down costs a few retries, never the proxy.
const (
	otelQueueSize   = 2048
	otelBatchSize   = 128
	otelFlushEvery  = 2 * time.Second
	otelMaxAttempts = 4 // 1 try + 3 retries
	otelBackoffBase = 500 * time.Millisecond
	otelCloseWait   = 5 * time.Second
)

// OTELExporter converts trace entries to OTLP JSON spans and exports them.
type OTELExporter struct {
	endpoint string // "stdout", "http://...", or file path ending in ".jsonl"
	client   *http.Client
	service  string
	headers  map[string]string // sent on every OTLP/HTTP request (auth, tenant…)

	// JSONL file output
	file   *os.File
	fileMu sync.Mutex

	// OTLP/HTTP queue (nil unless the endpoint is http(s))
	queue     chan otlpSpan
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
	backoff   time.Duration

	sent    atomic.Int64
	dropped atomic.Int64
	failed  atomic.Int64
}

// OTELOptions tunes the HTTP transport of an exporter. The zero value is the
// historical behaviour: no extra headers, system trust store, verification on.
type OTELOptions struct {
	Headers            map[string]string
	CACert             string // PEM file appended to the system roots
	InsecureSkipVerify bool
}

// NewOTELExporter creates an exporter with default transport options.
// endpoint: "stdout", "http://..." (OTLP HTTP), or a file path (e.g. "/path/traces-otel.jsonl").
func NewOTELExporter(endpoint string) *OTELExporter {
	return NewOTELExporterWithOptions(endpoint, OTELOptions{})
}

// NewOTELExporterWithOptions creates an exporter whose HTTP requests carry the
// given headers and trust the given CA. A bad CA file is logged and ignored
// rather than fatal: losing traces must never take the proxy down.
func NewOTELExporterWithOptions(endpoint string, opts OTELOptions) *OTELExporter {
	exp := &OTELExporter{
		endpoint: endpoint,
		client:   &http.Client{Timeout: 5 * time.Second, Transport: otelTransport(opts)},
		service:  "flux7-mesh",
		headers:  opts.Headers,
	}

	if isHTTP(endpoint) {
		exp.queue = make(chan otlpSpan, otelQueueSize)
		exp.done = make(chan struct{})
		exp.stopped = make(chan struct{})
		exp.backoff = otelBackoffBase
		go exp.run()
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

// otelTransport builds the HTTP transport from the options. It starts from a
// clone of the default transport so proxies, timeouts and keep-alives stay as
// Go ships them; only the TLS settings change.
func otelTransport(opts OTELOptions) http.RoundTripper {
	if opts.CACert == "" && !opts.InsecureSkipVerify {
		return http.DefaultTransport
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec // operator opt-in for a self-signed local collector
	if opts.CACert != "" {
		pem, err := os.ReadFile(opts.CACert)
		if err != nil {
			slog.Error("otel: cannot read ca cert", "path", opts.CACert, "error", err)
		} else {
			pool, err := x509.SystemCertPool()
			if err != nil || pool == nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(pem) {
				slog.Error("otel: no certificate found in ca cert", "path", opts.CACert)
			}
			tlsCfg.RootCAs = pool
		}
	}
	tr.TLSClientConfig = tlsCfg
	return tr
}

// Export sends a trace entry as an OTLP span: written at once to stdout or a
// file, queued for a batched POST to an OTLP/HTTP collector. Never blocks.
func (e *OTELExporter) Export(entry Entry) {
	export := e.toOTLP(entry)

	if e.queue != nil {
		span := export.ResourceSpans[0].ScopeSpans[0].Spans[0]
		select {
		case e.queue <- span:
		default:
			if n := e.dropped.Add(1); n == 1 || n%100 == 0 {
				slog.Warn("otel: queue full, span dropped", "dropped_total", n)
			}
		}
		return
	}

	data, err := json.Marshal(export)
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
	}
}

// run batches queued spans: a batch leaves when full or every otelFlushEvery.
// On Close it drains the queue and sends what is left.
func (e *OTELExporter) run() {
	defer close(e.stopped)
	tick := time.NewTicker(otelFlushEvery)
	defer tick.Stop()
	batch := make([]otlpSpan, 0, otelBatchSize)
	flush := func() {
		if len(batch) > 0 {
			e.send(batch)
			batch = make([]otlpSpan, 0, otelBatchSize)
		}
	}
	for {
		select {
		case sp := <-e.queue:
			batch = append(batch, sp)
			if len(batch) >= otelBatchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-e.done:
			for {
				select {
				case sp := <-e.queue:
					batch = append(batch, sp)
					if len(batch) >= otelBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// send POSTs one batch, retrying network errors, 429 and 5xx with
// exponential backoff. Other 4xx mean the collector refuses the payload:
// retrying would not change its mind. During Close there is no backoff.
func (e *OTELExporter) send(spans []otlpSpan) {
	data, err := json.Marshal(e.wrap(spans))
	if err != nil {
		slog.Error("otel: marshal failed", "error", err)
		return
	}
	url := e.endpoint + "/v1/traces"
	var last string
	for attempt := 0; attempt < otelMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-e.done:
				// shutting down: one immediate try each, no waiting
			case <-time.After(e.backoff << (attempt - 1)):
			}
		}
		req, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err != nil {
			slog.Error("otel: request failed", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range e.headers {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode < 300:
			e.sent.Add(int64(len(spans)))
			return
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = resp.Status
			continue
		default:
			e.failed.Add(int64(len(spans)))
			slog.Warn("otel: export rejected", "endpoint", url, "status", resp.StatusCode, "spans", len(spans))
			return
		}
	}
	e.failed.Add(int64(len(spans)))
	slog.Warn("otel: export failed after retries", "endpoint", url, "error", last, "spans", len(spans))
}

// OTELStats counts spans by outcome, for the metrics endpoint and tests.
type OTELStats struct {
	Sent    int64 `json:"sent"`
	Dropped int64 `json:"dropped"` // queue full
	Failed  int64 `json:"failed"`  // rejected or out of retries
	Queued  int   `json:"queued"`
}

// Stats returns delivery counters. Zero for stdout and file exporters.
func (e *OTELExporter) Stats() OTELStats {
	return OTELStats{Sent: e.sent.Load(), Dropped: e.dropped.Load(), Failed: e.failed.Load(), Queued: len(e.queue)}
}

// wrap puts spans under one resource and scope.
func (e *OTELExporter) wrap(spans []otlpSpan) otlpExport {
	return otlpExport{
		ResourceSpans: []otlpResourceSpan{{
			Resource: otlpResource{
				Attributes: []otlpKV{
					{Key: "service.name", Value: strVal(e.service)},
				},
			},
			ScopeSpans: []otlpScopeSpan{{
				Scope: otlpScope{Name: "flux7-mesh", Version: "0.6.0"},
				Spans: spans,
			}},
		}},
	}
}

// OTLP JSON structures (minimal, spec-compliant subset).

type otlpExport struct {
	ResourceSpans []otlpResourceSpan `json:"resourceSpans"`
}

type otlpResourceSpan struct {
	Resource   otlpResource    `json:"resource"`
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
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"` // 3 = SERVER
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpKV   `json:"attributes"`
	Status            otlpStatus `json:"status"`
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

func strVal(s string) otlpValue { return otlpValue{StringValue: &s} }
func intVal(n int64) otlpValue  { v := fmt.Sprintf("%d", n); return otlpValue{IntValue: &v} }

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
	// Span ID: 16 hex chars, derived from the trace ID rather than random.
	// A random span ID is unreferenceable — nothing downstream can point at it —
	// so a parent link would be impossible to express. Deriving it makes any
	// span addressable from its trace ID alone, which is what parentSpanId below
	// needs. Entropy is unchanged: the trace ID is already 16 random bytes.
	spanID := spanIDFor(traceID)
	if len(entry.SpanID) == 16 && isHex(entry.SpanID) {
		spanID = entry.SpanID
	}

	// A grant that recorded its origin makes the authorizing call the parent of
	// every call it later waves through. This is the one causal edge a proxy can
	// observe without being told: the agent's reasoning stays invisible, but the
	// chain of authority does not.
	//
	// A caller that sent a traceparent names its own span as the parent, and
	// that edge wins: it keeps the caller's tree whole (behind Kong, an SDK,
	// another mesh). The store resolves grant lineage into ParentSpanID too
	// when the originating call is still in memory.
	parentSpanID := ""
	if len(entry.ParentSpanID) == 16 && isHex(entry.ParentSpanID) {
		parentSpanID = entry.ParentSpanID
	} else if entry.ParentTraceID != "" && len(entry.ParentTraceID) == 32 && isHex(entry.ParentTraceID) {
		parentSpanID = spanIDFor(entry.ParentTraceID)
	}

	attrs := []otlpKV{
		{Key: "agent.id", Value: strVal(entry.AgentID)},
		{Key: "tool.name", Value: strVal(entry.Tool)},
		{Key: "policy.action", Value: strVal(entry.Policy)},
		{Key: "policy.rule", Value: strVal(entry.PolicyRule)},
		{Key: "http.status_code", Value: intVal(int64(entry.StatusCode))},
		// OpenTelemetry GenAI semantic conventions, alongside our own keys: a
		// dashboard built for any agent framework (Langfuse, Grafana's GenAI
		// panels) recognises these without a mapping. One mesh call is one
		// tool execution on behalf of an agent.
		{Key: "gen_ai.operation.name", Value: strVal("execute_tool")},
		{Key: "gen_ai.tool.name", Value: strVal(entry.Tool)},
		{Key: "gen_ai.agent.id", Value: strVal(entry.AgentID)},
	}

	if entry.SessionID != "" {
		attrs = append(attrs, otlpKV{Key: "session.id", Value: strVal(entry.SessionID)})
	}
	// enduser.id is the OpenTelemetry semantic-convention name for the
	// authenticated human behind a request; agent.id stays our own key.
	if entry.UserID != "" {
		attrs = append(attrs, otlpKV{Key: "enduser.id", Value: strVal(entry.UserID)})
	}
	if entry.Error != "" {
		attrs = append(attrs, otlpKV{Key: "error.message", Value: strVal(entry.Error)})
	}
	if entry.GrantID != "" {
		attrs = append(attrs, otlpKV{Key: "grant.id", Value: strVal(entry.GrantID)})
	}
	if entry.ParentTraceID != "" {
		attrs = append(attrs, otlpKV{Key: "mesh.parent_trace_id", Value: strVal(entry.ParentTraceID)})
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
		// Same figures under the GenAI convention names.
		attrs = append(attrs, otlpKV{Key: "gen_ai.usage.input_tokens", Value: intVal(int64(entry.EstimatedInputTokens))})
		attrs = append(attrs, otlpKV{Key: "gen_ai.usage.output_tokens", Value: intVal(int64(entry.EstimatedOutputTokens))})
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
					ParentSpanID:      parentSpanID,
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

	return exp.wrap(spans)
}

// Close sends the queued spans (bounded by otelCloseWait) and closes the
// file, if any. Safe to call more than once.
func (e *OTELExporter) Close() error {
	if e.queue != nil {
		e.closeOnce.Do(func() { close(e.done) })
		select {
		case <-e.stopped:
		case <-time.After(otelCloseWait):
			slog.Warn("otel: close timed out, spans left in queue", "queued", len(e.queue))
		}
		st := e.Stats()
		slog.Info("otel: exporter closed", "sent", st.Sent, "dropped", st.Dropped, "failed", st.Failed)
	}
	if e.file != nil {
		return e.file.Close()
	}
	return nil
}

// spanIDFor derives a span ID from a trace ID: the first 8 bytes, per the OTEL
// requirement of 16 hex chars. Deterministic on purpose, so that a span can be
// referenced as a parent from nothing but the trace ID of the call it belongs to.
// Malformed input falls back to a random ID rather than a short or padded one.
func spanIDFor(traceID string) string {
	if len(traceID) < 16 || !isHex(traceID) {
		return randomSpanID()
	}
	return strings.ToLower(traceID[:16])
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
