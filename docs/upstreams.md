# Changing Upstreams

The upstream MCP servers of a running mesh can be added, changed and removed without a restart, and without disturbing the servers you did not touch. Nothing new is started without your go-ahead.

## What happens when the file changes

mesh7 watches `config.yaml`. When it changes, the declared `mcp_servers` are compared, by name, with the ones that run:

| The declaration is | mesh7 does |
|---|---|
| unchanged | nothing: the server keeps running, calls in flight included |
| gone | closes the server and removes its tools, at once |
| new | **holds it**: nothing is started |
| different (command, arguments, environment, URL, headers) | **holds it**: the server keeps running as it was declared before |

A held declaration is started when an operator accepts it:

```bash
mesh7 upstream held
# time  (stdio, replaces the running declaration)
#     runs     uvx mcp-server-time@2026.11.11
# fetch  (stdio, new server)
#     runs     uvx mcp-server-fetch@2026.11.11
#     env      HTTP_PROXY

mesh7 upstream accept time      # closes the running `time`, starts the new declaration
mesh7 upstream accept fetch
```

`held` shows what each declaration would run: the command and its arguments, or the URL, and the names of the env and header keys. Their values are never shown.

A config that does not load changes nothing: the running upstreams stay and the reason is logged. So does a list with a duplicate name or an unknown transport.

## Why a declaration is held

A stdio declaration is a command line, and starting it runs a program as mesh7. The config file can be written by anything able to write files as the mesh's user, and an agent whose policy allows a file-writing tool is one of them. If a file change alone started the program, such an agent could make the mesh execute what it likes and step outside its governance.

So the rule is the one pinning applies to tools: what is new or changed waits for a human. Two cases need no acceptance:

- a declaration that is **removed**: it can only restrict;
- a held declaration that is withdrawn, or set back to the accepted one: it simply stops being held.

## A restart is not a go-ahead

A service is started again by a reboot or after a crash, with nobody watching. So mesh7 remembers what you accepted, in its state database (`storage_path`), as one fingerprint per server name, and a start launches only the declarations that match. A declaration that is new, or differs from the accepted one, is held at start exactly as it would be while the mesh runs, and the log says so.

- The **first start ever** has nothing to compare with: it trusts the file, once, and records it.
- The fingerprint covers the transport, the command and its arguments, the URL, the names and values of `env`, and the names of the headers. Header values are left out, so a rotated token does not hold a server.
- A declaration held at start has nothing running in its place: mesh7 keeps the fingerprint of what you accepted, not the declaration itself.
- Without `storage_path` nothing is remembered and every start trusts the file as it is. mesh7 warns about it at start.

Three limits are worth knowing. The state database is as writable as the config file: keep both out of reach. Without `auth.admin_token`, any local process can call the control plane, `accept` included: the hold stops an agent confined to mesh tools, not one that has a shell. And policy files are applied as soon as they change, so keep `config.yaml` and `policy_dir` out of the directories your agents can write to. See [Control-plane auth](https://docs.flux7.art/mesh7/control-plane-auth/).

## What a change costs

- Calls in flight on a server being replaced or removed fail with `client closed`. Calls on the other servers are not disturbed.
- The tools of a replaced server leave the registry for the time it takes to reconnect.
- A server that does not answer is retried in the background, 5 seconds apart at first and up to 5 minutes, and shown as `retrying` with its error.

## Pinning still decides what is callable

With `pin_tools: true`, every connection is observed. A tool whose description or schema changed is held back (`pin:changed`) until you accept it; a call to it asks for approval. The tools of a server accepted for the first time are trusted on first use.

```bash
mesh7 pin list                    # tools held back: new, or changed since accepted
mesh7 pin accept --server time    # accept all the held tools of one server
```

## Commands

```bash
mesh7 upstream list               # status and number of tools per server
mesh7 upstream reload             # read mcp_servers again now, report what was removed and held
mesh7 upstream held               # the held declarations and what each would run
mesh7 upstream accept <name>      # start a held declaration
mesh7 upstream restart <name>     # close a server and connect it again as declared
```

`restart` is for a server whose declaration did not change but whose program did: a new build behind the same command. It needs no acceptance, the declaration was already accepted.

The same actions are on the Tools page of [flux7-console](https://docs.flux7.art/console/), and in the API:

| Method | Path | Description |
|---|---|---|
| `POST` | `/mcp-servers/reload` | Read `mcp_servers` again; answers `{"diff": {"removed", "held", "unchanged", ...}}`, or `422` and nothing changed |
| `GET` | `/mcp-servers/held` | The held declarations and what each would run |
| `POST` | `/mcp-servers/{name}/accept` | Start a held declaration |
| `POST` | `/mcp-servers/{name}/restart` | Reconnect one server |

All four are control-plane routes. A file change that holds or removes a declaration is traced as `mesh.upstream_file_change`, an acceptance as `mesh.upstream_accept`, the others as `mesh.upstream_reload` and `mesh.upstream_restart`.

## How clients learn about it

`GET /health` carries a `catalog` counter that moves each time an upstream comes, goes or reconnects.

- A client behind `mesh7 --mcp` (Claude Code, Cursor) is told: the stdio proxy watches the counter and sends `notifications/tools/list_changed`, so the client reads the tool list again by itself.
- A client on the HTTP export (`POST /mcp`) is not notified. It sees the new list at its next `tools/list`.

## Check before you save

```bash
mesh7 validate --config config.yaml
# ok: config.yaml
#   17 upstream server(s), 5 CLI tool(s), 4 policy(ies) with 50 rule(s)
```

`validate` loads the config and its policy files as the daemon would, without starting anything, and exits 1 with the reason when they are unsound: a misspelt action, a duplicate server name, an unknown transport.

## What still needs a restart

CLI tools (`cli_tools`), OpenAPI specs, the port, authentication and storage settings are read at start.
