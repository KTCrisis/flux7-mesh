# A non-blocking call waits a few seconds for an automatic decision

- **Problem**: sup7 decides a pending call in about half a second, but in MCP mode the agent had already been told "approval required" and had to retry the same call; an automatic approval only helped an agent that thought of retrying, and an LLM rewriting its arguments on retry opened a new approval.
- **Decision**: `approval.wait_seconds` (default 0) makes the non-blocking path wait on the approval's result for that long; decided in time, the call runs or is refused in the same request and `Store.Claim` marks the approval used, so a retry cannot run it twice; otherwise the usual "approval required" flow applies.
- **Why**: the wait is sized for a supervisor, never for a human (a stdio session is blocked while it waits); it keeps the non-blocking flow of 28/09 and makes L1 invisible to the agent when it answers quickly, which open-world tools (Bash) sent to Jev need.
- **Where**: `config/config.go` (`WaitSeconds`), `approval/store.go` (`Claim`), `mcp/server.go` (the wait), `mcp/http_server.go`, `cmd/mesh7`; tests in `mcp/approval_wait_test.go`; doc `docs/approval-flow.md`.
