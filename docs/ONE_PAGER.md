# agent-mesh — Governance Mesh for AI Agents

## The problem

You're deploying agents. They call tools — file writes, emails, API calls, database queries. You need to answer three questions before going to production :

- **Which agent can call which tool ?** Frameworks don't enforce boundaries. An agent can call anything it discovers.
- **Who approved that action ?** The developer clicked "yes" in a terminal prompt 3 weeks ago. That decision is gone.
- **What happened ?** You have stdout logs somewhere. They're not structured, not queryable, and definitely not auditable.

These aren't agent framework problems. They're infrastructure problems. Service meshes solved them for microservices a decade ago — policy enforcement, observability, access control at the network layer. Agents need the same thing, at the tool call layer.

## What agent-mesh is

A sidecar proxy that sits between agents and their tools. One Go binary, one YAML config, zero dependencies.

```
Agent (Claude, LangChain, script)
  │
  └──► agent-mesh (sidecar)
         ├── policy: allow / deny / human_approval
         ├── rate limiting + loop detection
         ├── temporal grants (sudo for agents)
         ├── approval queue (async, non-blocking)
         ├── traces (JSONL + OTEL)
         └──► tools (MCP servers, OpenAPI, CLI binaries)
```

Agents don't know the proxy exists. They call tools, get results. The governance layer is invisible to the agent, visible to the operator.

**Transports :** MCP stdio (Claude Code, Cursor) · MCP Streamable HTTP at `POST /mcp` (Anthropic Managed Agents, remote clients) · HTTP REST (`POST /tool/{name}`)

## Adaptive governance

Policies start strict. Over time, the system learns.

```
Day 1:  human_approval for all writes
        ↓ human approves filesystem.write 3 times
Day 7:  agent-mesh queries mem7 → 3 approvals, 0 rejections → auto-approve
        ↓ novel tool call, no history
        ↓ external supervisor (rules + LLM) evaluates → approve
Day 30: routine patterns auto-resolve in ~100ms
        humans only see genuinely new or ambiguous requests
```

Three layers :

| Level | Who | Speed | What it handles |
|-------|-----|-------|-----------------|
| 0 | Policy engine | 0ms | Static rules (allow, deny, human_approval) |
| 1 | Built-in mem7 lookup | ~100ms | Routine patterns (3+ past approvals) |
| 1+ | External supervisor | ~20s | Novel cases (rule engine + LLM) |
| 2 | Human | minutes | Unknowns, high-stakes decisions |

Every decision is stored as a fact in [mem7](https://github.com/KTCrisis/flux7-memory). Every tool call is a trace. Both are queryable.

## What makes it different

| | API Gateways (Kong, Apigee) | Agent Frameworks (LangChain, CrewAI) | agent-mesh |
|---|---|---|---|
| **Traffic** | North-south (user → LLM) | Internal (agent runtime) | East-west (agent → tools) |
| **Policy** | API keys, rate limits | None or coarse allow/ask | Semantic YAML rules per agent per tool |
| **Approval** | None | Framework-specific | Async queue, non-blocking, with memory |
| **Identity** | API consumer | Single agent | Per-agent (`agent:claude`, `agent:worker-3`) |
| **Decision persistence** | None | None | Facts in mem7, queryable, auditable |
| **Deployment** | Heavy infrastructure | Embedded in code | Single binary sidecar, zero config to start |

Closest comparable : Microsoft Agent Governance Toolkit. But middleware vs sidecar — agent-mesh requires zero changes to agent code.

## Current state (May 2026)

- **v0.9.1** — 238+ tests, 14 packages, race clean
- **Import** — MCP servers (stdio + SSE), OpenAPI specs, CLI binaries
- **Export** — MCP stdio + MCP Streamable HTTP + HTTP REST
- **Governance** — YAML policies, glob patterns, conditions, per-agent policy files, specificity sort
- **Approval** — async queue, temporal grants, supervisor protocol, mem7 auto-approve
- **Observability** — JSONL traces, OTEL export, session tracking, Prometheus metrics
- **Integrations** — [mem7](https://github.com/KTCrisis/flux7-memory) (decision persistence + auto-approve), [agent7](https://github.com/KTCrisis/flux7-console) (dashboard + governance UI)
- **Next** — durable state, daemon mode (`agent-mesh serve`), operator auth

## Get started

```bash
# Install
go install github.com/KTCrisis/flux7-mesh/cmd/agent-mesh@latest

# Add to Claude Code
claude mcp add agent-mesh -- agent-mesh --mcp --config config.yaml

# Or run standalone
agent-mesh --config config.yaml
```

MIT licensed. [github.com/KTCrisis/flux7-mesh](https://github.com/KTCrisis/flux7-mesh)
