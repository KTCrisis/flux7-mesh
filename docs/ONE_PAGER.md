# flux7-mesh — Governance Mesh for AI Agents

One Go binary between your agents and their tools. Policy, approval, and a causal audit trail, for MCP servers, OpenAPI backends and CLI binaries alike.

## The problem

You are deploying agents. They call tools: file writes, emails, API calls, database queries. Three questions have to be answered before production, and no agent framework answers them.

- **Which agent can call which tool?** Frameworks do not enforce boundaries. An agent can call anything it discovers.
- **Who approved that action, and why?** Someone clicked "yes" in a terminal three weeks ago. That decision is gone, and so is its reason.
- **What actually happened?** Stdout logs, somewhere. Not structured, not queryable, not auditable.

These are not framework problems, they are infrastructure problems. Service meshes solved the equivalent for microservices a decade ago: policy enforcement, observability and access control at the network layer. Agents need the same thing one layer up, at the tool call.

## Where it sits

```
Agent (Claude Code, Agent SDK, LangChain, script, cron)
  │
  └──► mesh7
         ├── identity     JWT (JWKS) or per-agent bearer
         ├── rate limit   sliding window + budget + loop detection
         ├── policy       allow / deny / human_approval, fail-closed
         ├── grants       temporal sudo, bypasses approval only
         ├── approval     async queue, survives restarts
         └── trace        JSONL + OTEL, with a walkable causal chain
                │
                └──► MCP servers · OpenAPI backends · CLI binaries
```

The agent does not know the proxy is there. It calls a tool and gets a result. The governance layer is invisible to the agent and visible to the operator.

**In:** MCP over stdio, SSE and Streamable HTTP · OpenAPI specs (URL or file) · CLI binaries
**Out:** MCP stdio (Claude Code, Cursor) · MCP Streamable HTTP at `POST /mcp` (Anthropic Managed Agents) · HTTP REST at `POST /tool/{name}` · policy-only evaluation at `POST /decide`

## What makes it different

Three things, and only three. The rest is table stakes, well executed.

**1. The enforcement point is protocol-agnostic.** A policy attaches to a tool's identity, not to the transport carrying it. The same rule governs a stdio MCP server, an imported OpenAPI operation and a local binary, in one catalog. Change your agent framework or your tool protocol and the rule survives both.

**2. The audit trail is causal, not chronological.** A trace carries `parent_trace_id` and `grant_id`. `GET /traces/{id}/why` walks the chain back, so the answer is not "allowed by rule X" but "allowed by grant G, which came from approval A, which came from call T". Most systems record what happened. This one records why it was permitted, back to the human decision.

**3. Enforcement is decoupled from proxying.** `POST /decide` evaluates policy without executing anything. A PreToolUse hook asks "would you allow this?" and enforces locally, which means mesh7 governs tools that never transit it, including a harness's built-in tools. That is the difference between a proxy and a policy authority.

| | API gateways (Kong, Apigee) | Agent frameworks (LangChain, CrewAI) | flux7-mesh |
|---|---|---|---|
| **Axis** | North-south (user → LLM) | Inside one runtime | East-west (agent → tools) |
| **Tool surface** | HTTP only | Whatever the framework wraps | MCP + OpenAPI + CLI, one catalog |
| **Policy** | API keys, quotas | None, or coarse allow/ask | Per agent, per tool, with conditions on arguments |
| **Approval** | None | Framework-specific prompt | Async queue, persisted, with memory |
| **Identity** | API consumer | The process itself | Per agent, JWT-validated |
| **Audit** | Access logs | None | Causal chain, queryable |
| **Deployment** | Cluster infrastructure | Embedded in your code | One binary, one YAML |

Closest comparable: Microsoft Agent Governance Toolkit, but middleware rather than sidecar. mesh7 requires no change to agent code.

## Why a single Go binary matters

This is an operational argument, not an aesthetic one.

- **No runtime to install.** A static binary, four platforms via goreleaser. No interpreter, no virtualenv, no version drift between the box that governs and the box that runs.
- **Zero CGO.** The only dependencies are `gopkg.in/yaml.v3` and `modernc.org/sqlite`, a pure-Go SQLite. Cross-compilation stays trivial and the container is a scratch image with one file in it.
- **It can run where the agent runs.** A sidecar has to be cheap enough to put next to every workload. A JVM or a Python service with a dependency tree is not; a few megabytes of static ELF is.
- **The governance plane must not be the fragile one.** A policy engine that fails to start because of a transitive dependency conflict fails open in practice, since the operator disables it. Fewer moving parts is a security property.
- **Concurrency is in the language.** Blocking approvals, upstream MCP subprocesses, the trace writer and the policy watcher are goroutines with explicit locking, not an event loop shared with user code.

## One daemon, every agent

Early on, mesh7 was launched *by* its client: Claude Code spawned it as a stdio subprocess, and that subprocess also bound the control-plane port. Identity was tied to the process, so it was one mesh per agent. Two clients meant two instances, a port conflict, and worse, two trace stores, two approval queues and two pools of upstream MCP servers.

Identity moved into the request header, and the topology collapsed to one process.

```
                  mesh7 serve  (persistent, owns :9090, SQLite, upstream pool)
                  ┌──────────────────────────────────────────┐
Claude A ──stdio──┤                                          │
Claude B ──stdio──┤  registry · policy · approval · grants   ├──► tools
Agent C  ───HTTP──┤  traces · rate limiting · sessions       │
Managed  ───/mcp──┤                                          │
                  └────────────────────┬─────────────────────┘
                                supervisor (polls one queue)
```

`mesh7 --mcp` now probes `/health` before any initialisation. If a daemon answers, the process becomes a 96-line stdio-to-HTTP shuttle instead of a second mesh; if nothing answers, it falls through to the embedded behaviour. The same client configuration works either way, with no flag to flip.

The shuttle carries `Mcp-Session-Id` in both directions and releases it on exit, so sessions are first-class server-side objects. It sets `Authorization` per connection, a validated JWT when `MESH_AGENT_TOKEN` is present and the per-agent bearer otherwise, so the daemon sees distinct identities on a shared port and the specificity sort gives each one its own rules.

What that buys: one causal chain that crosses agents, one approval queue for the supervisor to poll, fleet-wide rate budgets, upstream MCP servers started once rather than once per session, and approvals and grants in SQLite that outlive the client that raised them. Governance that dies with the agent is not governance.

## Adaptive governance

Policies start strict. The system then learns which requests are routine.

| Level | Who | Latency | Handles |
|-------|-----|---------|---------|
| 0 | Policy engine | 0 ms | Static rules, conditions on arguments |
| 1 | Built-in mem7 lookup | ~100 ms | Routine reads (3+ human approvals, no refusal); writes always ask |
| 1+ | External supervisor | ~350 ms | Novel cases and writes, rules then a decision model |
| 2 | Human | minutes | Unknowns and high-stakes calls |

Auto-approval is skipped whenever the arguments carry a prompt-injection pattern, so the escalation is conservative in the right direction: the tripwire blocks the automatic decision, not the call. Every decision is stored as a fact in [mem7](https://github.com/KTCrisis/flux7-memory) and every call is a trace. Both are queryable.

## Hardening

- **Fail-closed under partial information.** No matching rule denies. A condition that cannot be evaluated is false in both directions, so an `allow` that cannot be checked does not allow, and a `deny` that cannot be checked still lands on the default deny.
- **CLI execution never touches a shell.** `exec.Command` directly, shell metacharacters rejected per argument, flag allowlist, context timeout, 1 MB output cap, and an environment rebuilt from scratch so a wrapped binary never inherits the mesh's secrets.
- **SSRF checked at dial time.** The host is resolved on the actual connection, every resolved address is inspected, and the vetted IP is dialled directly. No TOCTOU window for DNS rebinding, and cloud metadata endpoints are blocked.
- **Data plane and control plane are separate.** Tool calls, `/decide`, `/mcp` and `/health` are never admin-gated. Traces, approvals, grants, policies and metrics require a bearer token, or are restricted to loopback when none is set, compared in constant time.
- **Loop detection.** The same tool with the same arguments three times in ten seconds is refused. An agent stuck in a retry loop is an agent-specific failure mode that no API gateway models.

## Current state (August 2026, v0.15.1)

- 374 Go test functions across 17 packages, race clean, plus the Python SDK suite; CI on every push
- Identity: JWT validated against JWKS with issuer and audience checks; the plaintext agent header is opt-in, off by default
- Import: MCP over stdio, SSE and Streamable HTTP; OpenAPI; CLI binaries with a tightening-only dispatch floor
- Governance: per-agent policy files, specificity sort, hot reload, numeric and string conditions on arguments
- Approval: routing via `queue | tty | tty-fallback`, temporal grants recording their origin, SQLite persistence
- Observability: JSONL traces with rotation, OTEL export, sessions, Prometheus metrics, token accounting that labels real counts and estimates differently
- SDK: `pip install flux7-mesh`, `GovernedToolkit` for the Claude API, `MeshHooks` for the Agent SDK, `mesh7-hook` as a Claude Code PreToolUse hook
- Ecosystem: [mem7](https://github.com/KTCrisis/flux7-memory) for decision persistence, [flux7-console](https://github.com/KTCrisis/flux7-console) for the dashboard, [flux7-supervisor](https://github.com/KTCrisis/flux7-supervisor) for L1 evaluation

## Known limits

Stated plainly, because a governance tool that oversells itself is worse than none.

- Policy conditions match text, case-sensitively. Denying `rm -rf` does not stop `RM -RF` or a base64 payload. Semantic conditions are the open work item.
- Injection detection is a regex tripwire wired to auto-approval, not a defence.
- CLI positional arguments are metacharacter-filtered but not allowlisted; the allowlist governs flags.
- Rate-limit counters live in memory and reset with the daemon. Approvals and grants do not.
- The `--mcp` auto-proxy probes localhost, so it is a same-host mechanism.

## Get started

```bash
go install github.com/KTCrisis/flux7-mesh/cmd/mesh7@latest

# Daemon, shared by every agent on the host
mesh7 serve --config config.yaml

# Claude Code attaches to it (or spawns its own if no daemon is up)
claude mcp add mesh7 -- mesh7 --mcp --config config.yaml
```

Apache 2.0. [github.com/KTCrisis/flux7-mesh](https://github.com/KTCrisis/flux7-mesh)
