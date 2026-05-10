"""
Agent SDK agent with mesh7 governance via hooks.

mesh7 enforces YAML policies on every tool call — the agent doesn't
need to know about governance. Same policies work in Claude Code,
Cursor, LangChain, or any other mesh7-connected agent.

Usage:
    1. mesh7 serve --config examples/agent-sdk-hooks/config.yaml
    2. python examples/agent-sdk-hooks/agent.py
"""

from mesh7 import MeshHooks

hooks = MeshHooks(agent="my-agent", url="http://localhost:9092")

# Pass to your Agent SDK setup:
#
#   from claude_agent_sdk import Agent, ClaudeAgentOptions
#
#   agent = Agent(
#       name="my-agent",
#       model="claude-sonnet-4-20250514",
#       tools=[...],
#       options=ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks()),
#   )

# Preview the hook config
if __name__ == "__main__":
    import json

    cfg = hooks.agent_sdk_hooks()
    print("Hook config for Agent SDK:")
    print(f"  PreToolUse matchers: {len(cfg['PreToolUse'])}")
    print(f"  Tool matcher regex: {cfg['PreToolUse'][0]['matcher']}")
    print(f"  Callback: {cfg['PreToolUse'][0]['hooks'][0].__name__}")
    print()
    print("Integration code:")
    print('  hooks = MeshHooks(agent="my-agent")')
    print("  options = ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks())")
