"""GovernedToolkit — wrap Python functions as governed tools for the Claude API.

Usage::

    from mesh7 import GovernedToolkit

    toolkit = GovernedToolkit(agent="my-agent")

    @toolkit.tool
    def get_weather(city: str) -> str:
        \"\"\"Get current weather for a city.\"\"\"
        return fetch_weather(city)

    # Generate tools[] for Claude API (names are namespace-qualified)
    schemas = toolkit.schemas()  # [{"name": "my-agent.get_weather", ...}]

    # Execute tool_use blocks with governance
    result = toolkit.execute("my-agent.get_weather", tool_input)
"""
from __future__ import annotations

import inspect
import json
from typing import Any, Callable, get_type_hints

from mesh7.client import Action, AgentMesh, AgentMeshError, Decision


def _python_type_to_json(t: Any) -> str:
    mapping = {str: "string", int: "integer", float: "number", bool: "boolean"}
    return mapping.get(t, "string")


def _build_schema(func: Callable) -> dict[str, Any]:
    """Build a JSON Schema input_schema from a function's signature and type hints."""
    hints = get_type_hints(func)
    sig = inspect.signature(func)
    properties: dict[str, Any] = {}
    required: list[str] = []

    for name, param in sig.parameters.items():
        prop: dict[str, Any] = {"type": _python_type_to_json(hints.get(name, str))}
        desc = ""
        if func.__doc__:
            for line in func.__doc__.splitlines():
                stripped = line.strip()
                if stripped.startswith(f":param {name}:") or stripped.startswith(f"{name}:"):
                    desc = stripped.split(":", 2)[-1].strip()
                    break
        if desc:
            prop["description"] = desc
        properties[name] = prop
        if param.default is inspect.Parameter.empty:
            required.append(name)

    schema: dict[str, Any] = {"type": "object", "properties": properties}
    if required:
        schema["required"] = required
    return schema


def tool(func: Callable) -> Callable:
    """Standalone decorator — marks a function as a governed tool."""
    func._is_governed_tool = True  # type: ignore[attr-defined]
    return func


class GovernedToolkit:
    def __init__(
        self,
        agent: str = "default",
        url: str = "http://localhost:9090",
        mesh: AgentMesh | None = None,
        namespace: str | None = None,
    ) -> None:
        self._mesh = mesh or AgentMesh(url=url, agent=agent)
        self._tools: dict[str, Callable] = {}
        self._namespace = namespace or agent

    def _qualify(self, name: str) -> str:
        """Qualify a tool name with the namespace (e.g. 'read_file' -> 'audit7.read_file')."""
        if "." in name:
            return name
        return f"{self._namespace}.{name}"

    def _unqualify(self, name: str) -> str:
        """Strip the namespace prefix to get the local tool name."""
        prefix = f"{self._namespace}."
        if name.startswith(prefix):
            return name[len(prefix):]
        return name

    def tool(self, func: Callable) -> Callable:
        """Register a function as a governed tool."""
        self._tools[func.__name__] = func
        func._is_governed_tool = True  # type: ignore[attr-defined]
        return func

    def register(self, func: Callable, name: str | None = None) -> None:
        """Register a function programmatically."""
        key = name or func.__name__
        self._tools[key] = func

    def schemas(self) -> list[dict[str, Any]]:
        """Generate the tools[] array for the Claude API messages endpoint.

        Tool names are namespace-qualified (e.g. 'audit7.get_weather').
        """
        result = []
        for name, func in self._tools.items():
            result.append({
                "name": self._qualify(name),
                "description": (func.__doc__ or "").strip().split("\n")[0],
                "input_schema": _build_schema(func),
            })
        return result

    def execute(self, tool_name: str, tool_input: dict[str, Any]) -> Decision:
        """Execute a tool call with governance: decide via mesh, then run locally.

        Accepts both qualified ('audit7.get_weather') and bare ('get_weather') names.
        The qualified name is sent to mesh for policy evaluation; the bare name is
        used for local function lookup.
        """
        qualified = self._qualify(tool_name)
        local = self._unqualify(tool_name)
        if local not in self._tools:
            return Decision(
                action=Action.ERROR,
                tool=qualified,
                error=f"unknown tool: {qualified}",
            )
        decision = self._mesh.decide(qualified, tool_input)
        if decision.action != Action.ALLOW:
            return decision
        try:
            result = self._tools[local](**tool_input)
            decision.result = str(result) if result is not None else ""
        except Exception as e:
            decision.action = Action.ERROR
            decision.error = str(e)
        return decision

    def execute_local(self, tool_name: str, tool_input: dict[str, Any]) -> Decision:
        """Execute locally without going through mesh (for tools not proxied).

        Accepts both qualified and bare names.
        """
        qualified = self._qualify(tool_name)
        local = self._unqualify(tool_name)
        if local not in self._tools:
            return Decision(
                action=Action.ERROR,
                tool=qualified,
                error=f"unknown tool: {qualified}",
            )
        try:
            result = self._tools[local](**tool_input)
            return Decision(
                action=Action.ALLOW,
                tool=qualified,
                result=str(result) if result is not None else "",
            )
        except Exception as e:
            return Decision(action=Action.ERROR, tool=qualified, error=str(e))

    def process_response(self, content: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """Process tool_use blocks from a Claude API response.

        Returns tool_result blocks ready to send back.
        """
        results = []
        for block in content:
            if block.get("type") != "tool_use":
                continue
            decision = self.execute(block["name"], block.get("input", {}))
            if decision.action in (Action.DENY, Action.ERROR):
                results.append({
                    "type": "tool_result",
                    "tool_use_id": block["id"],
                    "is_error": True,
                    "content": decision.error or f"denied: {decision.tool}",
                })
            elif decision.action == Action.HUMAN_APPROVAL:
                results.append({
                    "type": "tool_result",
                    "tool_use_id": block["id"],
                    "is_error": True,
                    "content": f"awaiting human approval (id: {decision.approval_id})",
                })
            else:
                results.append({
                    "type": "tool_result",
                    "tool_use_id": block["id"],
                    "content": decision.result,
                })
        return results
