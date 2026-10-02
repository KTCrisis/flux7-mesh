# Emergency stop

An agent loops, spends, or acts on an injected instruction. The emergency stop
blocks every tool call of that agent, of one session, or of everything, at
once, and lifts the block later.

## What a stop does

| Scope | Blocks | Revokes | Denies the pending approvals of |
|---|---|---|---|
| `all` | every call of every agent | every grant | every agent |
| `agent` (an ID or a glob) | the agent's calls | the agent's grants | the agent |
| `session` | the calls of that session | nothing (grants are not tied to a session) | that session |

- **First, before everything.** The stop is checked before the rate limiter,
  the policy, the grants and the approvals, on every path: `POST /tool/...`,
  `POST /decide` and MCP (stdio and Streamable HTTP). No rule and no grant lets
  a halted call through.
- **What the agent sees.** HTTP answers `403` with `"policy": "halted"` and the
  reason. `/decide` answers `"action": "deny"` with `"halted": true`, so every
  existing client refuses the call. MCP returns an error result whose text
  carries the reason.
- **Every process.** With `storage_path`, stops live in the state database and
  every mesh7 process reloads them within a second, including a standalone
  `mesh7 --mcp` client started before the stop. They survive a restart. Without
  `storage_path`, a stop holds in memory, for that process only.
- **Traced.** The stop and the resume are recorded in the trace chain
  (`mesh.halt`, `mesh.resume`, with the operator, the scope, the revoked grants
  and the denied approvals), and so is every call refused by a stop
  (`policy: halted`, `policy_rule: halt:<id>`).

## Resuming

Resuming lifts the stop and puts back the grants it revoked, with their
original ID, expiry and origin. A grant that expired in the meantime stays
gone. Pending approvals denied by the stop are not reopened: the agent asks
again.

Starting a stop identical to one in force returns the one in force: pressing
the button twice does not stack two stops.

## From the CLI

```bash
mesh halt --agent scout7 --reason "loops on search"
mesh halt --session 7f3c...
mesh halt --all --reason "incident"
mesh halts                # the stops in force
mesh resume 1563d782      # by ID or prefix
```

`MESH_URL` points at the daemon; `MESH_ADMIN_TOKEN` is sent when the control
plane is not on the loopback.

## From the console

The **Emergency stop** page stops everything in one click, or one agent or
session, and lists the stops in force with a Resume button. While a stop is in
force, a red banner shows on every page of the console.

## How it works inside

**One request.** The console button, `mesh halt` and the API all end in `POST /halts` on the daemon, a control-plane route.

**When the stop arrives**, mesh7, in this order:

1. records it in the `halts` table of the state database, after checking the scope; an identical stop already in force is returned instead of a second one;
2. revokes the grants in scope and keeps a copy of them on the stop, for the resume;
3. denies the approvals waiting in scope: an agent blocked on a pending approval gets its refusal now, signed `halt:<id>`, instead of at the timeout;
4. records `mesh.halt` in the trace chain: who, what scope, why, which grants and approvals.

**At the agent's next call**, the stop is the first check, before everything else:

```text
tool call
  │
  ├─ 0. a stop in force for this agent or session?  → refused here
  ├─ 1. rate limit
  ├─ 2. policy (allow / deny / human_approval)
  ├─ 3. temporal grant
  ├─ 4. mem7 precedents, supervisor, human
  └─ 5. forward to the tool
```

The tool is never reached, and the refused call is traced (`policy: halted`, `policy_rule: halt:<id>`), so what the agent tried during the stop stays visible.

**Across processes.** Each mesh7 process reloads the active stops from the database at most every second before deciding. A standalone `mesh7 --mcp` client, started before the stop and holding its own memory, is therefore stopped within a second. If a reload fails (database locked, disk full), the process keeps the stops it already knew instead of dropping them: a failure cannot lift a stop.

**On resume**, the row is marked resumed (who, when), calls go through the normal chain again, and the revoked grants come back with their original ID, expiry and origin, except those that expired during the stop. The resume is traced as `mesh.resume` with the grants restored.

## HTTP API (control plane)

| Method | Path | Body | Answer |
|---|---|---|---|
| `GET` | `/halts` | | the stops in force |
| `POST` | `/halts` | `{"scope": "all" \| "agent" \| "session", "target": "...", "reason": "...", "by": "..."}` | `201` with the stop, the number of grants revoked and approvals denied; `200` with `"already_active": true` when an identical stop is in force |
| `POST` | `/halts/{id}/resume` | `{"by": "..."}` (optional) | `200` with the stop and the number of grants restored; `404` when no stop has this ID |

Like the rest of the control plane, these endpoints require the admin token,
or a loopback caller when none is set.

## What it does not do

mesh7 does not revoke the agent's credentials at your identity provider: a stop
blocks the agent at mesh7, which is where its tool calls go. Revoking the
agent's token upstream (Okta, Entra) remains a separate step.
