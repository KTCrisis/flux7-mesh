# The mesh sends the trace, and on opt-in the agent, in tools/call _meta

- **Problem**: MCP upstreams were called with `context.Background()`: an upstream like mem7 could not tell which governed call or which agent a request came from, so memories had no provenance and could not be scoped.
- **Decision**: every `tools/call` to an MCP upstream carries the W3C `traceparent` in `_meta`; the authenticated agent (`art.flux7/agent`) is added only for upstreams with `forward_identity: true`. Upstream `headers` and `memory.token` now go through `${VAR}` expansion.
- **Why**: `_meta` is the MCP specification's place for request metadata and works over every transport; identity is opt-in because a remote server has no use for agent names; expansion keeps bearer tokens in the service's environment file.
- **Where**: `internal/callmeta`, `mcp/meta.go`, `mcp/client.go`, `proxy/handler.go` (`ForwardAs`), `config/config.go`.
