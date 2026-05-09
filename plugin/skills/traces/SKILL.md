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

Run: `curl -s "<url>" | jq '.[-${2:-20}:]'`

Display results as a table: timestamp, agent, tool, action (allow/deny), latency, status.
