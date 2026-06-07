"""
Claude Agent SDK example with flux7-mesh governance.

Demonstrates:
  - Auto-discovery of tools from flux7-mesh
  - Agentic tool-use loop with Claude
  - human_approval policy (write requires approval)
  - X-Callback-URL for async approval notification

Usage:
  1. Start flux7-mesh:  mesh7 --config examples/agent-sdk/config.yaml
  2. Run this agent:    python examples/agent-sdk/agent.py "list files in /tmp/demo, read test.txt, then write a summary"

  In another terminal, approve pending requests:
    mesh --addr localhost:9092 watch

Requires: ANTHROPIC_API_KEY env var
"""

import os
import sys
import json
import requests
import anthropic

MESH_URL = os.environ.get("MESH_URL", "http://localhost:9092")
AGENT_ID = "claude-agent"


# --- Tool discovery from flux7-mesh ---

def discover_tools() -> list[dict]:
    """Fetch available tools from flux7-mesh and convert to Claude tool format."""
    r = requests.get(f"{MESH_URL}/tools", timeout=5)
    r.raise_for_status()
    mesh_tools = r.json()

    claude_tools = []
    for t in mesh_tools:
        tool_def = {
            "name": t["name"],
            "description": t.get("description", t["name"]),
            "input_schema": t.get("input_schema", {"type": "object", "properties": {}}),
        }
        claude_tools.append(tool_def)

    return claude_tools


# --- Tool execution through flux7-mesh ---

def call_tool(name: str, args: dict) -> str:
    """Call a tool through flux7-mesh HTTP API."""
    r = requests.post(
        f"{MESH_URL}/tool/{name}",
        json={"params": args},
        headers={
            "Authorization": f"Bearer agent:{AGENT_ID}",
            # Agent callback: flux7-mesh POSTs here when approval is resolved
            # (In a real agent, this would be a real endpoint)
            # "X-Callback-URL": "http://my-agent:8080/hook",
        },
        timeout=30,
    )
    data = r.json()

    if r.status_code == 403:
        return f"DENIED: {data.get('error', 'policy denied')}"
    if r.status_code == 202:
        approval_id = data.get("approval_id", "unknown")
        return (
            f"APPROVAL REQUIRED (id: {approval_id}). "
            f"Run: mesh --addr localhost:9092 approve {approval_id}"
        )
    if r.status_code == 408:
        return f"TIMEOUT: approval timed out"
    if "error" in data and data["error"]:
        return f"ERROR: {data['error']}"

    result = data.get("result", data)
    if isinstance(result, str):
        return result
    return json.dumps(result, indent=2)


# --- Agentic loop ---

SYSTEM = """You are a helpful assistant with access to filesystem tools through flux7-mesh.
Some operations (like writing files) may require human approval — if so, tell the user
and wait for them to approve before retrying. Read operations are always allowed."""


def run(query: str):
    client = anthropic.Anthropic()
    tools = discover_tools()

    print(f"\n--- Agent: {AGENT_ID} ---")
    print(f"--- Tools: {len(tools)} discovered from flux7-mesh ---")
    print(f"--- Query: {query} ---\n")

    messages = [{"role": "user", "content": query}]

    while True:
        response = client.messages.create(
            model="claude-sonnet-4-20250514",
            max_tokens=2048,
            system=SYSTEM,
            tools=tools,
            messages=messages,
        )

        # Collect assistant content
        assistant_content = []
        for block in response.content:
            assistant_content.append(block)
            if block.type == "text":
                print(f"Agent: {block.text}")

        messages.append({"role": "assistant", "content": assistant_content})

        if response.stop_reason == "end_turn":
            break

        if response.stop_reason == "tool_use":
            tool_results = []
            for block in response.content:
                if block.type == "tool_use":
                    print(f"  -> {block.name}({json.dumps(block.input, ensure_ascii=False)[:120]})")
                    result = call_tool(block.name, block.input)
                    print(f"  <- {result[:200]}")
                    tool_results.append({
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": result,
                    })

            messages.append({"role": "user", "content": tool_results})
        else:
            break


if __name__ == "__main__":
    query = (
        " ".join(sys.argv[1:])
        if len(sys.argv) > 1
        else "List files in /tmp/demo, read any .txt file, then write a summary.txt"
    )
    run(query)
