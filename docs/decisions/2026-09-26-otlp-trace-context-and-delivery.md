# The mesh joins the caller's trace; OTLP leaves in retried batches

- **Problem**: the caller's span in `traceparent` was dropped (tree broken behind Kong), calls sharing a caller trace all got the same span ID, the outgoing `traceparent` had an all-zero parent (invalid W3C), and OTLP/HTTP was one unretried POST per call in its own goroutine.
- **Decision**: `trace.NewContext` validates the headers; with a `traceparent` the caller span is the parent and the mesh span is random, otherwise the span stays derived from the trace ID; backends get the mesh span as parent. OTLP/HTTP goes through a bounded queue, batches of 128 or 2 s, 3 retries on network/429/5xx, drop-and-log when full, drain on Close.
- **Why**: a governance proxy that breaks the caller's trace is invisible in the tools auditors already use; losing spans silently when the collector blinks is worse than counting them.
- **Where**: `trace/context.go`, `trace/otel.go`, `trace/store.go` (lineage resolves to the stored span), `proxy/handler.go` (`Forward` takes a `trace.Context`), `docs/otel.md`.
