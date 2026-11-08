---
name: catalog
description: Show all flux7-mesh tools organized by source/category with policy actions
user-invocable: true
argument-hint: "[source]"
allowed-tools:
  - mcp__mesh7__mesh_catalog
---

Show all tools available through flux7-mesh, grouped by source with policy actions.

Arguments:
- $0 = source filter (optional, e.g. "filesystem", "git", "gmail"). Omit to show all.

Call the `mesh.catalog` MCP tool with:
- If $0 is provided: `{"source": "$0"}`
- Otherwise: `{}`

Display the result as a formatted table with columns: Source, Type, Runtime, Count, Policy summary.
- Type = the "source" field from each group (mcp, cli, virtual)
- Runtime = the "runtime" field from each group (command+args, URL, or binary path); leave the column out when no group carries one
- For each group, summarize the policy actions (e.g. "read: allow, write: approval").

The catalogue is scoped to the calling agent: tools it can never call and runtime details are shown to supervisor agents only. Say so in one line under the table when the runtime is absent; the full view is `GET /tools` on the mesh HTTP port (admin) or the Tools page of flux7-console.
