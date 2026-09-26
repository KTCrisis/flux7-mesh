---
name: traces
description: Query and display recent flux7-mesh traces (tool calls, policy decisions, latency)
user-invocable: true
argument-hint: "[agent] [tool] [limit]"
allowed-tools:
  - Bash
---

Query flux7-mesh traces.

Arguments:
- $0 = agent filter (optional, default: all)
- $1 = tool filter (optional, default: all)  
- $2 = limit (optional, default: 20)

Build the query URL: `http://localhost:9090/traces` with query params:
- If $0 is provided and not empty: `?agent=$0`
- If $1 is provided and not empty: `&tool=$1`

Run: `curl -s "<url>" | jq '[sort_by(.timestamp) | .[] | {trace_id: .trace_id[0:12], session_id: (.session_id // "-")[0:12], ts: .timestamp[0:19], agent: .agent_id, tool: .tool, policy: .policy, latency_ms: .latency_ms, in_tokens: .estimated_input_tokens, out_tokens: .estimated_output_tokens}] | .[-${2:-20}:]'`

Display results as a table: trace_id (12 chars), session_id (12 chars or "-"), timestamp, agent, tool, policy (allow/deny), latency, input tokens, output tokens.
