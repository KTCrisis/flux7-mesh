# mesh7 Python SDK

Governance mesh for AI agent tool calls. Wraps any Python function with policy enforcement, human approval, and tracing via [flux7-mesh](https://github.com/KTCrisis/flux7-mesh).

## Install

```bash
pip install flux7-mesh
pip install flux7-mesh[anthropic]  # with Claude API support
```

## Quick start

### Direct client

```python
from mesh7 import AgentMesh

mesh = AgentMesh("http://localhost:9090", agent="my-agent")

# Check what's available
tools = mesh.tools()
health = mesh.health()

# Policy check only (POST /decide) — no execution
decision = mesh.decide("filesystem.write_file", {"path": "/tmp/x"})
print(decision.action)  # allow | deny | human_approval

# Full proxy (POST /tool/{name}) — policy + execute + trace
decision = mesh.call_tool("filesystem.read_file", {"path": "/tmp/data.txt"})
print(decision.result)

# Manage grants
mesh.create_grant("filesystem.*", "30m")
mesh.revoke_grant("grant-id")
```

### GovernedToolkit (Claude API)

```python
from mesh7 import GovernedToolkit

toolkit = GovernedToolkit(agent="my-agent")

@toolkit.tool
def get_weather(city: str) -> str:
    """Get current weather for a city."""
    return fetch_weather(city)

@toolkit.tool
def send_email(to: str, subject: str, body: str) -> str:
    """Send an email."""
    return smtp_send(to, subject, body)

# Generate tools[] for Claude API — names are namespace-qualified
schemas = toolkit.schemas()
# [{"name": "my-agent__get_weather", ...}, {"name": "my-agent__send_email", ...}]

# Process tool_use blocks with governance
response = client.messages.create(model="claude-sonnet-4-6", tools=schemas, ...)
results = toolkit.process_response([b.model_dump() for b in response.content])
```

The toolkit:
1. **`schemas()`** — generates the `tools[]` array with namespace-qualified names (e.g. `my-agent__get_weather`)
2. **`process_response()`** — intercepts `tool_use` blocks, checks policy via mesh7, executes locally if allowed
3. **`execute()`** — single tool call with governance (accepts API-safe, qualified, and bare names)

Tool names are namespace-qualified with the agent name. **The Claude API rejects dots in tool names** (the schema is `^[a-zA-Z0-9_-]{1,128}$`), so `schemas()` emits the API-safe form `my-agent__get_weather` (double underscore). On the way back in, `execute()`/`process_response()` map it to the dotted form `my-agent.get_weather` that mesh7 evaluates — so policy authors still write one name format (`agent.tool`) regardless of call path, matching the MCP proxy `server.tool` convention. Use `namespace="custom"` to override the prefix.

> **Fixed in 0.4.1.** Versions ≤ 0.4.0 emitted dotted names from `schemas()`, which the Claude API rejected with `tools.0.custom.name: String should match pattern`. Upgrade if you call the Claude Messages API through `GovernedToolkit`.

### MeshHooks (Agent SDK)

```python
from mesh7 import MeshHooks

hooks = MeshHooks(agent="my-agent")

# Ready for Anthropic Agent SDK
options = ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks())
```

Every tool call goes through mesh7's `/decide` endpoint. Same YAML policies, same tracing, same grants and rate limits as Claude Code or any other mesh7-connected agent. No policy duplication.

| mesh7 action | Agent SDK | Effect |
|---|---|---|
| `allow` | `allow` | Tool executes |
| `deny` | `deny` | Tool blocked with reason |
| `human_approval` | `ask` | User prompted in terminal |

Options:

| Param | Default | Description |
|---|---|---|
| `tool_matcher` | `".*"` | Regex pre-filter (which tools hit mesh7) |
| `fail_action` | `"deny"` | Behavior when mesh7 is unreachable |

See `examples/agent-sdk-hooks/` for a complete example.

## Prerequisites

mesh7 daemon running:

```bash
mesh7 serve --config config.yaml
```

## API

### `AgentMesh(url, agent, timeout)`

| Method | Description |
|---|---|
| `decide(name, args)` | Evaluate policy without executing |
| `call_tool(name, args)` | Execute tool through governance (proxy) |
| `tools()` | List available tools |
| `approvals()` | List pending approvals |
| `approve(id)` / `deny(id)` | Resolve approval |
| `grants()` | List active grants |
| `create_grant(tools, duration)` | Create temporal grant |
| `health()` / `is_healthy()` | Check mesh status |

### `GovernedToolkit(agent, url, namespace)`

| Method | Description |
|---|---|
| `@toolkit.tool` | Decorator to register a function |
| `schemas()` | Generate Claude API tools array |
| `execute(name, input)` | Governed execution |
| `process_response(content)` | Process Claude response blocks |

### `MeshHooks(agent, url, tool_matcher, fail_action)`

| Method | Description |
|---|---|
| `agent_sdk_hooks()` | Return hooks dict for `ClaudeAgentOptions` |
