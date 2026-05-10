# Agent SDK + mesh7

Govern an Anthropic Agent SDK agent with mesh7 YAML policies.

## How it works

`MeshHooks` generates `PreToolUse` hooks that call mesh7's `/decide`
endpoint before every tool execution. Policy evaluation, grants, rate
limits, tracing, and mem7 auto-approve all happen server-side.

```python
from mesh7 import MeshHooks

hooks = MeshHooks(agent="my-agent")
options = ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks())
```

## Action mapping

| mesh7 policy | Agent SDK | Effect |
|---|---|---|
| `allow` | `allow` | Tool executes |
| `deny` | `deny` | Tool blocked |
| `human_approval` | `ask` | User prompted |

## Run the example

```bash
mesh7 serve --config examples/agent-sdk-hooks/config.yaml
python examples/agent-sdk-hooks/agent.py
```

## When to use hooks vs MCP sidecar

| Feature | Hooks (`MeshHooks`) | MCP sidecar (`--mcp`) |
|---|---|---|
| Policy enforcement | Yes | Yes |
| Tracing | Yes (via `/decide`) | Yes |
| Terminal approval (`ask`) | Yes | Yes |
| Full approval workflow | No | Yes |
| Temporal grants | Yes (server-side) | Yes |
| Works without mesh7 | No (fail-closed) | No |

Use hooks when building with the Agent SDK. Use MCP sidecar for
Claude Code, Cursor, or Managed Agents.
