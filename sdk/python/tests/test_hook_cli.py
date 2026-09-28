"""Tests for the Claude Code CLI PreToolUse hook."""
from __future__ import annotations

import io
import json
from unittest.mock import MagicMock

import pytest

from mesh7.client import Action, AgentMesh, Decision
from mesh7.hook_cli import decide, main


@pytest.fixture
def mesh():
    return MagicMock(spec=AgentMesh)


@pytest.fixture(autouse=True)
def clean_env(monkeypatch):
    for var in (
        "MESH7_HOOK_MODE",
        "MESH7_URL",
        "MESH7_AGENT",
        "MESH7_HOOK_TIMEOUT",
        "MESH7_HOOK_SKIP_PREFIX",
    ):
        monkeypatch.delenv(var, raising=False)


def _payload(tool: str = "Bash", params: dict | None = None) -> dict:
    return {
        "hook_event_name": "PreToolUse",
        "tool_name": tool,
        "tool_input": params or {"command": "ls"},
        "session_id": "sess-1",
        "cwd": "/home/test",
    }


def _verdict(out: dict | None) -> str | None:
    if out is None:
        return None
    return out["hookSpecificOutput"]["permissionDecision"]


def _enforce(monkeypatch):
    monkeypatch.setenv("MESH7_HOOK_MODE", "enforce")


class TestObserveMode:
    """Observe mode traces without ever changing what the harness would do."""

    def test_default_mode_is_observe(self, mesh):
        mesh.decide.return_value = Decision(action=Action.DENY, tool="Bash", error="nope")
        assert decide(_payload(), mesh) is None

    def test_observe_still_calls_decide(self, mesh):
        mesh.decide.return_value = Decision(action=Action.DENY, tool="Bash")
        decide(_payload(), mesh)
        mesh.decide.assert_called_once_with("Bash", {"command": "ls"}, session_id="sess-1")

    def test_observe_stays_silent_on_allow(self, mesh):
        """An allow must not suppress the prompt the operator would have seen."""
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="Bash")
        assert decide(_payload(), mesh) is None

    def test_observe_stays_silent_when_mesh_is_down(self, mesh):
        mesh.decide.side_effect = ConnectionError("refused")
        assert decide(_payload(), mesh) is None


class TestEnforceMode:
    def test_allow(self, mesh, monkeypatch):
        _enforce(monkeypatch)
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="Bash")
        assert _verdict(decide(_payload(), mesh)) == "allow"

    def test_deny_carries_the_reason(self, mesh, monkeypatch):
        _enforce(monkeypatch)
        mesh.decide.return_value = Decision(action=Action.DENY, tool="Bash", error="policy claude")
        out = decide(_payload(), mesh)
        assert _verdict(out) == "deny"
        assert out["hookSpecificOutput"]["permissionDecisionReason"] == "policy claude"

    def test_human_approval_maps_to_ask(self, mesh, monkeypatch):
        """The operator is at the keyboard — the harness prompt is the short loop."""
        _enforce(monkeypatch)
        mesh.decide.return_value = Decision(action=Action.HUMAN_APPROVAL, tool="Write")
        assert _verdict(decide(_payload("Write"), mesh)) == "ask"

    def test_unreachable_mesh_fails_closed(self, mesh, monkeypatch):
        _enforce(monkeypatch)
        mesh.decide.side_effect = ConnectionError("refused")
        out = decide(_payload(), mesh)
        assert _verdict(out) == "deny"
        assert "fail closed" in out["hookSpecificOutput"]["permissionDecisionReason"]

    def test_error_action_fails_closed(self, mesh, monkeypatch):
        """A 401 or a malformed body is not a decision."""
        _enforce(monkeypatch)
        mesh.decide.return_value = Decision(action=Action.ERROR, tool="Bash")
        assert _verdict(decide(_payload(), mesh)) == "deny"


class TestScope:
    """The hook governs what the proxy cannot reach, and nothing else."""

    @pytest.mark.parametrize("tool", ["mcp__mesh7__filesystem_read_file", "mcp__other__thing"])
    def test_mcp_tools_are_left_to_the_proxy(self, mesh, monkeypatch, tool):
        _enforce(monkeypatch)
        assert decide(_payload(tool), mesh) is None
        mesh.decide.assert_not_called()

    def test_skip_prefix_is_configurable(self, mesh, monkeypatch):
        _enforce(monkeypatch)
        monkeypatch.setenv("MESH7_HOOK_SKIP_PREFIX", "mcp__mesh7__,Notebook")
        mesh.decide.return_value = Decision(action=Action.DENY, tool="x")

        assert decide(_payload("NotebookEdit"), mesh) is None
        # A different MCP server is no longer skipped under this setting.
        assert _verdict(decide(_payload("mcp__other__thing"), mesh)) == "deny"

    @pytest.mark.parametrize("tool", ["Bash", "Read", "Write", "Edit", "WebFetch", "Task"])
    def test_native_tools_are_evaluated(self, mesh, monkeypatch, tool):
        _enforce(monkeypatch)
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool=tool)
        assert _verdict(decide(_payload(tool), mesh)) == "allow"

    def test_missing_tool_name_is_ignored(self, mesh, monkeypatch):
        _enforce(monkeypatch)
        assert decide({"hook_event_name": "PreToolUse"}, mesh) is None
        mesh.decide.assert_not_called()


class TestMain:
    def test_unparseable_stdin_stays_silent(self, monkeypatch, capsys):
        """A harness payload change must not wedge the session."""
        _enforce(monkeypatch)
        monkeypatch.setattr("sys.stdin", io.StringIO("not json"))
        assert main() == 0
        assert capsys.readouterr().out == ""

    def test_non_object_payload_stays_silent(self, monkeypatch, capsys):
        _enforce(monkeypatch)
        monkeypatch.setattr("sys.stdin", io.StringIO("[1, 2]"))
        assert main() == 0
        assert capsys.readouterr().out == ""

    def test_skipped_tool_prints_nothing(self, monkeypatch, capsys):
        _enforce(monkeypatch)
        monkeypatch.setattr(
            "sys.stdin", io.StringIO(json.dumps(_payload("mcp__mesh7__memory_memory_store")))
        )
        assert main() == 0
        assert capsys.readouterr().out == ""

    def test_emits_valid_json_on_deny(self, monkeypatch, capsys):
        _enforce(monkeypatch)
        monkeypatch.setattr("sys.stdin", io.StringIO(json.dumps(_payload())))

        fake = MagicMock(spec=AgentMesh)
        fake.decide.return_value = Decision(action=Action.DENY, tool="Bash", error="nope")
        monkeypatch.setattr("mesh7.hook_cli.AgentMesh", lambda **kw: fake)

        assert main() == 0
        out = json.loads(capsys.readouterr().out)
        assert out["hookSpecificOutput"]["hookEventName"] == "PreToolUse"
        assert out["hookSpecificOutput"]["permissionDecision"] == "deny"

    def test_client_is_built_from_the_environment(self, monkeypatch, capsys):
        _enforce(monkeypatch)
        monkeypatch.setenv("MESH7_URL", "http://mesh.internal:9999")
        monkeypatch.setenv("MESH7_AGENT", "claude-code")
        monkeypatch.setenv("MESH7_HOOK_TIMEOUT", "3")
        monkeypatch.setattr("sys.stdin", io.StringIO(json.dumps(_payload())))

        seen: dict = {}
        fake = MagicMock(spec=AgentMesh)
        fake.decide.return_value = Decision(action=Action.ALLOW, tool="Bash")

        def build(**kwargs):
            seen.update(kwargs)
            return fake

        monkeypatch.setattr("mesh7.hook_cli.AgentMesh", build)
        assert main() == 0
        assert seen == {
            "url": "http://mesh.internal:9999",
            "agent": "claude-code",
            "timeout": 3,
            "token": None,  # MESH7_TOKEN unset: legacy header, never an empty Bearer
        }


def test_mesh7_token_env_reaches_the_client(monkeypatch):
    """MESH7_TOKEN must become AgentMesh(token=...); unset must stay None so
    the legacy header is used, not an empty Bearer."""
    from mesh7 import hook_cli

    seen: dict = {}

    class FakeMesh:
        def __init__(self, **kw):
            seen.update(kw)

        def decide(self, *a, **k):
            return Decision(action=Action.ALLOW, tool="Bash")

    monkeypatch.setattr(hook_cli, "AgentMesh", FakeMesh)
    monkeypatch.setenv("MESH7_HOOK_MODE", "enforce")
    monkeypatch.setenv("MESH7_TOKEN", "eyJ.header.sig")
    hook_cli.decide({"tool_name": "Bash", "tool_input": {"command": "ls"}})
    assert seen.get("token") == "eyJ.header.sig"

    seen.clear()
    monkeypatch.delenv("MESH7_TOKEN")
    hook_cli.decide({"tool_name": "Bash", "tool_input": {"command": "ls"}})
    assert seen.get("token") is None


class TestSession:
    """The harness session reaches the mesh, so traces group by session."""

    def test_session_id_is_forwarded(self, mesh):
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="Bash")
        decide(_payload(), mesh)
        assert mesh.decide.call_args.kwargs["session_id"] == "sess-1"

    def test_missing_session_id_sends_none(self, mesh):
        mesh.decide.return_value = Decision(action=Action.ALLOW, tool="Bash")
        payload = _payload()
        del payload["session_id"]
        decide(payload, mesh)
        assert mesh.decide.call_args.kwargs["session_id"] is None
