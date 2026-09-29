# Auto-approve from flux7-memory

Before submitting a `human_approval` call to the approval queue, flux7-mesh asks [flux7-memory](https://github.com/KTCrisis/flux7-memory) whether a human has already approved the same thing enough times. If so, the call passes at once, traced as `supervisor:mem7`.

What counts as "the same thing" was tightened on 2026-09-29, after the rule turned out to be far looser than it read:

- only **human** approvals count, of **exactly** this tool and this agent;
- **one refusal**, from anyone (a human or a supervisor), blocks;
- only tools that **read** may be approved from precedents at all (`auto_approve_writes` restores the former behaviour).

## How it works

```{.text .f7-diagram}
Tool call → policy: human_approval
  │
  ├─ Level 0: Policy engine (static rules, instant)
  │
  ├─ Level 1: flux7-memory precedents (~10 ms)
  │   the tool reads (named read tool)?          no → skip
  │   human approvals of this tool + agent  ≥ min_approvals
  │   and no refusal of this tool + agent           → auto-approve (supervisor:mem7)
  │   else                                          → escalate
  │
  ├─ Level 1+: External supervisor (sup7: rules, then Jev or a local model)
  │   polls the approval queue, evaluates, resolves
  │
  └─ Level 2: Human (terminal, CLI or flux7-console)
```

Every decision is written to flux7-memory as a fact tagged with who settled it: `by:human`, `by:supervisor` (sup7), `by:mem7` (an auto-approval) or `by:system` (an expiry). Precedents are read with `memory_list` on exact tags (`decision`, `approved`, `by:human`, `<tool>`, `agent:<id>`; then `denied` for refusals), never with a semantic search.

## Why these limits

Three flaws were found in production on 2026-09-29, each confirmed on real data:

| Flaw | Consequence | Now |
|---|---|---|
| A precedent was keyed on tool and agent, not on the arguments | three approved `filesystem.write_file` in a project let a fourth write anywhere (`~/.bashrc`) through, around the supervisor | precedents approve tools that read only |
| Auto-approvals were written back as approvals, and counted | "3 prior approvals", then 4, 5, 6, without a human | only `by:human` approvals count |
| sup7's approvals counted like a human's | an automatic L1 decision became a way around L1 | idem |
| The count read the first 10 results of a semantic search | a refusal past the tenth result was never seen, and nothing checked the line was about this tool | exact tags, every decision of the couple |

Decisions written before the `by:` tag carry no resolver and no longer count: precedents start again from human decisions.

"Reads" is decided by [tool classification](https://docs.flux7.art/mesh7/tool-classification/): a named tool whose declared metadata says it reads. Writes, generic tools (shell, SQL, code) and tools of unknown access always go on to the supervisor or a human.

## Configuration

```yaml
memory:
  url: http://localhost:9070    # flux7-memory daemon URL
  token: ""                     # optional Bearer token

supervisor:
  auto_approve: true            # default true when memory.url is set
  min_approvals: 3              # human approvals needed (default 3)
  auto_approve_writes: false    # default: precedents approve reads only
```

These three settings, with the approval timeout and wait, can be changed at runtime from the control plane (`PUT /approvals/settings`) or from the flux7-console Approvals page: see [Approval flow](approval-flow.md#changing-approval-settings-at-runtime).

## Seeing and forgetting precedents

```bash
# every tool + agent flux7-memory remembers, and what the next call would do
curl -s http://localhost:9090/approvals/precedents | python3 -m json.tool
```

```json
{
  "enabled": true,
  "min_approvals": 3,
  "precedents": [
    {
      "tool": "filesystem.write_file", "agent": "claude",
      "human_approved": 0, "other_approved": 1, "refused": 0, "untagged": 5,
      "auto_approvable": false, "would_auto_approve": false
    }
  ]
}
```

`other_approved` are approvals by sup7 or by auto-approval (never counted), `untagged` are decisions from before the `by:` tag. To start a couple over:

```bash
curl -s -X POST http://localhost:9090/approvals/precedents/forget \
  -H "Content-Type: application/json" -d '{"tool":"filesystem.write_file","agent":"claude"}'
```

Both are control-plane endpoints (loopback, or `Authorization: Bearer <admin_token>`). flux7-console shows the same table, with a Forget button, under the approval history.

## Example: testing end-to-end

Prerequisites: flux7-mesh built after 2026-09-29, flux7-memory running on `:9070`, and a policy that sends a **read** tool to approval, for the test:

```yaml
rules:
  - tools: ["filesystem.read_text_file"]
    action: human_approval
```

### 1. Seed 3 human approvals

```bash
for i in 1 2 3; do
  curl -s -X POST http://localhost:9070/rpc -H "Content-Type: application/json" -d "{
    \"jsonrpc\": \"2.0\", \"id\": $i, \"method\": \"tools/call\",
    \"params\": {\"name\": \"memory_store\", \"arguments\": {
      \"key\": \"decision.filesystem.read_text_file.test${i}\",
      \"value\": \"approved by user:alice — agent:claude tool:filesystem.read_text_file\",
      \"tags\": [\"decision\", \"approved\", \"filesystem.read_text_file\", \"by:human\", \"agent:claude\"],
      \"agent\": \"flux7-mesh\"}}}"
done
```

### 2. Call the tool

```bash
curl -s -X POST http://localhost:9090/tool/filesystem.read_text_file \
  -H "Authorization: Bearer agent:claude" -H "Content-Type: application/json" \
  -d '{"params":{"path":"/tmp/test.txt"}}' | python3 -m json.tool
```

Expected: no approval, `"policy": "allow"`, and the trace carries `"policy_rule": "supervisor:mem7"`.

### 3. Check what does not pass

- the same seeds with `filesystem.write_file`: the call goes to the approval queue (a write is never approved from precedents);
- seeds tagged `by:supervisor` instead of `by:human`: the call goes to the queue (a supervisor's approval is not a precedent);
- one more fact tagged `denied`: the call goes to the queue.

## Behavior summary

| Scenario | Action | Traced as |
|----------|--------|-----------|
| Read tool, ≥ `min_approvals` human approvals, no refusal | Auto-approve | `supervisor:mem7` |
| Write, generic or unknown-access tool | Skip check (unless `auto_approve_writes`) | Normal approval flow |
| Any refusal of this tool + agent | Escalate | Normal approval flow |
| Not enough human approvals | Escalate | Normal approval flow |
| flux7-memory down or unreadable answer | Escalate | Normal approval flow |
| Params look like a prompt injection | Skip check, escalate | Normal approval flow |
| `auto_approve: false` | Skip check | Normal approval flow |

## Graceful degradation

- flux7-memory unreachable → escalate (3 s timeout, never blocks)
- flux7-memory returns an error or an unexpected answer → escalate
- All failure modes fall back to the normal approval flow: auto-approval is additive, never subtractive.

## Injection guard

Auto-approval replays a human's past decision on a routine pattern. A call whose parameters carry a prompt-injection marker is no longer routine, so the flux7-memory lookup is skipped entirely: the call is logged (`injection risk detected, forcing human review`) and goes to the normal approval queue.

The guard is a single function shared by every transport (REST, MCP stdio, MCP Streamable HTTP). It is a regex tripwire on known phrasings, not a classifier; see the known limits on the [overview](https://docs.flux7.art/mesh7/#known-limits).

## Metrics

Monitor the flux7-memory write path at `GET /metrics` (Prometheus text format). It is a control-plane endpoint: scrape it from loopback, or send `Authorization: Bearer <admin_token>` (see [Control Plane Auth](https://docs.flux7.art/mesh7/control-plane-auth/)).

```
agent_mesh_mem7_writes_attempted_total
agent_mesh_mem7_writes_succeeded_total
agent_mesh_mem7_writes_failed_total
```

A growing `failed` count means flux7-memory is down: auto-approval escalates everything until it recovers.
