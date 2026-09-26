# Traces are an append-only HMAC chain

- **Problem**: traces could be edited without detection, and approval outcomes (who approved, supervisor reasoning, backend status) were set in memory only, so the file lost them at every restart.
- **Decision**: updates are appended as revisions; every line carries `seq`, `alg`, `prev_hash`, `hash` (HMAC-SHA256 with `MESH_TRACE_KEY`, SHA-256 without), chained across restarts and rotations; `mesh7 trace verify` finds the first break; the head is logged at start and stop.
- **Why**: an audit trail is evidence only if altering it is detectable and rewriting it needs a secret; a flat envelope appended to the existing JSON keeps old lines, `jq` and the loader working unchanged.
- **Where**: `trace/integrity.go`, `trace/store.go`, `cmd/mesh7/trace.go`, `docs/trace-integrity.md`. Known limits there: truncated tail, one rotated file, one writer per file.
