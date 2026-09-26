# OpenTelemetry Export

Agent Mesh exports every tool call as an OTLP span. Zero new dependencies — raw OTLP JSON, no SDK required.

## Configuration

Add `otel_endpoint` to your config YAML:

```yaml
# Write OTLP spans to a JSONL file (zero infra)
otel_endpoint: /path/to/traces-otel.jsonl

# Send to an OTLP HTTP backend (Jaeger, Grafana Tempo, OTEL Collector)
otel_endpoint: http://localhost:4318

# Debug: print spans to stderr
otel_endpoint: stdout
```

Omit `otel_endpoint` to disable OTEL export. The internal trace store (`trace_file`) continues to work independently.

## Span attributes

Every span includes the following attributes:

| Attribute | Type | Description |
|-----------|------|-------------|
| `service.name` | resource | Always `mesh7` |
| `agent.id` | string | Agent identity (e.g. `claude`, `crewai-researcher`) |
| `enduser.id` | string | The human the agent acted for, when the token carried one (`auth.jwt.user_claim`). OpenTelemetry semantic-convention key; absent on agent-only calls |
| `session.id` | string | MCP session the call belongs to (when set) |
| `tool.name` | string | Tool that was called (e.g. `filesystem.write_file`) |
| `policy.action` | string | Policy decision: `allow`, `deny`, `human_approval` |
| `policy.rule` | string | Which policy rule matched |
| `http.status_code` | int | Backend response status code |
| `error.message` | string | Error details (when applicable) |
| `approval.id` | string | Approval request ID (when `human_approval`) |
| `approval.status` | string | `approved`, `denied`, or `timeout` |
| `approval.duration_ms` | int | Time spent waiting for human approval |
| `llm.token.input` | int | Estimated input tokens (chars/4 heuristic) |
| `llm.token.output` | int | Estimated output tokens |
| `grant.id` | string | Temporal grant that authorized the call (when one did) |
| `mesh.parent_trace_id` | string | The call that motivated that grant (when it recorded an origin) |

Span kind is `SERVER` (3). Status code is `OK` (1) for allowed calls, `ERROR` (2) for denied or failed calls.

## Trace context

An incoming W3C `traceparent` is honoured: the mesh span joins the caller's
trace, with the caller's span as its `parentSpanId`, so behind Kong, an
instrumented SDK or another mesh the tree stays whole. Its span ID is random,
since several calls may share one caller trace. Without a `traceparent` (or
with `X-Trace-Id` only), the trace is the mesh's own and the span ID is derived
from the trace ID (its first 16 hex chars). A malformed or all-zero
`traceparent` is ignored.

HTTP backends receive `traceparent: 00-<trace>-<mesh span>-01` and `X-Trace-Id`:
the mesh span is their parent.

## Chain of authority

Without a caller trace, span IDs are derived from the trace ID rather than
generated at random. A random span ID is unreferenceable, so no span could ever
name another as its parent; the trace store also records each span, so lineage
resolves to the real span when the authorizing call joined a caller trace.

When a temporal grant authorizes a call, and that grant recorded the call that
motivated it, the span carries `parentSpanId` pointing at the authorizing call.
Jaeger, Tempo and any OTLP viewer then render the real shape:

```
approved call (human_approval)
└── call the grant waved through
    └── the next one
```

This is the one causal edge a proxy can observe rather than be told. The agent's
reasoning stays invisible — mesh7 never claims to know *why the model chose* a
tool — but the chain of authority is mechanical and complete.

A root span with no `parentSpanId` means the call needed no grant, or the grant
covering it was issued without an origin. Both are honest answers, not gaps.

Origin is recorded at grant creation, and is always optional:

```bash
curl -X POST localhost:9090/grants -d '{
  "agent": "claude", "tools": "filesystem.write_*", "duration": "1h",
  "approval_id": "appr-7", "trace_id": "<the call being approved>"
}'
```

`GET /traces/{id}/why?depth=10` walks the same chain over the HTTP API, oldest
first, without an OTLP backend.

## JSONL file mode

The simplest mode — each line is a complete OTLP JSON export:

```yaml
otel_endpoint: /home/user/mesh7/traces-otel.jsonl
```

Query with `jq`:

```bash
# All denied calls
cat traces-otel.jsonl | jq '.resourceSpans[].scopeSpans[].spans[] | select(.status.code == 2)'

# Calls by agent
cat traces-otel.jsonl | jq '.resourceSpans[].scopeSpans[].spans[] | select(.attributes[] | select(.key == "agent.id" and .value.stringValue == "claude"))'

# Latency (endTime - startTime)
cat traces-otel.jsonl | jq '.resourceSpans[].scopeSpans[].spans[] | {name, duration_ns: ((.endTimeUnixNano | tonumber) - (.startTimeUnixNano | tonumber))}'
```

The file is append-only. Use it as a feed for dashboards, analytics, or agent7.

## OTLP HTTP mode

Send spans to any OTLP-compatible backend:

```yaml
# Jaeger (all-in-one)
otel_endpoint: http://localhost:4318

# Grafana Tempo
otel_endpoint: http://localhost:4318

# OTEL Collector
otel_endpoint: http://localhost:4318
```

Spans are POSTed to `{endpoint}/v1/traces` with `Content-Type: application/json`.

Delivery:

- spans wait in a bounded queue (2048) and leave in batches of up to 128, or
  every 2 seconds;
- network errors, `429` and `5xx` are retried three times with exponential
  backoff (0.5 s, 1 s, 2 s); any other `4xx` is final;
- a full queue drops the span and logs it: a tool call never waits on the
  collector;
- on shutdown the queue is drained within 5 seconds, and the exporter logs how
  many spans were sent, dropped and failed.

### Authenticated or TLS collectors

```yaml
otel_endpoint: https://otlp-gateway.example.com/otlp
otel_headers:
  Authorization: "Basic ${GRAFANA_OTLP_TOKEN}"   # ${VAR} expanded from the environment
  X-Scope-OrgID: tenant-42
otel_ca_cert: /etc/mesh7/collector-ca.pem          # appended to the system roots
otel_insecure_skip_verify: false                   # true only for a self-signed local collector
```

Header values are expanded from the environment and never logged (only header
names are). An unreadable CA file is logged, not fatal.

### Jaeger quick start

```bash
docker run -d --name jaeger \
  -p 16686:16686 \
  -p 4318:4318 \
  jaegertracing/jaeger:latest
```

Then set `otel_endpoint: http://localhost:4318` and open `http://localhost:16686` to browse traces.

## How it works

```
Agent calls tool
  → policy evaluated
  → tool forwarded
  → trace.Entry recorded in Store
  → Store.Record() hands the entry to the OTEL exporter
       → Entry converted to OTLP span
       → Written to file / printed to stderr / queued for a batched POST
```

The OTEL exporter is a hook on the existing trace store and never blocks tool calls. If the OTLP endpoint is down, spans are retried, then counted as failed, and the call proceeds normally.

A span is exported when the call is first recorded. For `human_approval`, the
approval outcome is added later as a revision in `trace_file` (see
[trace-integrity.md](trace-integrity.md)) and is not re-exported.

## Relationship to trace_file

`trace_file` and `otel_endpoint` are independent:

| Setting | Format | Purpose |
|---------|--------|---------|
| `trace_file` | Custom JSONL (flat) | Internal trace store, queryable via `/traces` API |
| `otel_endpoint` | OTLP JSON (nested) | Standard export for external observability tools |

You can use both simultaneously. The internal format is simpler to query with `jq`; the OTLP format is compatible with the entire observability ecosystem.
