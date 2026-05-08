"""agent-mesh Python SDK — governance mesh for AI agent tool calls.

Usage::

    from mesh7 import AgentMesh

    mesh = AgentMesh("http://localhost:9090", agent="my-agent")
    decision = mesh.decide("filesystem.write_file", {"path": "/tmp/x"})
    print(decision.action)  # "allow" | "deny" | "human_approval"

    tools = mesh.tools()
    health = mesh.health()
"""
from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any

import requests


class Action(str, Enum):
    ALLOW = "allow"
    DENY = "deny"
    HUMAN_APPROVAL = "human_approval"
    ERROR = "error"


@dataclass
class Decision:
    action: Action
    tool: str
    result: str = ""
    error: str = ""
    approval_id: str = ""


@dataclass
class Tool:
    name: str
    description: str = ""
    source: str = ""


class AgentMeshError(Exception):
    pass


class AgentMesh:
    def __init__(
        self,
        url: str = "http://localhost:9090",
        agent: str = "default",
        timeout: int = 300,
    ) -> None:
        self._url = url.rstrip("/")
        self._agent = agent
        self._timeout = timeout
        self._session = requests.Session()
        self._session.headers["Authorization"] = f"Bearer agent:{agent}"
        self._session.headers["Content-Type"] = "application/json"

    def decide(self, name: str, arguments: dict[str, Any] | None = None) -> Decision:
        """Evaluate policy without executing. Returns allow/deny/human_approval."""
        resp = self._session.post(
            f"{self._url}/decide",
            json={"agent": self._agent, "tool": name, "arguments": arguments or {}},
            timeout=self._timeout,
        )
        body = resp.json() if resp.content else {}
        action = body.get("action", "error")
        try:
            act = Action(action)
        except ValueError:
            act = Action.ERROR
        return Decision(
            action=act,
            tool=name,
            error=body.get("reason", "") if act == Action.DENY else "",
        )

    def call_tool(self, name: str, arguments: dict[str, Any] | None = None) -> Decision:
        """Execute a tool call through agent-mesh (policy + execute + trace)."""
        resp = self._session.post(
            f"{self._url}/tool/{name}",
            json=arguments or {},
            timeout=self._timeout,
        )
        if resp.status_code == 403:
            return Decision(action=Action.DENY, tool=name, error="denied by policy")
        if resp.status_code == 202:
            body = resp.json() if resp.content else {}
            return Decision(
                action=Action.HUMAN_APPROVAL,
                tool=name,
                approval_id=body.get("approval_id", ""),
            )
        if resp.status_code >= 400:
            return Decision(
                action=Action.ERROR,
                tool=name,
                error=f"HTTP {resp.status_code}: {resp.text}",
            )
        return Decision(
            action=Action.ALLOW,
            tool=name,
            result=resp.text,
        )

    def tools(self) -> list[Tool]:
        """List all available tools."""
        resp = self._session.get(f"{self._url}/tools", timeout=self._timeout)
        resp.raise_for_status()
        items = resp.json()
        return [
            Tool(
                name=t.get("name", ""),
                description=t.get("description", ""),
                source=t.get("source", ""),
            )
            for t in items
        ]

    def approvals(self, status: str | None = None, tool: str | None = None) -> list[dict[str, Any]]:
        """List approvals, optionally filtered by status and/or tool glob."""
        params: dict[str, str] = {}
        if status:
            params["status"] = status
        if tool:
            params["tool"] = tool
        resp = self._session.get(
            f"{self._url}/approvals", params=params, timeout=self._timeout
        )
        resp.raise_for_status()
        return resp.json()

    def pending(self, tool_scope: str | None = None) -> list[dict[str, Any]]:
        """List pending approvals, optionally filtered by tool glob."""
        return self.approvals(status="pending", tool=tool_scope)

    def approval_detail(self, approval_id: str) -> dict[str, Any]:
        """Get approval detail with recent traces and active grants context."""
        resp = self._session.get(
            f"{self._url}/approvals/{approval_id}", timeout=self._timeout
        )
        resp.raise_for_status()
        return resp.json()

    def approve(self, approval_id: str) -> bool:
        """Approve a pending request (simple, no metadata)."""
        resp = self._session.post(
            f"{self._url}/approvals/{approval_id}/approve",
            timeout=self._timeout,
        )
        return resp.status_code == 200

    def deny(self, approval_id: str) -> bool:
        """Deny a pending request (simple, no metadata)."""
        resp = self._session.post(
            f"{self._url}/approvals/{approval_id}/deny",
            timeout=self._timeout,
        )
        return resp.status_code == 200

    def resolve(
        self,
        approval_id: str,
        action: str,
        *,
        resolved_by: str = "",
        reasoning: str = "",
        confidence: float = 0.0,
    ) -> bool:
        """Resolve an approval with full metadata (reasoning, confidence)."""
        body: dict[str, Any] = {}
        if resolved_by:
            body["resolved_by"] = resolved_by
        if reasoning:
            body["reasoning"] = reasoning
        if confidence > 0:
            body["confidence"] = confidence
        resp = self._session.post(
            f"{self._url}/approvals/{approval_id}/{action}",
            json=body if body else None,
            timeout=self._timeout,
        )
        return resp.status_code == 200

    def grants(self) -> list[dict[str, Any]]:
        """List active grants."""
        resp = self._session.get(f"{self._url}/grants", timeout=self._timeout)
        resp.raise_for_status()
        return resp.json()

    def create_grant(self, tools: str, duration: str) -> dict[str, Any]:
        """Create a temporal grant."""
        resp = self._session.post(
            f"{self._url}/grants",
            json={"agent": self._agent, "tools": tools, "duration": duration},
            timeout=self._timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def revoke_grant(self, grant_id: str) -> bool:
        """Revoke an active grant."""
        resp = self._session.delete(
            f"{self._url}/grants/{grant_id}",
            timeout=self._timeout,
        )
        return resp.status_code == 200

    def traces(self, limit: int = 100) -> list[dict[str, Any]]:
        """Get recent traces."""
        resp = self._session.get(
            f"{self._url}/traces",
            params={"limit": limit},
            timeout=self._timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def health(self) -> dict[str, Any]:
        """Get mesh health status."""
        try:
            resp = self._session.get(f"{self._url}/health", timeout=5)
            resp.raise_for_status()
            return resp.json()
        except requests.RequestException:
            return {"status": "unreachable"}

    def is_healthy(self) -> bool:
        return self.health().get("status") == "ok"
