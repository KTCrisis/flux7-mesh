"""PreToolUse hook for the Claude Code CLI, backed by the mesh7 policy engine.

The proxy path governs what an agent routes *through* mesh7. It cannot reach
the tools the harness executes itself — Bash, Read, Write, Edit, WebFetch. This
hook closes that gap: the harness asks mesh7 before running its own tools, so a
single policy file governs both surfaces.

Install by pointing a ``PreToolUse`` command hook at ``mesh7-hook`` in
``~/.claude/settings.json``. Read ``docs/harness-hook.md`` for the full recipe,
the staged rollout and the policy starter set.

Configuration is by environment variable:

===========================  ==========================  ===========================
Variable                     Default                     Meaning
===========================  ==========================  ===========================
``MESH7_HOOK_MODE``          ``observe``                 ``observe`` traces only,
                                                         ``enforce`` applies denials
``MESH7_URL``                ``http://localhost:9090``   mesh7 data plane
``MESH7_AGENT``              ``claude``                  agent identity to evaluate
``MESH7_TOKEN``              *(unset)*                   JWT presented instead of
                                                         the self-declared agent
``MESH7_HOOK_TIMEOUT``       ``5``                       seconds before fail-closed
``MESH7_HOOK_SKIP_PREFIX``   ``mcp__``                   tool prefixes left to the
                                                         proxy, comma-separated
===========================  ==========================  ===========================
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

from mesh7.client import Action, AgentMesh

# Claude Code accepts three verdicts. "ask" hands the call back to the harness's
# own permission prompt rather than to the mesh7 approval queue — the operator is
# already at the keyboard, so the shorter loop is the right one.
_ACTION_MAP = {
    Action.ALLOW: "allow",
    Action.DENY: "deny",
    Action.HUMAN_APPROVAL: "ask",
}

_OBSERVE = "observe"
_ENFORCE = "enforce"


def _permission(decision: str, reason: str = "") -> dict[str, Any]:
    out: dict[str, Any] = {
        "hookSpecificOutput": {
            "hookEventName": "PreToolUse",
            "permissionDecision": decision,
        },
    }
    if reason:
        out["hookSpecificOutput"]["permissionDecisionReason"] = reason
    return out


def _skip_prefixes() -> tuple[str, ...]:
    raw = os.environ.get("MESH7_HOOK_SKIP_PREFIX", "mcp__")
    return tuple(p.strip() for p in raw.split(",") if p.strip())


def decide(
    payload: dict[str, Any], mesh: AgentMesh | None = None
) -> dict[str, Any] | None:
    """Evaluate one PreToolUse payload.

    Returns the hook output to print, or ``None`` to stay silent and let the
    harness apply its normal permission flow.
    """
    mode = os.environ.get("MESH7_HOOK_MODE", _OBSERVE).strip().lower()
    tool_name = payload.get("tool_name", "")
    tool_input = payload.get("tool_input", {})

    if not tool_name:
        return None

    # MCP tools already cross the proxy, which evaluates them on their real
    # name. The harness mangles that name (dots become underscores under an
    # `mcp__<server>__` prefix) and the reverse mapping is ambiguous, so
    # re-deciding here would double-evaluate on a name we cannot trust.
    if tool_name.startswith(_skip_prefixes()):
        return None

    mesh = mesh or AgentMesh(
        url=os.environ.get("MESH7_URL", "http://localhost:9090"),
        agent=os.environ.get("MESH7_AGENT", "claude"),
        timeout=int(os.environ.get("MESH7_HOOK_TIMEOUT", "5")),
        token=os.environ.get("MESH7_TOKEN") or None,
    )

    try:
        # The harness names its session in every payload; passing it on is
        # what lets the console group these calls with the rest of the session.
        decision = mesh.decide(tool_name, tool_input, session_id=payload.get("session_id") or None)
    except Exception as exc:  # noqa: BLE001 — any failure is a failure to govern
        if mode == _ENFORCE:
            return _permission("deny", f"mesh7 unreachable — fail closed ({exc})")
        return None

    # Observe mode never widens anything: it writes the trace and stays silent,
    # so a policy that would have said "allow" does not suppress the prompt the
    # operator would otherwise have seen.
    if mode != _ENFORCE:
        return None

    # A malformed or unauthenticated answer is not a decision. Fail closed.
    if decision.action == Action.ERROR:
        return _permission("deny", "mesh7 returned no decision — fail closed")

    verdict = _ACTION_MAP.get(decision.action, "deny")
    reason = decision.error if decision.action == Action.DENY else ""
    return _permission(verdict, reason)


def main() -> int:
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        # A payload we cannot parse is a payload we cannot govern, but denying
        # on it would wedge the session on a harness change. Stay silent and let
        # the harness decide; the operator sees nothing break.
        return 0

    if not isinstance(payload, dict):
        return 0

    out = decide(payload)
    if out is not None:
        json.dump(out, sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
