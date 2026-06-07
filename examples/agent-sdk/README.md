# Claude agent through flux7-mesh

Demonstrates Claude (Anthropic API) as an autonomous agent, governed by flux7-mesh with human approval on write operations.

## What it shows

| Operation | Policy | What happens |
|-----------|--------|-------------|
| `filesystem.list_directory` | allow | Passes through |
| `filesystem.read_file` | allow | Passes through |
| `filesystem.write_file` | **human_approval** | Blocks until approved via CLI |
| `filesystem.move_file` | **human_approval** | Blocks until approved via CLI |

This differs from the LangChain example (which shows deny). Here, writes are **not denied** — they require a human to approve, then execute normally.

## Setup

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# Create test data
mkdir -p /tmp/demo
echo "hello from flux7-mesh" > /tmp/demo/test.txt

# Set your API key
export ANTHROPIC_API_KEY=sk-ant-...
```

## Run

Terminal 1 — start flux7-mesh:

```bash
mesh7 --config examples/agent-sdk/config.yaml
```

Terminal 2 — watch for approvals:

```bash
mesh --addr localhost:9092 watch
```

Terminal 3 — run the agent:

```bash
source examples/agent-sdk/.venv/bin/activate
python examples/agent-sdk/agent.py "List files in /tmp/demo, read test.txt, then write a summary"
```

## Expected flow

```
Agent: I'll start by listing the files...
  -> filesystem.list_directory({"path": "/tmp/demo"})
  <- [FILE] test.txt                                        # allow

Agent: Now let me read the file...
  -> filesystem.read_file({"path": "/tmp/demo/test.txt"})
  <- hello from flux7-mesh                                  # allow

Agent: Let me write a summary...
  -> filesystem.write_file({"path": "/tmp/demo/summary.txt", ...})
  <- APPROVAL REQUIRED (id: a1b2c3d4)                      # human_approval
     Run: mesh --addr localhost:9092 approve a1b2c3d4

Agent: The write requires human approval. Please approve in your terminal.
```

In terminal 2 (`mesh watch`), you'll see the approval request and can approve it.

## How it works

1. **Tool discovery** — the agent calls `GET /tools` to get all available tools from flux7-mesh
2. **Agentic loop** — Claude decides which tools to call based on the user's query
3. **Policy enforcement** — flux7-mesh evaluates each tool call against the `claude-agent` policy
4. **Approval flow** — write operations return 202 (approval required), the agent tells the user
5. **Auth** — the agent identifies as `claude-agent` via `Authorization: Bearer agent:claude-agent`

## Callback webhook (advanced)

For autonomous agents that shouldn't block, uncomment the `X-Callback-URL` header in `agent.py`. Agent-mesh will POST the approval result to that URL when resolved, instead of requiring the agent to poll.

## Comparison with LangChain example

| | LangChain example | This example |
|---|---|---|
| **LLM** | Ollama (local) | Claude (API) |
| **Write policy** | deny | human_approval |
| **Demo** | Policy blocks bad actions | Human stays in the loop |
| **Agent awareness** | Sees error, moves on | Sees pending, asks user |
