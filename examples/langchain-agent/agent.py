"""
LangChain agent that calls tools through flux7-mesh.

Demonstrates cross-agent governance: this agent (langchain-bot) has
read-only access to the filesystem, while Claude Code has full access.
Same tools, different policies.

Usage:
  1. Start flux7-mesh:  mesh7 --config examples/langchain-agent/config.yaml --port 9091
  2. Run this agent:    python examples/langchain-agent/agent.py

Requires: Ollama running (local or cloud) with env vars set
"""

import os
import sys
import json
import requests

from langchain_ollama import ChatOllama
from langchain_core.tools import tool
from langgraph.prebuilt import create_react_agent

MESH_URL = "http://localhost:9091"
AGENT_ID = "langchain-bot"


def call_mesh(tool_name: str, params: dict) -> str:
    """Call a tool through flux7-mesh HTTP API."""
    try:
        r = requests.post(
            f"{MESH_URL}/tool/{tool_name}",
            json={"params": params},
            headers={"Authorization": f"Bearer agent:{AGENT_ID}"},
            timeout=10,
        )
        return json.dumps(r.json(), indent=2)
    except requests.ConnectionError:
        return "Error: flux7-mesh is not running on port 9091"


@tool
def list_directory(path: str) -> str:
    """List files in a directory."""
    return call_mesh("filesystem.list_directory", {"path": path})


@tool
def read_file(path: str) -> str:
    """Read a file."""
    return call_mesh("filesystem.read_file", {"path": path})


@tool
def write_file(path: str, content: str) -> str:
    """Write content to a file. This will be DENIED by flux7-mesh policy."""
    return call_mesh("filesystem.write_file", {"path": path, "content": content})


llm = ChatOllama(
    base_url=os.environ.get("OLLAMA_HOST", "http://localhost:11434"),
    model=os.environ.get("OLLAMA_MODEL", "qwen2.5"),
    temperature=0,
)

agent = create_react_agent(llm, [list_directory, read_file, write_file])

if __name__ == "__main__":
    query = " ".join(sys.argv[1:]) if len(sys.argv) > 1 else "List the files in /tmp/demo and read any file you find"
    print(f"\n--- Agent: {AGENT_ID} ---")
    print(f"--- Query: {query} ---\n")

    for chunk in agent.stream({"messages": [("user", query)]}):
        if "agent" in chunk:
            for msg in chunk["agent"]["messages"]:
                if hasattr(msg, "content") and msg.content:
                    print(f"Agent: {msg.content}")
                if hasattr(msg, "tool_calls") and msg.tool_calls:
                    for tc in msg.tool_calls:
                        print(f"  -> calling {tc['name']}({tc['args']})")
        if "tools" in chunk:
            for msg in chunk["tools"]["messages"]:
                print(f"  <- {msg.content[:200]}")
