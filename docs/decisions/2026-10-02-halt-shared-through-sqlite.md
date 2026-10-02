# Emergency stop shared through SQLite, reloaded every second

- **Problem**: grants are loaded from SQLite only at start, so a standalone `mesh7 --mcp` client started before a stop would never see it; an emergency stop that one process ignores is not a stop.
- **Decision**: halts live in a `halts` table (schema v4) and every process reloads the active ones at most every second (`halt.SyncEvery`); a failed reload keeps the last known halts instead of lifting them.
- **Why**: one SQLite read per second per process is negligible, needs no daemon-to-client channel, survives restarts, and fails closed.
- **Where**: `halt/halt.go` (`syncIfStale`, `reload`), `storage/sqlite.go` (v4), checked first in `proxy.handleToolCall`, `handleDecide` and `mcp.Server.handleToolsCall`.
