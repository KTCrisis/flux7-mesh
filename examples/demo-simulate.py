#!/usr/bin/env python3
"""
Simulate 15 agents calling tools through flux7-mesh.

Usage:
    python demo-simulate.py --scenario enterprise
    python demo-simulate.py --scenario eda
    python demo-simulate.py --scenario enterprise --mesh http://localhost:9090

Each agent makes 5-10 tool calls matching its role. The supervisor
auto-resolves pending approvals. Designed to populate the agent7
dashboard with realistic traces.
"""

import argparse
import asyncio
import json
import random
import time
from dataclasses import dataclass

import httpx

# ──────────────────────────────────────────────
# Agent definitions per scenario
# ──────────────────────────────────────────────

@dataclass
class AgentCall:
    tool: str
    params: dict


ENTERPRISE_AGENTS: dict[str, list[AgentCall]] = {
    # Developers — read, write (approval), some denied
    "dev-backend": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/main.py"}),
        AgentCall("filesystem.list_directory", {"path": "/tmp/demo-enterprise/src"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/src/main.py", "content": "# updated handler\ndef main(): pass"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "backend-status", "value": "main.py updated with new handler", "tags": ["dev", "backend"]}),
        AgentCall("git.git_status", {"repo_path": "/tmp/demo-enterprise"}),
    ],
    "dev-frontend": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/app.tsx"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/src/app.tsx", "content": "export default function App() { return <div>Hello</div> }"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "frontend-status", "value": "app.tsx component added", "tags": ["dev", "frontend"]}),
        AgentCall("filesystem.directory_tree", {"path": "/tmp/demo-enterprise/src"}),
    ],
    "dev-data": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/data/schema.avsc"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/data/schema.avsc", "content": '{"type":"record","name":"User","fields":[{"name":"id","type":"string"}]}'}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "schema-update", "value": "User schema v2 with new id field", "tags": ["data", "schema"]}),
    ],
    "dev-infra": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/infra/docker-compose.yml"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/infra/docker-compose.yml", "content": "version: '3'\nservices:\n  api:\n    build: .\n    ports:\n      - '8080:8080'"}),
        AgentCall("git.git_log", {"repo_path": "/tmp/demo-enterprise"}),
        AgentCall("filesystem.move_file", {"source": "/tmp/demo-enterprise/old.txt", "destination": "/tmp/demo-enterprise/new.txt"}),  # should be denied
    ],

    # Reviewers — read only, writes denied
    "review-code": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/main.py"}),
        AgentCall("git.git_diff", {"repo_path": "/tmp/demo-enterprise"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "review-main", "value": "main.py looks clean, no issues", "tags": ["review"]}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/src/main.py", "content": "# reviewer trying to write — should be denied"}),  # denied
    ],
    "review-security": [
        AgentCall("filesystem.search_files", {"path": "/tmp/demo-enterprise", "pattern": "password"}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/auth.py"}),
        AgentCall("git.git_log", {"repo_path": "/tmp/demo-enterprise"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "security-scan", "value": "no hardcoded credentials found", "tags": ["security"]}),
        AgentCall("git.git_show", {"repo_path": "/tmp/demo-enterprise", "revision": "HEAD"}),
    ],
    "review-docs": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/README.md"}),
        AgentCall("filesystem.list_directory", {"path": "/tmp/demo-enterprise/docs"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "docs-review", "value": "README needs API reference section", "tags": ["docs", "review"]}),
    ],

    # QA
    "qa-integration": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/tests/test_api.py"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/tests/test_new.py", "content": "def test_health(): assert True"}),  # approval
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "qa-status", "value": "new test added for health endpoint", "tags": ["qa"]}),
    ],
    "qa-performance": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/tests/bench.py"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/tests/bench_new.py", "content": "import time\ndef test_latency(): start=time.time(); assert time.time()-start < 1"}),  # approval
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "perf-baseline", "value": "p99 < 200ms target set", "tags": ["qa", "perf"]}),
    ],

    # Ops
    "ops-deploy": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/infra/k8s-deploy.yaml"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/infra/k8s-deploy.yaml", "content": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api"}),  # approval
        AgentCall("git.git_status", {"repo_path": "/tmp/demo-enterprise"}),
        AgentCall("memory.memory_store", {"key": "deploy-status", "value": "k8s manifest updated", "tags": ["ops", "deploy"]}),
    ],
    "ops-monitor": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/infra/alerts.yaml"}),
        AgentCall("memory.memory_recall", {"query": "deploy-status"}),
        AgentCall("memory.memory_store", {"key": "monitor-check", "value": "all alerts configured", "tags": ["ops", "monitor"]}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/infra/alerts.yaml", "content": "alerts:\n  - name: high-latency\n    threshold: 500ms"}),  # approval
    ],

    # Security
    "security-scanner": [
        AgentCall("filesystem.search_files", {"path": "/tmp/demo-enterprise", "pattern": "eval("}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/auth.py"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "vuln-report", "value": "SQL injection found in auth.py line 42", "tags": ["security", "critical"]}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/SECURITY.md", "content": "# Vulnerabilities found"}),  # denied — security can't write
    ],

    # Writers
    "writer-docs": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/README.md"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/docs/api-reference.md", "content": "# API Reference\n\n## GET /health\nReturns service health status."}),  # approval
        AgentCall("memory.memory_store", {"key": "docs-written", "value": "API reference draft created", "tags": ["docs"]}),
    ],

    # PM
    "pm-lead": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/README.md"}),
        AgentCall("filesystem.list_directory", {"path": "/tmp/demo-enterprise"}),
        AgentCall("memory.memory_list", {}),
        AgentCall("memory.memory_recall", {"query": "status"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-enterprise/STATUS.md", "content": "# Status"}),  # denied — PM can't write
    ],

    # Supervisor
    "supervisor": [
        AgentCall("memory.memory_list", {}),
        AgentCall("memory.memory_recall", {"query": "status"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-enterprise/src/main.py"}),  # denied — supervisor can't read files
    ],
}


EDA_AGENTS: dict[str, list[AgentCall]] = {
    # Spec writers
    "writer-openapi": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/users-api.yaml"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/specs/users-api.yaml", "content": "openapi: '3.1.0'\ninfo:\n  title: Users API\n  version: '1.0.0'\npaths:\n  /users:\n    get:\n      operationId: listUsers"}),
        AgentCall("memory.memory_store", {"key": "spec-users-api", "value": "OpenAPI spec v1.0.0 generated for Users API", "tags": ["spec", "openapi"]}),
    ],
    "writer-asyncapi": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/specs/order-events.yaml", "content": "asyncapi: '3.0.0'\ninfo:\n  title: Order Events\nchannels:\n  orders.events:\n    messages:\n      orderCreated:\n        payload:\n          type: object"}),
        AgentCall("memory.memory_store", {"key": "spec-order-events", "value": "AsyncAPI spec for order events on Kafka", "tags": ["spec", "asyncapi"]}),
        AgentCall("git.git_status", {"repo_path": "/tmp/demo-eda"}),  # denied — writers can't use git
    ],
    "writer-avro": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/schemas/order.avsc"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/schemas/order.avsc", "content": '{"type":"record","name":"OrderCreated","namespace":"com.example.orders","fields":[{"name":"orderId","type":"string"},{"name":"amount","type":"double"}]}'}),
        AgentCall("memory.memory_store", {"key": "schema-order", "value": "Avro schema OrderCreated v1", "tags": ["schema", "avro"]}),
    ],

    # Readers
    "reader-api": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/users-api.yaml"}),
        AgentCall("filesystem.list_directory", {"path": "/tmp/demo-eda/specs"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "analysis-users-api", "value": "Users API: 3 endpoints, CRUD pattern, no auth defined", "tags": ["analysis", "api"]}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/report.md", "content": "# report"}),  # denied — readers can't write
    ],
    "reader-events": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("filesystem.search_files", {"path": "/tmp/demo-eda", "pattern": "asyncapi"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "analysis-order-events", "value": "order events: well-structured, missing correlationId", "tags": ["analysis", "events"]}),
    ],
    "reader-deps": [
        AgentCall("filesystem.list_directory", {"path": "/tmp/demo-eda/specs"}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/users-api.yaml"}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("memory.memory_recall", {"query": "spec"}),
        AgentCall("memory.memory_store", {"key": "dep-graph", "value": "order-events depends on users-api (customerId reference)", "tags": ["deps"]}),
    ],

    # Validators
    "validator-openapi": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/users-api.yaml"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "validation-users-api", "value": "WARN: missing description on GET /users, missing 400/500 responses", "tags": ["validation", "openapi"]}),
    ],
    "validator-asyncapi": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "validation-order-events", "value": "FAIL: missing correlationId in orderCreated message", "tags": ["validation", "asyncapi"]}),
    ],
    "validator-compat": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/schemas/order.avsc"}),
        AgentCall("memory.memory_recall", {"query": "schema-order"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "compat-check-order", "value": "BREAKING: new required field timestamp without default", "tags": ["validation", "compat"]}),
    ],

    # Ops
    "ops-registry": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/schemas/order.avsc"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/deploy/registry-push.json", "content": '{"subject":"orders-value","schema":"OrderCreated v1"}'}),  # approval
        AgentCall("memory.memory_store", {"key": "registry-push", "value": "OrderCreated v1 pushed to schema registry", "tags": ["ops", "registry"]}),
        AgentCall("git.git_status", {"repo_path": "/tmp/demo-eda"}),  # approval
    ],
    "ops-catalog": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/catalog/order-events.md", "content": "# Order Events\n\nAsync channel for order lifecycle events."}),  # approval
        AgentCall("git.git_log", {"repo_path": "/tmp/demo-eda"}),
        AgentCall("git.git_diff", {"repo_path": "/tmp/demo-eda"}),  # approval
        AgentCall("memory.memory_store", {"key": "catalog-update", "value": "EventCatalog entry for order-events updated", "tags": ["ops", "catalog"]}),
    ],
    "ops-deploy": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/deploy/kafka-topics.yaml"}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/deploy/kafka-topics.yaml", "content": "topics:\n  - name: orders.events\n    partitions: 6\n    replication: 3\n    config:\n      retention.ms: 604800000"}),  # approval
        AgentCall("git.git_status", {"repo_path": "/tmp/demo-eda"}),  # approval
        AgentCall("memory.memory_store", {"key": "kafka-deploy", "value": "orders.events topic config: 6 partitions, 7d retention", "tags": ["ops", "kafka"]}),
    ],

    # Governance
    "governance-scorer": [
        AgentCall("memory.memory_recall", {"query": "validation"}),
        AgentCall("memory.memory_recall", {"query": "spec"}),
        AgentCall("memory.memory_list", {}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "governance-score", "value": "order-events: 65/100 (missing correlationId, no error responses)", "tags": ["governance", "score"]}),
        AgentCall("filesystem.write_file", {"path": "/tmp/demo-eda/scores.json", "content": "{}"}),  # denied — scorer can't write files
    ],
    "governance-reviewer": [
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),
        AgentCall("git.git_diff", {"repo_path": "/tmp/demo-eda"}),
        AgentCall("git.git_log", {"repo_path": "/tmp/demo-eda"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("memory.memory_store", {"key": "review-order-schema", "value": "REJECT: breaking change, needs default value for timestamp", "tags": ["governance", "review"]}),
        AgentCall("git.git_show", {"repo_path": "/tmp/demo-eda", "revision": "HEAD"}),
    ],

    # Supervisor
    "supervisor": [
        AgentCall("memory.memory_list", {}),
        AgentCall("memory.memory_recall", {"query": "governance"}),
        AgentCall("ollama.generate", {"model": "gemma4:e4b", "prompt": "ok"}),
        AgentCall("filesystem.read_file", {"path": "/tmp/demo-eda/specs/order-events.yaml"}),  # denied
    ],
}


# ──────────────────────────────────────────────
# Simulation engine
# ──────────────────────────────────────────────

async def call_tool(
    client: httpx.AsyncClient,
    mesh_url: str,
    agent_id: str,
    call: AgentCall,
) -> dict:
    """Make a single tool call through flux7-mesh."""
    url = f"{mesh_url}/tool/{call.tool}"
    headers = {"Authorization": f"Bearer agent:{agent_id}"}
    # ollama.generate can queue behind other concurrent requests — allow more time
    timeout = 120 if call.tool.startswith("ollama.") else 30
    try:
        resp = await client.post(url, json={"params": call.params}, headers=headers, timeout=timeout)
        data = resp.json()
        policy = data.get("policy", "unknown")
        symbol = {"allow": "+", "deny": "x", "human_approval": "?", "grant": "g"}.get(policy, "?")
        print(f"  [{symbol}] {agent_id:24s} → {call.tool:40s} [{policy}]")
        return data
    except Exception as e:
        err_msg = str(e) or type(e).__name__
        print(f"  [!] {agent_id:24s} → {call.tool:40s} [error: {err_msg}]")
        return {"error": err_msg}


async def run_agent(
    client: httpx.AsyncClient,
    mesh_url: str,
    agent_id: str,
    calls: list[AgentCall],
):
    """Simulate one agent making its tool calls sequentially."""
    for call in calls:
        await call_tool(client, mesh_url, agent_id, call)
        await asyncio.sleep(random.uniform(0.1, 0.5))


async def auto_approve(
    client: httpx.AsyncClient,
    mesh_url: str,
    duration: float,
):
    """Supervisor loop: auto-approve pending approvals."""
    end = time.time() + duration
    approved = 0
    denied = 0
    while time.time() < end:
        try:
            resp = await client.get(f"{mesh_url}/approvals")
            if resp.status_code == 200:
                approvals = resp.json()
                for a in approvals:
                    if a.get("status") != "pending":
                        continue
                    tool = a.get("tool", "")
                    agent = a.get("agent_id", "")
                    aid = a["id"]

                    # Supervisor logic: approve routine, deny suspicious
                    if "move_file" in tool or "delete" in tool:
                        await client.post(
                            f"{mesh_url}/approvals/{aid}/deny",
                            json={"reasoning": "destructive operation blocked by supervisor"},
                        )
                        print(f"  [S] supervisor denied  {agent} → {tool}")
                        denied += 1
                    else:
                        await client.post(
                            f"{mesh_url}/approvals/{aid}/approve",
                            json={"reasoning": "routine operation approved by supervisor"},
                        )
                        print(f"  [S] supervisor approved {agent} → {tool}")
                        approved += 1
        except Exception:
            pass
        await asyncio.sleep(1)
    return approved, denied


async def run_simulation(mesh_url: str, scenario: str):
    agents = ENTERPRISE_AGENTS if scenario == "enterprise" else EDA_AGENTS
    demo_dir = "/tmp/demo-enterprise" if scenario == "enterprise" else "/tmp/demo-eda"

    print(f"\n{'='*70}")
    print(f"  flux7-mesh demo: {scenario}")
    print(f"  {len(agents)} agents, {sum(len(c) for c in agents.values())} tool calls")
    print(f"  mesh: {mesh_url}")
    print(f"{'='*70}\n")

    # Create demo directory structure
    import os
    dirs = [
        f"{demo_dir}/src", f"{demo_dir}/tests", f"{demo_dir}/docs",
        f"{demo_dir}/infra", f"{demo_dir}/data",
    ]
    if scenario == "eda":
        dirs = [
            f"{demo_dir}/specs", f"{demo_dir}/schemas", f"{demo_dir}/deploy",
            f"{demo_dir}/catalog", f"{demo_dir}/docs",
        ]
    for d in dirs:
        os.makedirs(d, exist_ok=True)

    # Seed some files so reads don't 404
    seed = {
        "enterprise": {
            f"{demo_dir}/src/main.py": "def main():\n    print('hello')\n",
            f"{demo_dir}/src/app.tsx": "export default function App() { return null }",
            f"{demo_dir}/src/auth.py": "def authenticate(user_id):\n    query = f'SELECT * FROM users WHERE id={user_id}'\n",
            f"{demo_dir}/tests/test_api.py": "def test_health():\n    assert True\n",
            f"{demo_dir}/tests/bench.py": "# benchmarks\n",
            f"{demo_dir}/data/schema.avsc": '{"type":"record","name":"User","fields":[]}',
            f"{demo_dir}/infra/docker-compose.yml": "version: '3'\nservices: {}\n",
            f"{demo_dir}/infra/k8s-deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\n",
            f"{demo_dir}/infra/alerts.yaml": "alerts: []\n",
            f"{demo_dir}/README.md": "# Demo Enterprise Project\n",
            f"{demo_dir}/docs/.gitkeep": "",
        },
        "eda": {
            f"{demo_dir}/specs/users-api.yaml": "openapi: '3.1.0'\ninfo:\n  title: Users API\n  version: '0.1.0'\n",
            f"{demo_dir}/specs/order-events.yaml": "asyncapi: '3.0.0'\ninfo:\n  title: Order Events\n",
            f"{demo_dir}/schemas/order.avsc": '{"type":"record","name":"OrderCreated","fields":[{"name":"orderId","type":"string"}]}',
            f"{demo_dir}/deploy/kafka-topics.yaml": "topics: []\n",
            f"{demo_dir}/deploy/registry-push.json": "{}",
            f"{demo_dir}/catalog/.gitkeep": "",
            f"{demo_dir}/docs/.gitkeep": "",
        },
    }
    for path, content in seed.get(scenario, {}).items():
        with open(path, "w") as f:
            f.write(content)

    # Initialize git repo
    os.system(f"cd {demo_dir} && git init -q && git add -A && git commit -q -m 'init' 2>/dev/null")

    async with httpx.AsyncClient() as client:
        # Check connectivity
        try:
            resp = await client.get(f"{mesh_url}/health", timeout=5)
            health = resp.json()
            print(f"  mesh status: {health.get('status')} — {health.get('tools')} tools\n")
        except Exception as e:
            print(f"  ERROR: cannot reach flux7-mesh at {mesh_url}: {e}")
            print(f"  Start flux7-mesh first:")
            print(f"    mesh7 --config examples/demo-{scenario}.yaml\n")
            return

        # Launch all agents in parallel + supervisor
        start = time.time()

        agent_tasks = [
            run_agent(client, mesh_url, agent_id, calls)
            for agent_id, calls in agents.items()
            if agent_id != "supervisor"
        ]
        supervisor_task = auto_approve(client, mesh_url, duration=30)

        # Run agents + supervisor concurrently
        results = await asyncio.gather(
            *agent_tasks,
            supervisor_task,
            return_exceptions=True,
        )

        elapsed = time.time() - start
        approved, denied_by_sup = results[-1] if isinstance(results[-1], tuple) else (0, 0)

        # Run supervisor's own calls last
        if "supervisor" in agents:
            print(f"\n  --- supervisor own calls ---")
            await run_agent(client, mesh_url, "supervisor", agents["supervisor"])

        # Summary
        try:
            resp = await client.get(f"{mesh_url}/traces?limit=500")
            traces = resp.json() if resp.status_code == 200 else []
        except Exception:
            traces = []

        print(f"\n{'='*70}")
        print(f"  Simulation complete in {elapsed:.1f}s")
        print(f"  Total traces: {len(traces)}")
        print(f"  Supervisor: {approved} approved, {denied_by_sup} denied")
        print(f"")
        print(f"  Open agent7 dashboard: http://localhost:3000/mesh")
        print(f"{'='*70}\n")


def main():
    parser = argparse.ArgumentParser(description="Simulate 15 agents through flux7-mesh")
    parser.add_argument("--scenario", choices=["enterprise", "eda"], default="enterprise")
    parser.add_argument("--mesh", default="http://localhost:9090")
    args = parser.parse_args()

    asyncio.run(run_simulation(args.mesh, args.scenario))


if __name__ == "__main__":
    main()
