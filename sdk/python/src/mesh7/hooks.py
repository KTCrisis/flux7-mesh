"""Anthropic Agent SDK hooks backed by mesh7 policy engine.

Generates PreToolUse hooks that delegate policy evaluation to mesh7's
/decide endpoint. Write your policies once in YAML, enforce them in
any Agent SDK agent with 3 lines of code.

Usage:
    from mesh7 import MeshHooks

    hooks = MeshHooks(agent="my-agent")
    options = ClaudeAgentOptions(hooks=hooks.agent_sdk_hooks())
"""

from __future__ import annotations

import asyncio
from typing import Any

from mesh7.client import Action, AgentMesh

_ACTION_MAP = {
    Action.ALLOW: "allow",
    Action.DENY: "deny",
    Action.HUMAN_APPROVAL: "ask",
}


class MeshHooks:
    """Generates Agent SDK hook configs from mesh7 policies.

    All policy evaluation happens server-side in mesh7 (grants, rate limits,
    mem7 auto-approve, injection detection). The hooks are thin delegates.
    """

    def __init__(
        self,
        agent: str = "default",
        url: str = "http://localhost:9090",
        mesh: AgentMesh | None = None,
        tool_matcher: str = ".*",
        fail_action: str = "deny",
    ) -> None:
        self._mesh = mesh or AgentMesh(url=url, agent=agent)
        self._tool_matcher = tool_matcher
        self._fail_action = fail_action

    def agent_sdk_hooks(self) -> dict[str, list[dict[str, Any]]]:
        """Return a hooks dict ready for ClaudeAgentOptions."""
        return {
            "PreToolUse": [
                {
                    "matcher": self._tool_matcher,
                    "hooks": [self._pre_hook],
                },
            ],
        }

    async def _pre_hook(
        self,
        input_data: dict[str, Any],
        tool_use_id: str | None,
        context: Any,
    ) -> dict[str, Any]:
        """PreToolUse callback — evaluates policy via mesh7 /decide."""
        tool_name = input_data.get("tool_name", "")
        tool_input = input_data.get("tool_input", {})

        try:
            decision = await asyncio.to_thread(
                self._mesh.decide, tool_name, tool_input
            )
        except Exception:
            return _permission(self._fail_action, "mesh7 unreachable — fail closed")

        perm = _ACTION_MAP.get(decision.action, self._fail_action)
        reason = decision.error if decision.action == Action.DENY else ""
        return _permission(perm, reason)


def _permission(decision: str, reason: str = "") -> dict[str, Any]:
    result: dict[str, Any] = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": decision,
        },
    }
    if reason:
        result["hookSpecificOutput"]["permissionDecisionReason"] = reason
    return result
