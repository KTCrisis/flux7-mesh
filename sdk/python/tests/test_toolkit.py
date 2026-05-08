"""Tests for the GovernedToolkit."""
from __future__ import annotations

from unittest.mock import MagicMock, patch

import pytest

from mesh7 import GovernedToolkit
from mesh7.client import Action, AgentMesh, Decision


@pytest.fixture
def toolkit():
    tk = GovernedToolkit(agent="test", url="http://localhost:9090")

    @tk.tool
    def get_weather(city: str, units: str = "celsius") -> str:
        """Get current weather for a city."""
        return f"sunny in {city}"

    @tk.tool
    def add_numbers(a: int, b: int) -> int:
        """Add two numbers."""
        return a + b

    return tk


class TestSchemas:
    def test_generates_schemas(self, toolkit):
        schemas = toolkit.schemas()
        assert len(schemas) == 2
        weather = next(s for s in schemas if s["name"] == "get_weather")
        assert weather["description"] == "Get current weather for a city."
        assert weather["input_schema"]["properties"]["city"]["type"] == "string"
        assert weather["input_schema"]["properties"]["units"]["type"] == "string"
        assert "city" in weather["input_schema"]["required"]
        assert "units" not in weather["input_schema"]["required"]

    def test_integer_types(self, toolkit):
        schemas = toolkit.schemas()
        add = next(s for s in schemas if s["name"] == "add_numbers")
        assert add["input_schema"]["properties"]["a"]["type"] == "integer"
        assert add["input_schema"]["required"] == ["a", "b"]


class TestExecute:
    def test_allowed_runs_locally(self, toolkit):
        mock_decision = Decision(action=Action.ALLOW, tool="get_weather", result="")
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            d = toolkit.execute("get_weather", {"city": "Paris"})
        assert d.action == Action.ALLOW
        assert d.result == "sunny in Paris"

    def test_denied_does_not_run(self, toolkit):
        mock_decision = Decision(action=Action.DENY, tool="get_weather", error="denied")
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            d = toolkit.execute("get_weather", {"city": "Paris"})
        assert d.action == Action.DENY
        assert d.result == ""

    def test_unknown_tool(self, toolkit):
        d = toolkit.execute("nonexistent", {})
        assert d.action == Action.ERROR
        assert "unknown tool" in d.error

    def test_function_exception(self, toolkit):
        def failing_tool():
            raise ValueError("boom")

        toolkit.register(failing_tool, "failing")
        mock_decision = Decision(action=Action.ALLOW, tool="failing", result="")
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            d = toolkit.execute("failing", {})
        assert d.action == Action.ERROR
        assert "boom" in d.error


class TestProcessResponse:
    def test_processes_tool_use_blocks(self, toolkit):
        mock_decision = Decision(action=Action.ALLOW, tool="get_weather", result="")
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            results = toolkit.process_response([
                {"type": "text", "text": "Let me check the weather."},
                {"type": "tool_use", "id": "tu_1", "name": "get_weather", "input": {"city": "Lyon"}},
            ])
        assert len(results) == 1
        assert results[0]["type"] == "tool_result"
        assert results[0]["tool_use_id"] == "tu_1"
        assert results[0]["content"] == "sunny in Lyon"

    def test_denied_tool_returns_error(self, toolkit):
        mock_decision = Decision(action=Action.DENY, tool="get_weather", error="denied by policy")
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            results = toolkit.process_response([
                {"type": "tool_use", "id": "tu_2", "name": "get_weather", "input": {"city": "Lyon"}},
            ])
        assert results[0]["is_error"] is True
        assert "denied" in results[0]["content"]

    def test_approval_pending(self, toolkit):
        mock_decision = Decision(
            action=Action.HUMAN_APPROVAL, tool="get_weather", approval_id="ap_123"
        )
        with patch.object(toolkit._mesh, "decide", return_value=mock_decision):
            results = toolkit.process_response([
                {"type": "tool_use", "id": "tu_3", "name": "get_weather", "input": {"city": "Lyon"}},
            ])
        assert results[0]["is_error"] is True
        assert "ap_123" in results[0]["content"]


class TestRegister:
    def test_register_programmatic(self):
        tk = GovernedToolkit(agent="test")

        def my_func(x: str) -> str:
            """Do something."""
            return x.upper()

        tk.register(my_func, "custom_name")
        schemas = tk.schemas()
        assert schemas[0]["name"] == "custom_name"


class TestExecuteLocal:
    def test_runs_without_mesh(self, toolkit):
        d = toolkit.execute_local("add_numbers", {"a": 3, "b": 4})
        assert d.action == Action.ALLOW
        assert d.result == "7"

    def test_unknown_tool(self, toolkit):
        d = toolkit.execute_local("nope", {})
        assert d.action == Action.ERROR
