# GET /traces finds one trace by id

- **Problem**: a memory in mem7, a log line or a console link names a trace id, but `/traces` only listed the last 100 calls and ignored `limit`, so an older call could not be found.
- **Decision**: `?trace=<id>` returns every entry of that trace across everything the store keeps (10,000 calls); `limit` is honoured, default 100, capped at 1000.
- **Why**: provenance is only useful if it can be followed back; the store already holds the entries, the API just did not let anyone reach them.
- **Where**: `trace/store.go` (`QueryTrace`), `proxy/handler.go` (`handleTraces`); console `/mesh/traces?trace=`.
