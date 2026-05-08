# Writing Policies

Policies are YAML rules that decide what each agent can do. First match wins. Fail-closed — if no rule matches, the tool call is denied.

## Basic structure

```yaml
policies:
  - name: my-policy
    agent: "claude"        # which agent (glob pattern)
    rules:
      - tools: ["filesystem.read_*"]
        action: allow
      - tools: ["filesystem.write_*"]
        action: human_approval
      - tools: ["*"]
        action: deny
```

### Actions

| Action | What happens |
|--------|-------------|
| `allow` | Tool call proceeds immediately |
| `deny` | Tool call rejected, 403 returned |
| `human_approval` | Tool call queued, waits for human/supervisor |

### First match wins

Rules are evaluated top to bottom. The first rule whose `tools` pattern matches is applied. Put specific rules before general ones:

```yaml
rules:
  # Specific: deny destructive gmail operations
  - tools: ["gmail.delete_*", "gmail.move_to_trash"]
    action: deny

  # Medium: require approval for sends
  - tools: ["gmail.send_email"]
    action: human_approval

  # General: allow reads
  - tools: ["gmail.read_*", "gmail.list_*"]
    action: allow

  # Catch-all
  - tools: ["*"]
    action: deny
```

## Glob patterns

Both `agent` and `tools` fields support glob patterns:

| Pattern | Matches |
|---------|---------|
| `"claude"` | Exact match |
| `"*"` | Any agent or tool |
| `"worker-*"` | `worker-1`, `worker-docs`, etc. |
| `"gmail-*.send_email"` | `gmail-ktcrisis.send_email`, `gmail-perso.send_email` |
| `"filesystem.read_*"` | `filesystem.read_file`, `filesystem.read_multiple_files` |

## Per-agent policy files

Instead of putting all policies inline, use a directory:

```yaml
# config.yaml
policy_dir: policies/
```

```
policies/
├── claude.yaml       # rules for agent "claude"
├── worker.yaml       # rules for agent "worker-*"
└── default.yaml      # catch-all rules
```

Each file is a single policy:

```yaml
# policies/claude.yaml
name: claude
agent: "claude"
rules:
  - tools: ["filesystem.read_*"]
    action: allow
  - tools: ["filesystem.write_*"]
    action: human_approval
```

```yaml
# policies/default.yaml
name: default
agent: "*"
rules:
  - tools: ["*"]
    action: deny
```

Files are loaded alphabetically after any inline `policies:`. Duplicate names produce an error.

## Policy specificity

When multiple policies match an agent, more specific agent globs are evaluated first:

```
"claude"      → exact match, highest priority
"worker-*"    → prefix match
"*"           → catch-all, lowest priority
```

This means `agent: "claude"` rules are checked before `agent: "*"` rules, regardless of file order.

## Conditions

Rules can include conditions on request parameters:

```yaml
rules:
  - tools: ["payment.transfer"]
    action: allow
    condition:
      field: "params.amount"
      operator: "<"
      value: 100

  - tools: ["payment.transfer"]
    action: human_approval
```

This allows transfers under 100 automatically, requires approval for larger amounts.

### Supported operators

| Operator | Example |
|----------|---------|
| `<` | `params.amount < 100` |
| `>` | `params.amount > 1000` |
| `==` | `params.status == 1` |
| `!=` | `params.priority != 0` |

## Rate limiting

Per-policy rate limits protect against runaway loops:

```yaml
policies:
  - name: claude
    agent: "claude"
    rate_limit:
      max_per_minute: 60    # sliding window
      max_total: 1000       # lifetime of process
    rules:
      - tools: ["*"]
        action: allow
```

Loop detection is automatic: same tool + same params > 3 times in 10 seconds triggers a block.

## Agent identity

Agents identify themselves differently depending on the transport:

| Transport | Identity source |
|-----------|----------------|
| MCP stdio | `--mcp-agent` flag |
| HTTP | `Authorization: Bearer agent:<id>` header |
| MCP Streamable HTTP | `Authorization: Bearer agent:<id>` header |

If no identity is provided, the agent is `anonymous` (HTTP) or uses the configured default (MCP).

## Example: real-world config

```yaml
policies:
  - name: claude
    agent: "claude"
    rate_limit:
      max_per_minute: 120
    rules:
      # Read anything
      - tools: ["filesystem.read_*", "filesystem.list_*", "filesystem.search_*"]
        action: allow

      # Write with approval
      - tools: ["filesystem.write_file", "filesystem.edit_file"]
        action: human_approval

      # No destructive ops
      - tools: ["filesystem.move_file"]
        action: deny

      # Gmail: read yes, send with approval, delete never
      - tools: ["gmail.delete_*", "gmail.batch_*"]
        action: deny
      - tools: ["gmail.read_*", "gmail.list_*"]
        action: allow
      - tools: ["gmail.send_email", "gmail.draft_email"]
        action: human_approval

      # Local LLM: always allowed
      - tools: ["ollama.*"]
        action: allow

      # Deny everything else
      - tools: ["*"]
        action: deny

  - name: default
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
```

## Next steps

- [Approval Flow](approval-flow.md) — what happens when a tool call hits `human_approval`
- [CLI Tools](cli-tools.md) — governing git, docker, terraform as tools
- [Memory Integration](mem7-auto-approve.md) — auto-approve from past decisions
