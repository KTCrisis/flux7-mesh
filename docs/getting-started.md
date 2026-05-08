# Getting Started

Install agent-mesh, write your first policy, and make a governed tool call. Five minutes.

## Install

=== "Go install"

    ```bash
    go install github.com/KTCrisis/flux7-mesh/cmd/agent-mesh@latest
    ```

=== "Binary (Linux amd64)"

    ```bash
    curl -L $(curl -s https://api.github.com/repos/KTCrisis/agent-mesh/releases/latest \
      | grep browser_download_url | grep linux_amd64 | cut -d '"' -f 4) \
      -o agent-mesh && chmod +x agent-mesh && sudo mv agent-mesh /usr/local/bin/
    ```

Verify:

```bash
agent-mesh --version
```

## Minimal config

Create `config.yaml`:

```yaml
mcp_servers:
  - name: filesystem
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/home/user"]

policies:
  - name: default
    agent: "*"
    rules:
      - tools: ["filesystem.read_file", "filesystem.list_directory"]
        action: allow
      - tools: ["filesystem.write_file"]
        action: human_approval
      - tools: ["*"]
        action: deny
```

This config:

1. Connects to the filesystem MCP server
2. Allows read operations
3. Requires human approval for writes
4. Denies everything else

## Run with Claude Code

```bash
claude mcp add agent-mesh -- agent-mesh --mcp --config config.yaml
```

Claude Code now routes all tool calls through agent-mesh. Open Claude Code and try:

```
> Read the file config.yaml
```

This should work (policy: allow). Now try:

```
> Write "hello" to /home/user/test.txt
```

You'll see an approval prompt. Say yes — agent-mesh traces the decision.

## Run standalone (HTTP mode)

```bash
agent-mesh --config config.yaml
```

```bash
# List available tools
curl http://localhost:9090/tools | python3 -m json.tool

# Call a tool
curl -X POST http://localhost:9090/tool/filesystem.read_file \
  -H "Authorization: Bearer agent:my-script" \
  -H "Content-Type: application/json" \
  -d '{"params":{"path":"/home/user/config.yaml"}}'

# Check traces
curl http://localhost:9090/traces | python3 -m json.tool
```

## What just happened

```
Your agent ──► agent-mesh ──► filesystem MCP server
                  │
                  ├── policy check (allow / deny / human_approval)
                  ├── rate limit check
                  ├── trace recorded (JSONL)
                  └── approval queue (if human_approval)
```

Every tool call is logged. Every policy decision is traceable. The agent doesn't know the proxy exists.

## Next steps

- [Writing Policies](writing-policies.md) — per-agent rules, globs, conditions
- [Approval Flow](approval-flow.md) — approval queue, grants, CLI
- [Deployment Modes](deployment-modes.md) — solo dev, team, Managed Agents
