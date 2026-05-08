"""Tests for the agent-mesh Python SDK client."""
from __future__ import annotations

from unittest.mock import MagicMock, patch

import pytest

from agent_mesh import AgentMesh, Decision
from agent_mesh.client import Action, AgentMeshError, Tool


@pytest.fixture
def mesh():
    return AgentMesh("http://localhost:9090", agent="test-agent")


class TestInit:
    def test_url_trailing_slash(self):
        m = AgentMesh("http://localhost:9090/")
        assert m._url == "http://localhost:9090"

    def test_auth_header(self):
        m = AgentMesh(agent="bot")
        assert m._session.headers["Authorization"] == "Bearer agent:bot"

    def test_default_url(self):
        m = AgentMesh()
        assert m._url == "http://localhost:9090"


class TestDecide:
    def test_allow(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.content = b'{"action":"allow","rule":"allow-all","agent":"test-agent","tool":"fs.read"}'
        mock_resp.json.return_value = {"action": "allow", "rule": "allow-all", "agent": "test-agent", "tool": "fs.read"}
        with patch.object(mesh._session, "post", return_value=mock_resp) as mock_post:
            d = mesh.decide("fs.read", {"path": "/tmp"})
        assert d.action == Action.ALLOW
        assert d.tool == "fs.read"
        body = mock_post.call_args[1]["json"]
        assert body["agent"] == "test-agent"
        assert body["tool"] == "fs.read"
        assert mock_post.call_args[0][0] == "http://localhost:9090/decide"

    def test_deny(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 403
        mock_resp.content = b'{"action":"deny","reason":"blocked by policy"}'
        mock_resp.json.return_value = {"action": "deny", "reason": "blocked by policy"}
        with patch.object(mesh._session, "post", return_value=mock_resp):
            d = mesh.decide("dangerous.tool", {})
        assert d.action == Action.DENY
        assert "blocked" in d.error

    def test_human_approval(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.content = b'{"action":"human_approval","rule":"needs-approval"}'
        mock_resp.json.return_value = {"action": "human_approval", "rule": "needs-approval"}
        with patch.object(mesh._session, "post", return_value=mock_resp):
            d = mesh.decide("fs.write", {})
        assert d.action == Action.HUMAN_APPROVAL


class TestCallTool:
    def test_allowed(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        mock_resp.text = '{"result": "ok"}'
        with patch.object(mesh._session, "post", return_value=mock_resp) as mock_post:
            d = mesh.call_tool("mesh.catalog", {})
        assert d.action == Action.ALLOW
        assert d.tool == "mesh.catalog"
        assert d.result == '{"result": "ok"}'
        assert mock_post.call_args[0][0] == "http://localhost:9090/tool/mesh.catalog"

    def test_denied(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 403
        with patch.object(mesh._session, "post", return_value=mock_resp):
            d = mesh.call_tool("dangerous.tool", {})
        assert d.action == Action.DENY

    def test_human_approval(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 202
        mock_resp.content = b'{"approval_id": "abc123"}'
        mock_resp.json.return_value = {"approval_id": "abc123"}
        with patch.object(mesh._session, "post", return_value=mock_resp):
            d = mesh.call_tool("fs.write", {"path": "/etc/passwd"})
        assert d.action == Action.HUMAN_APPROVAL
        assert d.approval_id == "abc123"

    def test_server_error(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 500
        mock_resp.text = "internal error"
        with patch.object(mesh._session, "post", return_value=mock_resp):
            d = mesh.call_tool("broken", {})
        assert d.action == Action.ERROR
        assert "500" in d.error


class TestTools:
    def test_list_tools(self, mesh):
        mock_resp = MagicMock()
        mock_resp.json.return_value = [
            {"name": "fs.read", "description": "Read a file", "source": "filesystem"},
            {"name": "git.status", "description": "Git status", "source": "git"},
        ]
        mock_resp.raise_for_status = MagicMock()
        with patch.object(mesh._session, "get", return_value=mock_resp):
            tools = mesh.tools()
        assert len(tools) == 2
        assert isinstance(tools[0], Tool)
        assert tools[0].name == "fs.read"
        assert tools[1].source == "git"


class TestApprovals:
    def test_approve(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        with patch.object(mesh._session, "post", return_value=mock_resp) as mock_post:
            assert mesh.approve("abc123") is True
        assert "/approvals/abc123/approve" in mock_post.call_args[0][0]

    def test_deny(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        with patch.object(mesh._session, "post", return_value=mock_resp):
            assert mesh.deny("abc123") is True


class TestGrants:
    def test_create_grant(self, mesh):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"id": "g1", "tools": "fs.*", "duration": "30m"}
        mock_resp.raise_for_status = MagicMock()
        with patch.object(mesh._session, "post", return_value=mock_resp) as mock_post:
            g = mesh.create_grant("fs.*", "30m")
        assert g["id"] == "g1"
        body = mock_post.call_args[1]["json"]
        assert body["agent"] == "test-agent"
        assert body["tools"] == "fs.*"

    def test_revoke_grant(self, mesh):
        mock_resp = MagicMock()
        mock_resp.status_code = 200
        with patch.object(mesh._session, "delete", return_value=mock_resp):
            assert mesh.revoke_grant("g1") is True


class TestHealth:
    def test_healthy(self, mesh):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"status": "ok", "tools": 42}
        mock_resp.raise_for_status = MagicMock()
        with patch.object(mesh._session, "get", return_value=mock_resp):
            assert mesh.is_healthy() is True

    def test_unreachable(self, mesh):
        with patch.object(mesh._session, "get", side_effect=requests.ConnectionError):
            h = mesh.health()
            assert h["status"] == "unreachable"
            assert mesh.is_healthy() is False


import requests
