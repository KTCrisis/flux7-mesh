# A call over MCP HTTP joins the caller's trace

- **Problem**: the `/mcp` transport read no trace header, so every MCP call opened a trace of its own; behind Kong's OpenTelemetry plugin, the gateway's spans and the mesh's never met in a collector, unlike the REST data plane.
- **Decision**: `handlePost` builds the context from `Traceparent` (or `X-Trace-Id`) and `HandleRequestWith` carries it to `handleToolsCall`, which stamps the trace entries and calls the backend within it; the trace is fixed before an approval is submitted. stdio keeps its own trace per call.
- **Why**: one call, one trace from the gateway to the tool is what an operator reads first; the approval used to be persisted with an empty trace id.
- **Where**: `mcp/http_server.go`, `mcp/server.go`, tests in `mcp/http_server_test.go`. Verified in Jaeger behind a Kong Konnect node: the mesh span sits under Kong's balancer span.
