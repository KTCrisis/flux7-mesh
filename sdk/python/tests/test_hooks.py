"""Tests for the MeshHooks Agent SDK integration."""
from __future__ import annotations

import asyncio
from unittest.mock import MagicMock, patch

import pytest

from mesh7 import MeshHooks
from mesh7.client import Action, AgentMesh, Decision


@pytest.fixture
def mesh():
    return MagicMock(spec=AgentMesh)


@pytest.fixture
def hooks(mesh):
    return MeshHooks(agent="test", mesh=mesh)


def _input(tool: str = "filesystem.write_file", params: dict | None = None):
    return {
        "hook_event_name": "PreToolUse",
        "tool_name": tool,
        "tool_input": params or {"path": "/tmp/test"},
        "session_id": "sess-1",
        "cwd": "/home/test",
    }


def _run(coro):
    return asyncio.get_event_loop().run_until_complete(coro)


class TestAgentSdkHooks:
    def test_returns_hook_config(self, hooks):
        cfg = hooks.agent_sdk_hooks()
        assert "PreToolUse" in cfg
        assert len(cfg["PreToolUse"]) == 1
        assert cfg["PreToolUse"][0]["matcher"] == ".*"
        assert len(cfg["PreToolUse"][0]["hooks"]) == 1

    def test_custom_matcher(self, mesh):
        h = MeshHooks(agent="test", mesh=mesh, tool_matcher=r"^filesystem\.")
        cfg = h.agent_sdk_hooks()
        assert cfg["PreToolUse"][0]["matcher"] == r"^filesystem\."


class TestPreHook:
    def test_allow(self, hooks, mesh):
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="fs.read")
        result = _run(hooks._pre_hook(_input(), "id-1", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "allow"
        assert "permissionDecisionReason" not in result["hookSpecificOutput"]

    def test_deny(self, hooks, mesh):
        mesh.decide.return_value = Decision(
            action=Action.DENY, tool="fs.write", error="policy=default action=deny"
        )
        result = _run(hooks._pre_hook(_input(), "id-2", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
        assert "deny" in result["hookSpecificOutput"]["permissionDecisionReason"]

    def test_human_approval_maps_to_ask(self, hooks, mesh):
        mesh.decide.return_value = Decision(
            action=Action.HUMAN_APPROVAL, tool="fs.write"
        )
        result = _run(hooks._pre_hook(_input(), "id-3", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "ask"

    def test_error_maps_to_fail_action(self, hooks, mesh):
        mesh.decide.return_value = Decision(action=Action.ERROR, tool="fs.write")
        result = _run(hooks._pre_hook(_input(), "id-4", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "deny"

    def test_mesh_unreachable_fail_closed(self, hooks, mesh):
        mesh.decide.side_effect = ConnectionError("refused")
        result = _run(hooks._pre_hook(_input(), "id-5", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "deny"
        assert "unreachable" in result["hookSpecificOutput"]["permissionDecisionReason"]

    def test_mesh_unreachable_fail_open(self, mesh):
        h = MeshHooks(agent="test", mesh=mesh, fail_action="allow")
        mesh.decide.side_effect = ConnectionError("refused")
        result = _run(h._pre_hook(_input(), "id-6", None))
        assert result["hookSpecificOutput"]["permissionDecision"] == "allow"

    def test_passes_tool_name_and_input(self, hooks, mesh):
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="custom.tool")
        inp = _input(tool="custom.tool", params={"key": "val"})
        _run(hooks._pre_hook(inp, "id-7", None))
        mesh.decide.assert_called_once_with("custom.tool", {"key": "val"})
