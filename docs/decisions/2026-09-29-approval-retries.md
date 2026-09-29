# Retries while pending meet the same approval; after a refusal they learn it

- **Problem**: an agent that waits for a human by retrying the call opened a new pending approval on every retry (five retries, six requests), and after a denial its retry opened yet another one, so it could never learn the answer.
- **Decision**: `FindPending` returns the approval still waiting for exactly this call (agent, tool, arguments as JSON) and the MCP path answers with it, recording nothing new; `ClaimDenied`, twin of `ClaimApproved`, hands the retry the refusal once, as an error naming who refused.
- **Why**: waiting by retry is how a non-blocking agent follows a human decision (scout7 does it with `approval_wait`); the queue must show one request, and the agent must be able to stop.
- **Where**: `approval/store.go`, `mcp/server.go`, tests in `mcp/approve_retry_test.go` (red without the fix). Limit: approvals are keyed by agent, not by the human the agent acts for.
