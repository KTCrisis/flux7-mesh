# CLI Tool Sources

Govern CLI tools (terraform, kubectl, docker, gh, aws, gcloud…) through mesh7.
Agents call them via MCP or HTTP. Agent-mesh enforces policy, traces every call, and requires approval when configured — just like HTTP and MCP tools.

No competitor covers CLI tool governance today.

## How it works

```
Agent calls terraform.plan
  → Registry lookup (exact match or dynamic dispatch)
  → Policy evaluation (allow / deny / human_approval)
  → Temporal grant check (bypass approval if granted)
  → Argument validation (allowlist + injection rejection)
  → exec.Command() — never sh -c
  → Capture stdout/stderr (capped at 1MB)
  → Trace entry (tool, params, exit_code, duration)
  → Return structured result
```

CLI tools reuse the same policy engine, approval flow, grants, rate limiting, and tracing as HTTP and MCP tools. No special treatment needed in policy rules.

## Configuration

Add a `cli_tools` section to your `config.yaml`:

```yaml
cli_tools:
  - name: terraform
    bin: terraform
    default_action: human_approval
    commands:
      plan:
        timeout: 120s
      apply:
        allowed_args: ["-target"]
        timeout: 300s
```

### Four modes

#### Simple — just the binary

Wraps all subcommands. `default_action` applies to everything. Minimal config.

```yaml
cli_tools:
  - name: gh
    bin: gh
    default_action: allow
```

The agent can call `gh.pr.list`, `gh.issue.create`, etc. All go through policy and tracing.

#### Fine-tuned — binary + command overrides

Declare specific commands for custom rules (timeout, allowed_args). Unlisted commands fall through to `default_action`.

```yaml
cli_tools:
  - name: terraform
    bin: terraform
    working_dir: /home/user/infra
    default_action: human_approval
    env:
      TF_IN_AUTOMATION: "1"
    commands:
      plan:
        timeout: 120s
      apply:
        allowed_args: ["-target"]
        timeout: 300s
      destroy:
        allowed_args: ["-target"]
        timeout: 300s
      # init, validate, fmt, etc. → default_action (human_approval)
```

#### Strict — only declared commands allowed

With `strict: true`, any command not explicitly listed is denied. For high-risk tools.

```yaml
cli_tools:
  - name: kubectl
    bin: kubectl
    strict: true
    commands:
      get:
        allowed_args: ["-n", "--namespace", "-o", "--output"]
      apply:
        allowed_args: ["-f", "-n", "--namespace"]
      delete:
        allowed_args: ["-n", "--namespace"]
      # logs, exec, port-forward → denied (not listed)
```

#### Bare — binary without subcommands

For CLIs that take only flags and positional arguments (`jq`, `ffmpeg`, custom tools). Registers a single `<name>.run` tool — no subcommand is ever injected, no catch-all dispatch.

```yaml
cli_tools:
  - name: play7
    bin: /home/user/bin/play7.exe
    default_action: allow
    bare:
      allowed_args: ["--port", "--out", "--list"]
      timeout: 2m
```

The agent calls `play7.run` with `args` (and optionally `stdin`). `bare` is mutually exclusive with `commands` and `strict`.

### Config fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Tool name prefix (e.g. `terraform`) — must be unique |
| `bin` | string | yes | Path or name of the binary (resolved via PATH) |
| `default_action` | string | no | `allow`, `deny`, or `human_approval` (default: `deny`) |
| `strict` | bool | no | Only declared commands allowed (default: `false`) |
| `working_dir` | string | no | Working directory for all commands |
| `env` | map | no | Environment variables (isolated — only PATH, HOME, LANG + these) |
| `commands` | map | no | Per-command overrides (see below) |
| `bare` | object | no | Bare-binary mode: same fields as a command entry. Exclusive with `commands`/`strict`. |

#### Command fields

| Field | Type | Description |
|-------|------|-------------|
| `allowed_args` | []string | Flags the agent can use (e.g. `["-n", "--namespace"]`). Omit for no restriction. |
| `timeout` | string | Max execution time (e.g. `"30s"`, `"5m"`). Default: 30s. |

### Validation

Config loading fails if:
- `name` is empty or duplicated
- `bin` is empty
- `default_action` is not `allow`, `deny`, or `human_approval`
- `strict: true` with no `commands` declared
- `timeout` is not parsable as a Go duration
- `bare` combined with `commands` or `strict`

## Policy integration

Policy rules work identically to HTTP and MCP tools. Tool names follow the `<name>.<command>` pattern:

```yaml
policies:
  - name: infra-governance
    agent: claude
    rules:
      - tools: ["terraform.destroy"]
        action: deny                    # never, regardless of default_action
      - tools: ["terraform.plan"]
        action: allow                   # override default human_approval
      - tools: ["kubectl.delete*"]
        action: human_approval
      - tools: ["gh.*"]
        action: allow
```

### Resolution order

```
1. Policy rule match?              → use policy action
2. Temporal grant match?           → allow (bypass approval)
3. Strict mode, command not listed? → deny
4. Command declared with config?   → validate args, then default_action
5. default_action set?             → use default_action
6. Nothing?                        → deny (fail-closed)
```

## Tool naming and dispatch

### Declared commands

Each declared command gets a named tool: `terraform.plan`, `kubectl.get`, etc.

### Dynamic dispatch (simple and fine-tuned modes)

Non-strict tools also register a `<name>.__dispatch` catch-all tool. When an agent calls a tool that doesn't have an exact match (e.g. `terraform.init`), mesh7 falls back to `terraform.__dispatch` and extracts the subcommand from the tool name.

This means agents can call `terraform.init` directly — they don't need to know about `__dispatch`.

### MCP exposure

CLI tools appear as standard MCP tools in `tools/list`:

```json
{
  "name": "terraform.plan",
  "description": "Run terraform plan",
  "inputSchema": {
    "type": "object",
    "properties": {
      "args": {"type": "array", "description": "Command arguments"},
      "flags": {"type": "object", "description": "Named flags"}
    }
  }
}
```

## Call format

Agents pass parameters in three formats (combinable):

```json
{
  "params": {
    "args": ["-target", "aws_instance.web"],
    "flags": {
      "namespace": "prod",
      "output": "json"
    }
  }
}
```

- `args`: positional arguments passed directly
- `flags`: named flags, converted to CLI args (`-n prod` for single char, `--namespace prod` for multi char)
- `stdin`: string piped to the process stdin

For catch-all dispatch, include `command`:

```json
{
  "params": {
    "command": "init",
    "args": ["-backend=false"]
  }
}
```

### stdin

Any CLI tool accepts an optional `stdin` param:

```json
{
  "params": {
    "stdin": "{\"steps\":[{\"notes\":[\"C4\",\"E4\",\"G4\"],\"beats\":2}]}"
  }
}
```

stdin is **data, not shell syntax**: it is piped directly to the process and is deliberately exempt from metacharacter validation (a JSON or text payload may legitimately contain `|`, `$`, etc.). It never touches a shell. Note that stdin content is recorded in traces like any other param — by design, but worth knowing for large payloads.

## Security

CLI execution is the highest-risk surface in agent governance. Every layer has a specific defense:

| Threat | Protection |
|--------|------------|
| Shell injection (`; rm -rf /`) | `exec.Command()` directly — never `sh -c` |
| Argument injection (`--kubeconfig=/etc/shadow`) | `allowed_args` allowlist enforcement |
| Metacharacter injection (`` ` ``, `$(`, `&&`, `\|`) | Rejected before execution |
| Command timeout/hang | `context.WithTimeout` + process kill |
| Output bomb (huge stdout) | Capped at 1MB (configurable) |
| Env variable leak | Isolated env: only PATH + HOME + LANG + declared vars |
| Working directory escape | Fixed `cmd.Dir` from config |

### What gets rejected

Arguments containing any of these characters are rejected before execution:

```
;  &&  ||  |  `  $(  ${  \n  \r
```

### Allowed args enforcement

When `allowed_args` is set, only flags matching the list are accepted. Supports both short (`-n`) and long (`--namespace`) forms, including `--flag=value` syntax. Positional arguments (not starting with `-`) are always allowed (subject to metacharacter check).

## HTTP API

CLI tools are called the same way as any other tool:

```bash
# Declared command
curl -X POST http://localhost:9090/tool/terraform.plan \
  -H "Authorization: Bearer agent:claude" \
  -d '{"params": {"args": ["-out=plan.tfplan"]}}'

# Dynamic dispatch
curl -X POST http://localhost:9090/tool/terraform.init \
  -H "Authorization: Bearer agent:claude" \
  -d '{"params": {"args": ["-backend=false"]}}'
```

### Response format

```json
{
  "result": {
    "stdout": "Refreshing Terraform state...\nPlan: 2 to add, 0 to change, 0 to destroy.",
    "stderr": "",
    "exit_code": 0
  },
  "trace_id": "a1b2c3d4e5f6...",
  "policy": "allow",
  "latency_ms": 1523
}
```

## Tracing

CLI tool calls are traced like any other tool, with the addition of `exit_code`:

```json
{
  "trace_id": "a1b2c3d4...",
  "agent_id": "claude",
  "tool": "terraform.plan",
  "params": {"args": ["-out=plan.tfplan"]},
  "policy": "allow",
  "policy_rule": "infra-governance",
  "status_code": 200,
  "exit_code": 0,
  "latency_ms": 1523,
  "timestamp": "2026-04-09T21:34:13Z"
}
```

## Example

See [`examples/cli-tools/config.yaml`](../examples/cli-tools/config.yaml) for a complete example with terraform (fine-tuned), kubectl (strict), and gh (simple).

```bash
# Start with CLI tools
./mesh7 --mcp --config examples/cli-tools/config.yaml

# Or plug directly into Claude Code
claude mcp add mesh7 -- ./mesh7 --mcp --config config.yaml
```
