# An upstream down at start is retried

- **Problem**: an MCP upstream unreachable when mesh7 started was logged and dropped: it never came back, and `/mcp-servers` did not even list it (mem7 restarted at the same time as the mesh on 03/10).
- **Decision**: such an upstream is retried in the background, 5 s then doubling up to 5 min, until it answers or mesh7 stops; it shows as `retrying` with its last error; on success its tools are registered and pinned as at start.
- **Why**: a restart order (`After=mem7.service`) fixes one local case, not a remote server briefly down; a missing server should be visible, not silent.
- **Where**: `mcp/retry.go`, `mcp/manager.go` (`SetRetrying`), `cmd/mesh7/mesh.go` (`connect`, `retry`). Limit: MCP clients of the mesh are not notified (`tools/list_changed`); they see the tools on their next listing.
