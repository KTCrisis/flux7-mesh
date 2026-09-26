# Trace integrity

mesh7 writes one JSONL line per tool call to `trace_file`. Each line is
chained to the previous one, so an edited, deleted or inserted line is
detected.

## Line format

The entry's own JSON is followed by four keys:

```json
{"trace_id":"…","tool":"pay","policy":"human_approval",…,"seq":42,"alg":"hmac-sha256","prev_hash":"ab…","hash":"cd…"}
```

`hash = HMAC-SHA256(key, "<seq>\n<alg>\n<prev_hash>\n" + <entry JSON as written>)`

The entry bytes are hashed as they sit on disk, never re-encoded. `seq` and
`alg` are covered, so lines cannot be renumbered and a chain cannot be
downgraded. The chain carries on across restarts and rotations
(`traces.jsonl.old` → `traces.jsonl`).

Updates (approval outcome, backend status, latency) are appended as new
lines with a higher `revision`, never rewritten in place. On load, the
highest revision of a trace ID wins.

## Key

| `MESH_TRACE_KEY` | alg | What it protects against |
|---|---|---|
| unset | `sha256` | an edited or deleted line in the middle of the file. Someone with write access can recompute every hash after their edit. |
| set | `hmac-sha256` | the same, and rewriting the chain: without the key the hashes cannot be recomputed. |

Keep the key out of the config file, in an environment file readable by the
service only (a systemd `EnvironmentFile` with mode 600, for instance).
Losing it does not lose the traces, only the ability to prove them.

## Verify

```bash
mesh7 trace verify data/traces.jsonl.old data/traces.jsonl   # oldest first
mesh7 trace verify --json data/traces.jsonl                  # machine-readable
```

Exit code 0 when the chain holds, 1 at the first break (file, line, seq,
reason), 2 on usage or I/O error. With `MESH_TRACE_KEY` set, every chained
line must be HMAC. Lines written before the chain existed are counted as
`unchained` when they precede it, and are a break when they appear inside it.

## Limits

- **Truncated tail.** Deleting the last lines leaves a valid, shorter chain.
  mesh7 logs `seq` and `head` at startup and shutdown (`trace store ready`,
  `trace chain closed`); compare the verified head with the last one in the
  service log (`journalctl -u mesh7`). Exporting the head to an external
  collector is the next step.
- **Rotation keeps one old file.** `traces.jsonl.old` is replaced at the next
  rotation; the current file then verifies from its `anchor` (the last hash
  of the file that is gone). Archive `.old` files if history matters.
- **One writer per file.** Two mesh7 processes appending to the same
  `trace_file` (for instance `mesh7 --mcp` started without the daemon while
  the daemon runs with the same config) fork the chain, and verify reports a
  `prev_hash` break that is not tampering. Give each process its own file.
