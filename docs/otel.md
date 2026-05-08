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

Span kind is `SERVER` (3). Status code is `OK` (1) for allowed calls, `ERROR` (2) for denied or failed calls.

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

Spans are POSTed to `{endpoint}/v1/traces` with `Content-Type: application/json`. Export is async (non-blocking) with a 5-second timeout.

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
  → Store.Record() triggers OTEL export (async goroutine)
       → Entry converted to OTLP span
       → Written to file / POSTed to endpoint / printed to stderr
```

The OTEL exporter is a hook on the existing trace store. It runs asynchronously and never blocks tool calls. If the OTLP endpoint is down, a warning is logged and the call proceeds normally.

## Relationship to trace_file

`trace_file` and `otel_endpoint` are independent:

| Setting | Format | Purpose |
|---------|--------|---------|
| `trace_file` | Custom JSONL (flat) | Internal trace store, queryable via `/traces` API |
| `otel_endpoint` | OTLP JSON (nested) | Standard export for external observability tools |

You can use both simultaneously. The internal format is simpler to query with `jq`; the OTLP format is compatible with the entire observability ecosystem.
