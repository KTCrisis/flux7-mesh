# Harness hook — governing the tools mesh7 cannot proxy

## The gap

mesh7 governs what an agent routes *through* it. It cannot govern what the
harness runs by itself. In Claude Code that is `Bash`, `Read`, `Write`, `Edit`,
`WebFetch` and `Task` — the tools built into the CLI, which never touch the
proxy.

This is a structural limit, not an implementation gap: an MCP server can offer
tools, it cannot wrap tools it does not own. Exposing `bash` as a mesh7 CLI tool
does not close it either, because the harness's own `Bash` stays available and
an agent will take the shorter path.

The `PreToolUse` hook closes it from the other side. The harness asks mesh7
before running its own tools, so one policy file governs both surfaces:

| Surface | Path | Governed by |
|---------|------|-------------|
| MCP tools (`mcp__mesh7__*`) | proxy | policy, on the real tool name |
| Harness tools (`Bash`, `Write`, …) | hook | policy, on the harness tool name |
| Any other MCP server | neither | not governed — see [Coverage](#coverage) |

## Install

The hook ships with the Python SDK as a console script:

```bash
pip install flux7-mesh
which mesh7-hook
```

Add it to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": ".*",
        "hooks": [
          {
            "type": "command",
            "command": "mesh7-hook",
            "timeout": 10,
            "statusMessage": "mesh7 policy"
          }
        ]
      }
    ]
  }
}
```

Use an absolute path to the script if the harness does not start from a shell
with your virtualenv on `PATH`.

## Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `MESH7_HOOK_MODE` | `observe` | `observe` traces only, `enforce` applies denials |
| `MESH7_URL` | `http://localhost:9090` | mesh7 data plane |
| `MESH7_AGENT` | `claude` | agent identity to evaluate |
| `MESH7_HOOK_TIMEOUT` | `5` | seconds before the call is treated as a failure |
| `MESH7_HOOK_SKIP_PREFIX` | `mcp__` | tool prefixes left to the proxy, comma-separated |

## Roll it out in two steps

The policy engine is fail-closed. Arming `enforce` before any rule names `Bash`
denies every harness tool at the first keystroke, and the session is unusable
until you edit the policy from another terminal. Start in `observe`.

**Step 1 — observe.** The hook calls `/decide`, the trace is written, and
nothing is refused. It stays silent rather than answering `allow`, so the
prompts you would normally have seen still appear. Work a normal day, then read
what the harness actually asked for. `/traces` is on the control plane, so this
call has to come from loopback or carry the admin token:

```bash
curl -s 'http://localhost:9090/traces?agent=claude&limit=500' \
  | jq -r '.[] | select(.policy_rule == "default") | .tool' \
  | sort | uniq -c | sort -rn
```

Hook calls and proxy calls land in the same store, so this lists every tool the
agent used that no rule covers — harness tools among them. Each one would be a
denial the moment you switch to `enforce`.

**Step 2 — write the rules, then enforce.** Name the harness tools in the same
policy file as the MCP ones:

```yaml
name: claude
agent: "claude"
rules:
  # -- Harness tools --
  - tools: ["Read", "Glob", "Grep", "TodoWrite"]
    action: allow
  - tools: ["Bash", "Write", "Edit", "NotebookEdit"]
    action: human_approval
  - tools: ["WebFetch", "WebSearch"]
    action: allow
```

Then set `MESH7_HOOK_MODE=enforce`.

## Verdicts

| mesh7 action | Hook output | Effect |
|--------------|-------------|--------|
| `allow` | `allow` | runs without a prompt |
| `deny` | `deny` | the tool never runs, the reason is shown |
| `human_approval` | `ask` | the harness's own permission prompt |

`human_approval` maps to `ask` rather than to the mesh7 approval queue: the
operator is already at the keyboard, so the shorter loop is the right one. Use
the queue for unattended agents, where nobody is watching the terminal.

The hook fails closed in `enforce` mode. An unreachable mesh, a timeout, or an
answer that is not a decision all produce `deny`. In `observe` mode the same
failures produce silence.

## Coverage

Be precise about what this covers, because it is easy to overclaim.

The hook is mechanical only where the harness runs it. Claude Code's
`PreToolUse` hook is such a place: a `deny` means the tool never executes. But
the same policy is merely advisory in a harness with no hook, and the hook sees
nothing of what another MCP server does when the agent talks to it directly.

Two coverages, neither strictly better:

- **Proxy** — broad across runtimes, narrow across tools. Any agent that speaks
  MCP or HTTP is governed, but only for tools that transit mesh7.
- **Hook** — narrow across runtimes, broad across tools. Everything the harness
  executes is governed, but only in harnesses that expose a hook.

Running both is what makes the coverage add up on a coding harness.

## Why MCP tools are skipped

Tools already crossing the proxy are skipped by default. The harness prefixes
them with `mcp__<server>__` and replaces dots with underscores, so
`terraform.__dispatch` arrives as `mcp__mesh7__terraform___dispatch`. Mapping
that back is ambiguous — `filesystem_read_file` and `terraform.plan` both arrive
as `x_y` — and deciding on a name you cannot trust is worse than not deciding.
The proxy already evaluates these calls on their real name.

Set `MESH7_HOOK_SKIP_PREFIX` to a narrower value if you want the hook to see
other MCP servers, which the proxy does not govern. Their names will be
evaluated as the harness spells them, prefix included.

## Known limits

Policy conditions compare with `==`, `!=`, `<`, `<=`, `>`, `>=` only. There is
no substring or regex operator, so a rule can govern `Bash` as a whole but not
`rm -rf` inside the command string. Governing at command granularity needs a
`contains` operator in the condition engine — it does not exist today.
