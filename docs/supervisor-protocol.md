# Supervisor Protocol

Agent Mesh exposes a rich approval API so that external **supervisor agents** can review and resolve approval requests on behalf of humans.

The supervisor is not built into agent-mesh. It's any external process — Python script, Go service, LangChain agent, Claude API call — that polls for pending approvals and resolves them. Agent-mesh provides the protocol; you bring the logic.

## Why

Human-in-the-loop doesn't scale:

- **Multiple concurrent agents** generate approval requests faster than a human can review them
- **Overnight runs** stall at approval gates while the human sleeps
- **80% of approvals are routine** ("yes, you can write to the project directory") — only 20% need real judgment

A supervisor agent handles the routine and escalates the rest.

## Trust hierarchy

```
Level 0: Policy engine         — static rules, instant, no judgment
Level 1: Supervisor agent      — dynamic evaluation, fast, bounded judgment
Level 2: Human                 — full judgment, slow, expensive attention
```

| Level | Handles | Example |
|-------|---------|---------|
| **Policy (L0)** | Black/white rules | `allow` reads, `deny` deletes |
| **Supervisor (L1)** | Gray area, routine | write to project dir, draft email to known contact |
| **Human (L2)** | High-stakes, ambiguous | send to external recipient, write outside project scope |

The goal is not to remove the human. It's to **protect human attention** for decisions that actually need it.

## Architecture

```
                          ┌─────────────────────────────┐
                          │        agent-mesh            │
                          │                              │
Worker Agent ────────────>│  policy ──> approval store   │
                          │                  │           │
                          │                  v           │
                          │         GET /approvals       │
                          │              │               │
                          └──────────────┼───────────────┘
                                         │
                                         v
                               ┌───────────────────┐
                               │ Supervisor Agent   │
                               │                    │
                               │  evaluate risk     │
                               │  check context     │
                               │  decide:           │
                               │   - approve        │──> POST /approvals/{id}/approve
                               │   - deny           │──> POST /approvals/{id}/deny
                               │   - escalate       │──> notify human
                               │                    │
                               └───────────────────┘
                                         │
                                    (escalation only)
                                         │
                                         v
                                       Human
```

## API reference

### List pending approvals

```
GET /approvals?status=pending
```

Optional tool filter for domain-specific supervisors:

```
GET /approvals?status=pending&tool=filesystem.*
GET /approvals?status=pending&tool=gmail.*
```

The `tool` parameter supports glob patterns (Go `filepath.Match` syntax).

**Response:**

```json
[
  {
    "id": "a1b2c3d4e5f67890",
    "agent_id": "claude",
    "tool": "filesystem.write_file",
    "params": {"path": "/home/user/project/main.go", "content": "..."},
    "policy_rule": "claude:rule-2",
    "status": "pending",
    "created_at": "2026-04-10T14:30:00Z",
    "remaining": "4m30s",
    "injection_risk": false
  }
]
```

When `supervisor.expose_content: false` is configured, large or content-like param values are replaced with structural metadata:

```json
{
  "params": {
    "path": "/home/user/project/main.go",
    "content": {
      "content_length": 1234,
      "content_sha256": "a1b2c3d4...",
      "content_type_detected": "text/plain"
    }
  }
}
```

### Get approval detail (with context)

```
GET /approvals/{id}
```

Returns the approval plus the agent's recent activity and active grants — context for evaluation:

```json
{
  "id": "a1b2c3d4e5f67890",
  "agent_id": "claude",
  "tool": "filesystem.write_file",
  "params": {"path": "/home/user/project/main.go"},
  "policy_rule": "claude:rule-2",
  "status": "pending",
  "created_at": "2026-04-10T14:30:00Z",
  "remaining": "4m30s",
  "injection_risk": false,
  "recent_traces": [
    {
      "trace_id": "abc123...",
      "tool": "filesystem.read_file",
      "policy": "allow",
      "timestamp": "2026-04-10T14:29:55Z"
    },
    {
      "trace_id": "def456...",
      "tool": "filesystem.list_directory",
      "policy": "allow",
      "timestamp": "2026-04-10T14:29:50Z"
    }
  ],
  "active_grants": [
    {
      "id": "g_789",
      "agent": "claude",
      "tools": "filesystem.read_*",
      "expires_at": "2026-04-10T15:00:00Z",
      "remaining": "30m0s",
      "granted_by": "http:127.0.0.1:9090"
    }
  ]
}
```

### Approve

```
POST /approvals/{id}/approve
```

**Body (all fields optional):**

```json
{
  "resolved_by": "agent:supervisor",
  "reasoning": "write to project source directory, valid Go content, consistent with recent activity",
  "confidence": 0.95
}
```

| Field | Type | Description |
|-------|------|-------------|
| `resolved_by` | string | Who resolved it. Defaults to `http:<remote_addr>` |
| `reasoning` | string | Explanation of the decision (stored in traces) |
| `confidence` | float64 | 0.0–1.0, how confident the supervisor is (stored in traces) |

**Responses:**

| Status | Meaning |
|--------|---------|
| 200 | Approved. The original tool call is forwarded to the backend. |
| 404 | Approval ID not found |
| 409 | Already resolved |

### Deny

```
POST /approvals/{id}/deny
```

Same body format as approve. On deny, the agent receives a 403 response.

## Configuration

### Content isolation

When `expose_content` is `false`, the approval API replaces raw content values with structural metadata. This reduces the prompt injection attack surface — the supervisor evaluates structure, not content.

```yaml
supervisor:
  expose_content: false    # default: true
```

Content-like keys (`content`, `body`, `data`, `file_content`, `text`) and any string value longer than 256 bytes are redacted to:

```json
{
  "content_length": 1234,
  "content_sha256": "a1b2c3d4...",
  "content_type_detected": "text/plain"
}
```

Detected content types: `application/json`, `text/xml`, `text/html`, `text/plain`, `application/octet-stream`.

### Injection detection

Every approval view includes an `injection_risk` boolean. It is `true` when tool params contain patterns commonly used in prompt injection attacks:

- "ignore previous instructions"
- "you are now"
- "system prompt:"
- "override policy"
- "pre-approved"
- "do not deny" / "do not escalate"
- "pretend you are" / "act as"
- "IMPORTANT: approve/ignore/override"
- "confidence should be"

This is a best-effort heuristic, not a security guarantee. It raises the cost of attack and flags suspicious requests for closer review.

## Building a supervisor

A supervisor is any program that implements this loop:

```python
import requests
import time

MESH_URL = "http://localhost:9090"
CONFIDENCE_THRESHOLD = 0.8

while True:
    # 1. Poll for pending approvals (optionally filter by tool)
    pending = requests.get(f"{MESH_URL}/approvals",
        params={"status": "pending", "tool": "filesystem.*"}).json()

    for approval in pending:
        # 2. Get full context
        detail = requests.get(f"{MESH_URL}/approvals/{approval['id']}").json()

        # 3. Evaluate
        verdict = evaluate(detail)

        # 4. Act
        if verdict["confidence"] >= CONFIDENCE_THRESHOLD:
            requests.post(f"{MESH_URL}/approvals/{approval['id']}/{verdict['action']}", json={
                "resolved_by": "agent:supervisor",
                "reasoning": verdict["reasoning"],
                "confidence": verdict["confidence"],
            })
        else:
            # Escalate to human — leave pending
            notify_human(detail, verdict["reasoning"])

    time.sleep(2)
```

### Evaluation guidelines

The supervisor should evaluate **structural properties**, not interpret content semantically:

| Good (structural) | Bad (semantic) |
|-------------------|----------------|
| "path is within project directory" | "content says it's pre-approved" |
| "file type matches extension" | "the code looks safe" |
| "agent has been writing to this area recently" | "the comment says this is a fix" |
| "request rate is normal" | "the agent seems trustworthy" |

Structural evaluation is resistant to prompt injection. Semantic evaluation is vulnerable.

### Context signals available

From `GET /approvals/{id}`:

| Signal | Field | Use |
|--------|-------|-----|
| What tool | `tool` | Scope check |
| What params | `params` (or metadata if redacted) | Path analysis, size check |
| Which agent | `agent_id` | Trust level per agent |
| Which policy rule | `policy_rule` | Why it needs approval |
| Injection risk | `injection_risk` | Auto-escalate if true |
| Recent activity | `recent_traces` | Pattern consistency check |
| Active grants | `active_grants` | What's already trusted |
| Time remaining | `remaining` | Urgency signal |

### Escalation criteria

Recommended reasons to escalate to a human instead of auto-resolving:

| Condition | Why |
|-----------|-----|
| Confidence below threshold | Supervisor is unsure |
| `injection_risk: true` | Suspicious content in params |
| Path outside project scope | Higher risk write |
| Tool is high-stakes | Email send, database write, deploy |
| Rate anomaly | Unusual number of requests from the agent |
| Agent is unknown | No recent trace history to establish pattern |

## Observability

Every supervisor decision is recorded in traces:

```bash
# All supervisor-resolved traces
curl http://localhost:9090/traces | \
  jq '[.[] | select(.supervisor_reasoning != null)]'

# Average supervisor confidence
curl http://localhost:9090/traces | \
  jq '[.[] | select(.supervisor_confidence > 0) | .supervisor_confidence] | add / length'

# Low-confidence approvals (potential miscalibration)
curl http://localhost:9090/traces | \
  jq '[.[] | select(.supervisor_confidence > 0 and .supervisor_confidence < 0.85)]'
```

Trace fields added by supervisor resolution:

| Field | Type | Description |
|-------|------|-------------|
| `supervisor_reasoning` | string | Why the supervisor approved/denied |
| `supervisor_confidence` | float64 | How confident the supervisor was (0.0–1.0) |
| `approved_by` | string | `"agent:supervisor"` (vs `"http:127.0.0.1:..."` for humans) |

## The spectrum of autonomy

A single tool can move along this spectrum over time as trust is established:

```
deny <──── human_approval <──── supervisor <──── allow

"never"    "human decides"    "agent decides,     "always"
                               human on escalation"
```

Example progression for `gmail.send`:

1. **Day 1**: `deny` — not ready
2. **Day 30**: `human_approval` — human reviews every send
3. **Day 90**: supervisor — agent reviews, escalates external recipients
4. **Day 180**: `allow` for internal recipients — trusted pattern

The policies evolve as trust is established through trace data.

## Multi-supervisor topology

Multiple supervisors can coexist with different scopes using the `?tool=` filter:

```
Worker Agents ──> agent-mesh ──> Approval Store
                                      │
                         ┌────────────┼────────────┐
                         v            v            v
                   Supervisor A  Supervisor B    Human
                   (filesystem)  (gmail)        (everything else)
```

```bash
# Supervisor A — only filesystem approvals
GET /approvals?status=pending&tool=filesystem.*

# Supervisor B — only gmail approvals
GET /approvals?status=pending&tool=gmail.*
```

Domain-specific supervision improves judgment quality: the filesystem supervisor understands code patterns, the gmail supervisor understands communication norms.

## Reference implementation

A complete supervisor implementation is available in the [agent7](https://github.com/KTCrisis/flux7-console) repository at `backend/app/services/supervisor/`.

### Features

- **Rule engine** — first-match-wins rules with a simple DSL (`starts_with`, `equals`, `contains`). Fast path for known patterns (0ms).
- **LLM fallback** — when no rule matches, calls Ollama for evaluation (~20s). Configurable model, system prompt, and confidence threshold.
- **Process manager** — auto-spawns agent-mesh when it's down, monitors health, restarts on crash.
- **Memory integration** — stores decisions in memory-mcp (via agent-mesh), recalls them on startup for context continuity across sessions.
- **JSONL audit trail** — every decision logged with reasoning, confidence, rule matched, and evaluation time.

### Quick start

```bash
cd ~/agent7
python -m backend.app.services.supervisor --config supervisor.yaml
```

See [docs/deployment-modes.md](deployment-modes.md) for full setup options.
