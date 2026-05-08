# agent-mesh Python SDK

Governance mesh for AI agent tool calls. Wraps any Python function with policy enforcement, human approval, and tracing via [agent-mesh](https://github.com/KTCrisis/flux7-mesh).

## Install

```bash
pip install flux7-mesh
pip install flux7-mesh[anthropic]  # with Claude API support
```

## Quick start

### Direct client

```python
from agent_mesh import AgentMesh

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
from agent_mesh import GovernedToolkit

toolkit = GovernedToolkit(agent="my-agent")

@toolkit.tool
def get_weather(city: str) -> str:
    """Get current weather for a city."""
    return fetch_weather(city)

@toolkit.tool
def send_email(to: str, subject: str, body: str) -> str:
    """Send an email."""
    return smtp_send(to, subject, body)

# Generate tools[] for Claude API
schemas = toolkit.schemas()

# Process tool_use blocks with governance
response = client.messages.create(model="claude-sonnet-4-6", tools=schemas, ...)
results = toolkit.process_response([b.model_dump() for b in response.content])
```

The toolkit:
1. **`schemas()`** — generates the `tools[]` array from Python function signatures
2. **`process_response()`** — intercepts `tool_use` blocks, checks policy via agent-mesh, executes locally if allowed
3. **`execute()`** — single tool call with governance

## Prerequisites

agent-mesh daemon running:

```bash
agent-mesh serve --config config.yaml
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

### `GovernedToolkit(agent, url)`

| Method | Description |
|---|---|
| `@toolkit.tool` | Decorator to register a function |
| `schemas()` | Generate Claude API tools array |
| `execute(name, input)` | Governed execution |
| `process_response(content)` | Process Claude response blocks |
