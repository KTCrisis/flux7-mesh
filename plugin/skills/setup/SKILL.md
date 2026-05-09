---
name: setup
description: Generate a starter config.yaml config for flux7-mesh
user-invocable: true
argument-hint: "[template]"
allowed-tools:
  - Read
  - Write
  - Bash
---

Generate a starter flux7-mesh configuration.

Templates available:
- `minimal` (default) — single agent, basic allow/deny
- `travel` — travel agent with weather, flights, gmail
- `dev` — dev agent with filesystem, git, github

If $0 is "travel":

```yaml
port: 9091
mcp_servers:
  - name: weather
    transport: stdio
    command: npx
    args: ["-y", "open-meteo-mcp-server"]
  - name: flights
    transport: stdio
    command: npx
    args: ["-y", "google-flights-mcp-server"]
policies:
  - name: travel-agent
    agent: "claude"
    rules:
      - tools: ["weather.*"]
        action: allow
      - tools: ["flights.*"]
        action: allow
  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
```

If $0 is "dev":

```yaml
port: 9090
mcp_servers:
  - name: filesystem
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "."]
policies:
  - name: dev-agent
    agent: "claude"
    rules:
      - tools: ["filesystem.read_file", "filesystem.list_directory"]
        action: allow
      - tools: ["filesystem.write_file"]
        action: allow
        condition:
          field: "params.path"
          operator: "not_contains"
          value: ".env"
  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
```

Otherwise (minimal):

```yaml
port: 9090
policies:
  - name: my-agent
    agent: "claude"
    rules:
      - tools: ["*"]
        action: allow
  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
```

Write the config to `config.yaml` in the current directory. Confirm the file was written.
