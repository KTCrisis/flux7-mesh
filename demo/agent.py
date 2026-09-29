#!/usr/bin/env python3
"""A scripted agent for the mesh7 demo: it talks MCP over HTTP to the demo
mesh as `sales-assistant`, one step at a time, so the presenter can show the
console between steps. Standard library only.

    python3 agent.py tools      what the agent is offered
    python3 agent.py action     a read, then a write that waits for a human
    python3 agent.py sql        an interpreter tool: where text matching stops
    python3 agent.py rugpull    after the upstream changed its catalogue
"""
import json
import os
import sys
import urllib.request

MESH = os.environ.get("MESH", "http://localhost:9191")
AGENT = os.environ.get("AGENT", "sales-assistant")


class Session:
    def __init__(self):
        self.sid = None
        self.n = 0
        self.rpc("initialize", {"protocolVersion": "2024-11-05", "capabilities": {},
                                "clientInfo": {"name": "demo-agent", "version": "1"}})

    def rpc(self, method, params=None):
        self.n += 1
        body = json.dumps({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params or {}}).encode()
        req = urllib.request.Request(MESH + "/mcp", data=body, method="POST", headers={
            "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
            "Authorization": "Bearer agent:" + AGENT, **({"Mcp-Session-Id": self.sid} if self.sid else {})})
        with urllib.request.urlopen(req) as resp:
            self.sid = resp.headers.get("Mcp-Session-Id") or self.sid
            raw = resp.read().decode()
        if raw.startswith("event:") or raw.startswith("data:"):
            raw = next(l[5:] for l in raw.splitlines() if l.startswith("data:"))
        return json.loads(raw)

    def call(self, tool, **args):
        res = self.rpc("tools/call", {"name": tool, "arguments": args})
        if "error" in res:
            return "ERROR " + res["error"]["message"]
        return readable("\n".join(c.get("text", "") for c in res["result"].get("content", [])))


def readable(text):
    """mesh7 returns the upstream MCP result serialized as text; unwrap it so
    the demo shows what the CRM answered, one JSON line per value."""
    try:
        inner = json.loads(text)
    except ValueError:
        return text
    if isinstance(inner, dict) and "content" in inner:
        text = "\n".join(c.get("text", "") for c in inner["content"])
        try:
            return json.dumps(json.loads(text), ensure_ascii=False)
        except ValueError:
            return text
    return text


def step(title, text):
    print(f"\n\033[1;36m▸ {title}\033[0m")
    print("  " + text.replace("\n", "\n  "))


def pause(msg="Enter to continue"):
    if sys.stdin.isatty():
        input(f"\n\033[2m… {msg}\033[0m")


def tools(s):
    listed = s.rpc("tools/list")["result"]["tools"]
    step("Tools offered to the agent", "\n".join(t["name"] for t in listed))


def action(s):
    step("get_customer C-1001 (a read)", s.call("crm.get_customer", id="C-1001"))
    pause()
    args = dict(id="C-1001", field="segment", value="strategic")
    step("update_customer C-1001 segment=strategic (a write)", s.call("crm.update_customer", **args))
    pause("Approve it in the console (Approvals), then Enter: the agent retries the same call")
    step("Retry of the same call", s.call("crm.update_customer", **args))
    pause()
    step("Retry once more: one approval, one run", s.call("crm.update_customer", **args))


def sql(s):
    for stmt in ["SELECT name FROM customers", "DELETE FROM customers", "select * from customers"]:
        step(f"run_sql: {stmt}", s.call("crm.run_sql", sql=stmt))
        pause()


def rugpull(s):
    tools(s)
    pause()
    step("get_customer C-1002 (its description changed)", s.call("crm.get_customer", id="C-1002"))
    pause()
    step("export_all_customers (appeared after the review)", s.call("crm.export_all_customers"))


if __name__ == "__main__":
    act = sys.argv[1] if len(sys.argv) > 1 else "action"
    acts = {"tools": tools, "action": action, "sql": sql, "rugpull": rugpull}
    if act not in acts:
        sys.exit(__doc__)
    acts[act](Session())
