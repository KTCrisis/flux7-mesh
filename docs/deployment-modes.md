# Deployment Modes

flux7-mesh supports different configurations depending on who connects and how.

## Configuration matrix

| # | Setup | Transport | Who launches mesh | Supervisor | Status |
|---|-------|-----------|-------------------|------------|--------|
| **1** | Solo dev + Claude/Cursor | MCP stdio | Claude spawns it | None | Works |
| **2** | Solo dev + Claude + supervisor | MCP stdio + HTTP | Claude spawns it | Passive (poll :9090) | Works while Claude runs |
| **3** | Supervisor standalone (no Claude) | HTTP | Supervisor spawns it | Active | Works |
| **4** | External agent (LangChain, script) | HTTP | Manual or supervisor | Optional | Works |
| **5** | Claude + external agent | MCP stdio + HTTP | Claude spawns it | Optional | Works |
| **6** | Claude + supervisor (active spawn) | MCP stdio + HTTP | Both try to spawn | Active | Needs `mesh7 serve` |
| **7** | 2 Claude sessions | MCP stdio × 2 | First spawns, second proxies | - | Works (v0.9.4+) |
| **8** | Managed Agents (cloud) | MCP Streamable HTTP | Manual / deploy | Optional | Works |
| **9** | Agent SDK + hooks | HTTP `/decide` | Manual (`mesh7 serve`) | None | Works |

Configs 1–5 and 8–9 work out of the box. Config 7 resolves itself through the `--mcp` auto-proxy (v0.9.4+). Config 6 needs daemon mode (`mesh7 serve`), because there the conflict is over who owns the process lifecycle, not just the port.

---

## Config 1: Solo dev + Claude (most common)

The default. 90% of users. Zero setup.

```
Claude Code ──stdio──> flux7-mesh ──> filesystem, gmail, ollama...
                           │
                      :9090 HTTP (background, for mesh CLI / traces)
```

Claude launches mesh7 as an MCP subprocess. mesh7 launches upstream MCP servers, applies policies, records traces. When Claude quits, everything stops cleanly.

**Setup:** Just add flux7-mesh as an MCP server in Claude Code:

```bash
claude mcp add mesh7 -- mesh7 --mcp --config config.yaml
```

## Config 2: Solo dev + Claude + supervisor (passive)

Claude manages flux7-mesh. Two layers of auto-resolve handle routine approvals before they reach a human.

```
Claude Code ──stdio──> flux7-mesh :9090 ──> tools
                           │
                    Level 1: built-in (mem7 lookup, ~100ms)
                    ├── 3+ past approvals → auto-approve
                    └── else → escalate to Level 1+
                           │
                    Level 1+: supervisor (poll GET /approvals)
                    ├── rules → approve/deny (0ms)
                    └── ollama → evaluate (~20s)
```

The built-in auto-approve (Level 1) fires before the approval queue — routine patterns never block. If it can't resolve, the external supervisor (Level 1+) evaluates with rules and LLM. If both escalate, the human decides.

**Setup:**

```bash
# Terminal 1: Claude (launches flux7-mesh automatically)
claude

# Terminal 2: Supervisor (flux7-supervisor / sup7)
cd ~/flux7-supervisor
python -m sup7 --config supervisor.local.yaml
```

With `supervisor.enabled: true` in the flux7-mesh config, `approval.resolve` and `approval.pending` tools are hidden from Claude. Tool calls block until the supervisor resolves them.

**Supervisor config:**

```yaml
supervisor:
  mesh_url: http://localhost:9090
  mesh_process:
    enabled: false    # Claude manages the lifecycle
  ollama:
    enabled: true
    model: qwen3:14b
  rules:
    - name: project-scope
      condition: "params.path starts_with /home/user"
      action: approve
      confidence: 0.95
```

**Limitation:** When Claude quits, flux7-mesh dies. The supervisor retries every `poll_interval` until Claude starts again.

## Config 3: Supervisor standalone (no Claude)

For pipelines, overnight runs, CI/CD, batch jobs. No human in the loop — the supervisor manages everything.

```
supervisor (always alive)
  │
  ├── spawn/restart ──> flux7-mesh :9090 ──> tools
  │
  ├── poll → evaluate → resolve
  └── store decisions in memory-mcp
```

**Setup:**

```bash
cd ~/flux7-supervisor
python -m sup7 --config supervisor.yaml
```

With `mesh_process.enabled: true`, the supervisor spawns flux7-mesh on startup, monitors health, and restarts it on crash.

```yaml
supervisor:
  mesh_url: http://localhost:9090
  mesh_process:
    enabled: true
    command: mesh7
    config: /path/to/config.yaml
```

External agents connect via HTTP:

```bash
curl -X POST http://localhost:9090/tool/filesystem.write_file \
  -H "Authorization: Bearer agent:my-script" \
  -d '{"params":{"path":"/tmp/output.txt","content":"hello"}}'
```

## Config 4: External agent only

Standard HTTP proxy mode. No MCP, no supervisor.

```
flux7-mesh :9090 ──> tools
     │
Agent (HTTP) ──────┘
```

**Setup:**

```bash
mesh7 --config config.yaml
```

Any HTTP client can call `POST /tool/{name}`, query traces, manage approvals. Works with LangChain, CrewAI, custom scripts, cron jobs.

## Config 5: Claude + external agent (the real mesh)

Claude and external agents share the same flux7-mesh instance. One set of policies, one trace store, one approval queue.

```
Claude Code ──stdio──> flux7-mesh :9090 ──> tools
                           │
Agent B ─────────HTTP──────┘
```

**This works today.** Claude spawns flux7-mesh, the external agent connects via HTTP to `:9090`. Both are governed by the same policies.

Add a supervisor and you get Config 2 with extra agents — everything goes through one mesh.

---

## Config 8: Anthropic Managed Agents (cloud, MCP Streamable HTTP)

Cloud-hosted agents connect to your flux7-mesh over the internet via MCP Streamable HTTP.

```
Anthropic cloud
├── Managed Agent (coordinator)
│   ├── sub-agent: reviewer
│   ├── sub-agent: tester
│   └── all use mcp_toolset "mesh"
│
└── MCP connector ── POST /mcp ──> flux7-mesh (your server, public URL)
                                       │
                                  policies, traces, mem7
                                       │
                                  upstream MCP servers
```

**Setup:**

```python
# Anthropic SDK (Python)
agent = client.beta.agents.create(
    name="Governed Assistant",
    model="claude-opus-4-7",
    mcp_servers=[{
        "type": "url",
        "name": "mesh",
        "url": "https://mesh.example.com/mcp",
    }],
    tools=[
        {"type": "agent_toolset_20260401"},
        {"type": "mcp_toolset", "mcp_server_name": "mesh",
         "default_config": {"permission_policy": {"type": "always_allow"}}},
    ],
)
```

Auth via vault (static bearer):

```python
vault = client.beta.vaults.create(display_name="mesh-credentials")
client.beta.vaults.credentials.create(
    vault_id=vault.id,
    display_name="flux7-mesh token",
    auth={
        "type": "static_bearer",
        "mcp_server_url": "https://mesh.example.com/mcp",
        "token": "agent:my-managed-agent",
    },
)
```

flux7-mesh extracts the agent ID from `Authorization: Bearer agent:<id>` and applies per-agent policies.

**Networking:** flux7-mesh must be accessible from Anthropic's cloud. Options:
- Dev: Tailscale funnel or ngrok → `localhost:9090`
- Prod: deploy flux7-mesh on a VPS or cloud host

**Permission policies:** Set `always_allow` on the Managed Agent side — let flux7-mesh handle governance. Double-layer approval (Managed Agents `always_ask` + flux7-mesh `human_approval`) works but adds friction.

## Config 9: Anthropic Agent SDK + hooks

Agent SDK agents governed by mesh7 via `MeshHooks`. No MCP — hooks call `/decide` directly.

```
Agent SDK agent
├── PreToolUse hook ── POST /decide ──> flux7-mesh :9090
│                                          │
│                                     policies, traces, grants
│                                          │
└── tool execution (local) ───────────────┘
```

**Setup:**

```python
from mesh7 import MeshHooks

hooks = MeshHooks(agent="my-agent")
agent = Agent(
    model="claude-sonnet-4-6",
    tools=[...],
    options=ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks()),
)
```

Same YAML policies as every other mode. `human_approval` maps to Agent SDK's `"ask"` (terminal prompt). For the full approval workflow (webhooks, CLI, auto-approve), use Config 1 or 8 instead.

See `examples/agent-sdk-hooks/` and `sdk/python/README.md`.

---

## Configs that need daemon mode

### Config 6: Claude + supervisor (active spawn)

Both Claude and the supervisor try to spawn flux7-mesh on port `:9090`.

```
Claude ──stdio──> flux7-mesh :9090     ← process A
supervisor ──spawn──> flux7-mesh :9090 ← process B  💥 bind: address already in use
```

The second instance crashes with exit code 1. The supervisor restart loop detects the crash and spawns again, which is an infinite crash loop.

**Fix:** run `mesh7 serve` as the daemon and set `mesh_process.enabled: false` in the supervisor config, so neither Claude nor the supervisor owns the lifecycle. Claude's `--mcp` auto-proxies to the daemon and the supervisor polls it. Without a daemon, `mesh_process.enabled: false` alone still works (→ Config 2), at the cost of the mesh dying with Claude.

### Config 7: Two Claude sessions

Historically, two Claude Code sessions sharing an MCP config both spawned flux7-mesh, the second instance silently lost the bind on `:9090`, and traces and approvals ended up split across two isolated processes.

Since v0.9.4 this resolves itself. `mesh7 --mcp` probes `GET /health` on the configured port before initialising anything. If something answers, the process becomes a stdio-to-HTTP shuttle to that instance instead of standing up a second mesh.

```
Claude session 1 ──stdio──> flux7-mesh :9090  ← owns the mesh
Claude session 2 ──stdio──> shuttle ──POST /mcp──> :9090  ← same instance
```

Note that this works even with no `mesh7 serve` daemon: an embedded MCP-mode instance wires the Streamable HTTP handler too, so session 1's mesh already serves `POST /mcp` for session 2 to attach to. One registry, one trace store, one approval queue.

Two things to know:

- **Lifetime still belongs to session 1.** Quit it and session 2's shuttle reports `proxy: daemon unreachable`. Run `mesh7 serve` when you want the mesh to outlive every client.
- **Both sessions default to the same identity.** `--mcp-agent` defaults to `claude`, so unless you give each session a distinct value, policies and traces cannot tell them apart.

**Detection:**

```bash
lsof -i :9090     # who owns the port
curl -s localhost:9090/health | jq .tools
```

---

## Decision flow: which config to use

```
Do you use Claude/Cursor?
  │
  ├─ Yes, just Claude ──────────────────────────→ Config 1 (embedded)
  │
  ├─ Yes, Claude + auto-approve ────────────────→ Config 2 (passive supervisor)
  │
  ├─ Yes, Claude + external agents ─────────────→ Config 5 (shared mesh)
  │
  └─ No
       │
       ├─ Want auto-approve / overnight runs ───→ Config 3 (supervisor standalone)
       │
       ├─ Just HTTP proxy ─────────────────────→ Config 4 (standalone)
       │
       └─ Anthropic Managed Agents (cloud) ─→ Config 8 (MCP Streamable HTTP)
```

## Component lifecycle

| Component | Who starts it | Who stops it | Persists across sessions |
|-----------|--------------|-------------|------------------------|
| **Ollama** | System daemon | System | Yes |
| **mesh7** | Claude (config 1/2/5), supervisor (config 3), or `mesh7 serve` (daemon) | Dies with parent (embedded) or persistent (daemon) | Yes with daemon mode |
| **Upstream MCP servers** | flux7-mesh (subprocesses) | Die with flux7-mesh | No |
| **Supervisor** | User (terminal) | User (Ctrl+C) | Yes (as long as terminal lives) |
| **Claude Code** | User | User | No |

## Daemon mode (v0.9.4+)

A single persistent mesh7 instance shared by everyone:

```
                    mesh7 serve (daemon, persistent)
                    ┌─────────────────────────────────────┐
Claude ──connect──> │                                     │──> tools
Agent B ───HTTP───> │  registry · policy · approval       │
Agent C ───HTTP───> │  trace · grants · rate limiting     │
                    └──────────────┬──────────────────────┘
                                   │
                            supervisor (poll)
```

Two subcommands:

- **`mesh7 serve`** — run as a persistent daemon (HTTP + manages upstream MCP servers)
- **`mesh7 --mcp`** — auto-detects a running daemon and proxies to it (MCP stdio for Claude Code)

This solves Config 6 (competing spawns) and Config 2's limitation (the mesh dies with Claude), and it makes Config 7 durable rather than merely working. Every client uses the auto-proxy instead of spawning its own mesh7, and the daemon outlives all of them.

| Feature | Status |
|---------|--------|
| Config 1: Embedded MCP | Done |
| Config 2: Passive supervisor | Done |
| Config 3: Active supervisor | Done |
| Config 4: Standalone HTTP | Done |
| Config 5: Shared mesh | Done |
| Config 8: Managed Agents (MCP Streamable HTTP) | Done (v0.9.0) |
| `supervisor.enabled` (hide approval tools) | Done |
| `mesh7 serve` (daemon) | Done (v0.9.4) |
| `mesh7 --mcp` (auto-proxy to daemon) | Done (v0.9.4) |
