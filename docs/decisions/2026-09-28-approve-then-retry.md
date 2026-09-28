# A human approves out of band; the agent's retry of the same call runs once

- **Problem**: a regular MCP agent hitting `human_approval` was told to call `approval.resolve`, which only supervisor agents may call, and a human approving through the CLI, the console or HTTP had nothing to replay the call: a newcomer's first approval timed out.
- **Decision**: the agent is told what to relay (`mesh approve <id>`, the console, `POST /approvals/<id>/approve`) and to call again with the same arguments; `ClaimApproved` lets that retry run once on the human's approval (same agent, tool and arguments, within the approval's validity), then uses it up. The `mesh` CLI ships in every release archive.
- **Why**: a blocking call would freeze the whole stdio session while it waits; the retry keeps calls non-blocking and gives each approval exactly one effect. It is the pattern the MCP spec 2026-07-28 names MRTR (`resultType: input_required`), which mesh7 can adopt later without changing the flow.
- **Where**: `approval/store.go` (`ClaimApproved`), `mcp/server.go` (`approvalRequiredText`), `.goreleaser.yaml`; v0.17.0.
