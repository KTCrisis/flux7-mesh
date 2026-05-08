# Auto-approve from mem7

agent-mesh queries [mem7](https://github.com/KTCrisis/mem7) for past approval decisions before submitting to the approval queue. If a tool+agent pattern has enough consistent approvals (default: 3+, 0 rejections), the request is auto-approved.

## How it works

```
Tool call → policy: human_approval
  │
  ├─ Level 0: Policy engine (static rules, instant)
  │
  ├─ Level 1: Built-in mem7 lookup (~100ms)
  │   query mem7 for past decisions (tool + agent + tags=["decision"])
  │   3+ approvals, 0 rejections → auto-approve (supervisor:mem7)
  │   else → escalate
  │
  ├─ Level 1+: External supervisor (Python, rules + Ollama, ~20s)
  │   polls approval queue, evaluates, resolves
  │   else → escalate
  │
  └─ Level 2: Human (terminal prompt or agent7 UI)
```

Each auto-approved decision is written back to mem7, reinforcing the pattern for future queries.

## Configuration

```yaml
memory:
  url: http://localhost:9070    # mem7 daemon URL
  token: ""                     # optional Bearer token

supervisor:
  auto_approve: true            # default true when memory.url is set
  min_approvals: 3              # threshold (default 3)
```

Set `auto_approve: false` to disable even when mem7 is configured.

## Example: testing end-to-end

Prerequisites: agent-mesh v0.9.1+, mem7 running on `:9070`.

### 1. Verify mem7 is reachable

```bash
curl -s http://localhost:9070/rpc \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | head -c 100
```

### 2. Seed 3 approval decisions

Simulate 3 past approvals for `filesystem.write_file` by agent `claude`:

```bash
for i in 1 2 3; do
  curl -s -X POST http://localhost:9070/rpc \
    -H "Content-Type: application/json" \
    -d "{
      \"jsonrpc\": \"2.0\",
      \"id\": $i,
      \"method\": \"tools/call\",
      \"params\": {
        \"name\": \"memory_store\",
        \"arguments\": {
          \"key\": \"decision.filesystem.write_file.test${i}\",
          \"value\": \"approved by user:marc — agent:claude tool:filesystem.write_file reason:routine write\",
          \"tags\": [\"decision\", \"approved\", \"filesystem.write_file\", \"agent:claude\"],
          \"agent\": \"agent-mesh\"
        }
      }
    }"
done
```

### 3. Call the tool via HTTP

```bash
curl -s -X POST http://localhost:9090/tool/filesystem.write_file \
  -H "Authorization: Bearer agent:claude" \
  -H "Content-Type: application/json" \
  -d '{"params":{"path":"/home/user/test.txt","content":"auto-approve test"}}' \
  | python3 -m json.tool
```

Expected: no approval prompt, immediate response with `"policy": "allow"`.

### 4. Check the trace

```bash
curl -s "http://localhost:9090/traces?tool=filesystem.write_file&limit=1" \
  | python3 -m json.tool
```

Expected trace entry:

```json
{
    "agent_id": "claude",
    "tool": "filesystem.write_file",
    "policy": "allow",
    "policy_rule": "supervisor:mem7",
    "latency_ms": 3
}
```

`policy_rule: "supervisor:mem7"` confirms the built-in Level 1 supervisor resolved the request. The original policy was `human_approval`, but mem7 had 3 prior approvals — so it passed without blocking.

### 5. Verify the decision was written back

```bash
curl -s -X POST http://localhost:9070/rpc \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
      "name": "memory_search",
      "arguments": {
        "query": "decision filesystem.write_file",
        "tags": ["decision"],
        "limit": 5
      }
    }
  }' | python3 -m json.tool
```

You should see 4 entries: the 3 seeded + 1 auto-approval by `supervisor:mem7`.

## Behavior summary

| Scenario | Action | Traced as |
|----------|--------|-----------|
| 3+ approvals, 0 rejections | Auto-approve | `supervisor:mem7` |
| Any rejections in history | Escalate | Normal approval flow |
| Not enough history | Escalate | Normal approval flow |
| mem7 down or unreachable | Escalate | Normal approval flow |
| `auto_approve: false` | Skip check | Normal approval flow |

## Graceful degradation

- mem7 unreachable → escalate (3s timeout, never blocks)
- mem7 returns error → escalate
- Search returns no results → escalate
- All failure modes fall back to the normal approval flow — the auto-approve is additive, never subtractive.

## Metrics

Monitor the mem7 write path at `GET /metrics`:

```
agent_mesh_mem7_writes_attempted_total
agent_mesh_mem7_writes_succeeded_total
agent_mesh_mem7_writes_failed_total
```

A growing `failed` count means mem7 is down — auto-approve will escalate everything until it recovers.
