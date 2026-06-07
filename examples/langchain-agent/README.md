# LangChain agent through flux7-mesh

Demonstrates cross-agent governance: a LangChain agent (`langchain-bot`) has read-only access to the filesystem, while Claude Code has full access. Same tools, different policies.

## What it shows

| Operation | Claude Code | LangChain bot |
|-----------|-------------|---------------|
| `filesystem.list_directory` | allow | allow |
| `filesystem.read_file` | allow | allow |
| `filesystem.write_file` | allow | **deny** |
| `filesystem.move_file` | allow | **deny** |

This is the core flux7-mesh value prop: **same tools, per-agent policies, zero code change on the agent side**.

## Setup

```bash
# Create a venv (don't pollute your global env)
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# Create test data
mkdir -p /tmp/demo
echo "hello from flux7-mesh" > /tmp/demo/test.txt
```

## Run

Terminal 1 — start flux7-mesh:

```bash
cd /path/to/flux7-mesh
mesh7 --config examples/langchain-agent/config.yaml --port 9091
```

Terminal 2 — run the agent:

```bash
source examples/langchain-agent/.venv/bin/activate

# With local Ollama
python examples/langchain-agent/agent.py "List files in /tmp/demo, read test.txt, then try to write a new file"

# With remote Ollama-compatible API (e.g. Kimi K2.5)
export OLLAMA_HOST=https://ollama.com
export OLLAMA_MODEL=kimi-k2.5:cloud
export OLLAMA_API_KEY=your-key-here
python examples/langchain-agent/agent.py "List files in /tmp/demo, read test.txt, then try to write a new file"
```

## Expected output

```
list_directory(/tmp/demo)  → allow  → [FILE] test.txt
read_file(/tmp/demo/test.txt) → allow  → "hello from flux7-mesh"
write_file(/tmp/demo/new_file.txt) → deny   → policy=langchain-bot action=deny
```

Read passes, write gets denied. Check the flux7-mesh terminal for the policy logs.

## How it works

The agent authenticates via `Authorization: Bearer agent:langchain-bot`. Agent-mesh matches this against the `langchain-bot` policy in `config.yaml`, which only allows read operations. Write calls hit the deny rule before reaching the filesystem.

No changes needed on the LangChain side — it just sees a tool that returned an error. The governance is invisible to the agent.
