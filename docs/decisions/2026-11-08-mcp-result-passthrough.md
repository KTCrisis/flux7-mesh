# MCP upstream results pass through unchanged

- **Problem**: since the first MCP server mode (2026-04-04), mesh7 serialized an upstream's whole CallToolResult into one text block. An upstream `isError` reached the client as an apparent success, `structuredContent` vanished from the top level, and images or audio became base64 inside JSON. Found 2026-11-08 while probing SDK v2 compatibility.
- **Decision**: for a tool whose source is `mcp`, `tools/call` returns the upstream result as is (`mcp/result.go`, `toolResult`). OpenAPI and CLI backends, which return plain data, keep the single text block. A backend failure now carries `isError: true`.
- **Why**: a proxy must be transparent; programmatic clients (SDKs, retry loops, output-schema validation) read the result's structure, not its text.
- **Where**: `mcp/result.go`, the four call sites in `mcp/server.go`, tests in `mcp/result_test.go`. scout7 (`mesh/mcp.go`) unwrapped the old shape and still works; its workaround can now be simplified.
