---
name: catalog
description: Show all flux7-mesh tools organized by source/category with policy actions
user-invocable: true
argument-hint: "[source]"
allowed-tools:
  - mcp__mesh7__mesh.catalog
---

Show all tools available through flux7-mesh, grouped by source with policy actions.

Arguments:
- $0 = source filter (optional, e.g. "filesystem", "git", "gmail"). Omit to show all.

Call the `mesh.catalog` MCP tool with:
- If $0 is provided: `{"source": "$0"}`
- Otherwise: `{}`

Display the result as a formatted table with columns: Source, Count, Policy summary.
For each group, summarize the policy actions (e.g. "read: allow, write: approval, delete: deny").
