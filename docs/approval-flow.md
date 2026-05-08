# Approval Flow

When a policy rule has `action: human_approval`, the tool call enters an approval queue. The agent waits. A human (or supervisor) resolves it. The tool call proceeds or is rejected.

## How it works

```
Agent calls filesystem.write_file
  │
  ├── Policy: human_approval
  │
  ├── Check 1: Temporal grant active?
  │   yes → bypass approval, proceed
  │
  ├── Check 2: mem7 auto-approve? (3+ past approvals)
  │   yes → proceed, traced as supervisor:mem7
  │
  └── Submit to approval queue
      │
      ├── MCP mode: prompt in terminal OR block for supervisor
      ├── HTTP mode: return 202, agent polls or uses callback
      │
      └── Human/supervisor resolves
          ├── approved → tool call executed, result returned
          ├── denied → 403 returned
          └── timeout → 408 returned (default 5 min)
```

## Resolving approvals

### In Claude Code (MCP mode)

Claude Code shows a permission prompt inline. The developer says yes or no. This is the default for solo dev use.

### Via MCP virtual tools

Agents can resolve approvals themselves (unless supervisor mode is on):

```
approval.pending                → list pending approvals
approval.resolve {id, decision} → approve or deny
```

### Via CLI

```bash
# List pending
mesh pending

# Approve (prefix match)
mesh approve a1b2c3d4

# Deny
mesh deny a1b2c3d4

# Watch (live updates)
mesh watch
```

### Via HTTP API

```bash
# List all approvals
curl http://localhost:9090/approvals

# Get details (includes recent traces and active grants)
curl http://localhost:9090/approvals/a1b2c3d4e5f6g7h8

# Approve
curl -X POST http://localhost:9090/approvals/a1b2c3d4/approve \
  -H "Content-Type: application/json" \
  -d '{"resolved_by":"user:marc","reasoning":"routine operation","confidence":0.95}'

# Deny
curl -X POST http://localhost:9090/approvals/a1b2c3d4/deny \
  -d '{"resolved_by":"user:marc","reasoning":"unexpected target"}'
```

Prefix matching: `a1b2c3d4` matches the full ID if the prefix is unique.

## Temporal grants

Repeated approvals for the same tool pattern get tedious. Grants are like `sudo` — a temporary bypass:

```
grant.create {tools: "filesystem.write_*", duration: "30m"}
```

For the next 30 minutes, all `filesystem.write_*` calls bypass the approval queue. Traced as `grant:<id>`.

### MCP tools

```
grant.create  {tools: "filesystem.*", duration: "1h"}
grant.list
grant.revoke  {id: "abc123"}
```

### HTTP API

```bash
# Create
curl -X POST http://localhost:9090/grants \
  -d '{"agent":"claude","tools":"filesystem.*","duration":"30m"}'

# List
curl http://localhost:9090/grants

# Revoke
curl -X DELETE http://localhost:9090/grants/abc123
```

!!! warning "Grants only bypass `human_approval`"
    Tools marked `deny` remain blocked. A grant cannot override a deny rule — that requires a policy edit.

## Timeouts

Unanswered approvals time out after 5 minutes (configurable):

```yaml
approval:
  timeout_seconds: 300    # default
```

Timed-out approvals are recorded in traces and written to mem7 (if configured) with status `timeout`.

## Webhooks

Get notified when a new approval is pending:

```yaml
approval:
  notify_url: https://hooks.slack.com/services/...
```

mesh7 POSTs to this URL with the pending approval details. Useful for Slack/Teams alerts.

## Callback URL

HTTP agents can provide a callback URL to receive the resolution:

```bash
curl -X POST http://localhost:9090/tool/gmail.send_email \
  -H "Authorization: Bearer agent:my-bot" \
  -H "X-Callback-URL: http://my-bot:8080/approval-callback" \
  -d '{"params":{"to":"user@example.com","subject":"Hello"}}'
```

When the approval resolves, mesh7 POSTs the result to `X-Callback-URL`.

## Supervisor mode

When `supervisor.enabled: true`, the approval tools (`approval.resolve`, `approval.pending`) are hidden from agents. Only an external supervisor can resolve approvals:

```yaml
supervisor:
  enabled: true
  expose_content: false       # redact params for the supervisor
  supervisor_agents:          # whitelist (glob) for cloud supervisors
    - "supervisor-*"
```

In this mode, MCP tool calls **block** until the supervisor resolves. The agent waits transparently.

See [Supervisor Protocol](supervisor-protocol.md) for the external resolver API.

## Traces

Every approval is traced:

```json
{
  "tool": "filesystem.write_file",
  "policy": "human_approval",
  "approval_id": "a1b2c3d4e5f6g7h8",
  "approval_status": "approved",
  "approved_by": "user:marc",
  "supervisor_reasoning": "routine write operation",
  "supervisor_confidence": 0.95,
  "latency_ms": 4200
}
```

Query approval history:

```bash
curl "http://localhost:9090/traces?tool=filesystem.write_file" | python3 -m json.tool
```

## Decision persistence

When [mem7](mem7-auto-approve.md) is configured, every approval resolution (approve, deny, timeout) is stored as a queryable fact. This enables:

- **Auto-approve** — routine patterns resolve without human intervention
- **Audit trail** — "who approved what, when, why" is queryable
- **Cross-session memory** — decisions survive process restarts

## Next steps

- [Memory Integration](mem7-auto-approve.md) — auto-approve from past decisions
- [CLI Tools](cli-tools.md) — governing git, docker, terraform
- [Deployment Modes](deployment-modes.md) — solo, team, cloud
