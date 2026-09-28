# The stdio auto-proxy relays only to a daemon serving the same config

- **Problem**: `mesh7 --mcp --config other.yaml` relayed to any daemon answering on the config's port, so a second project without its own `port:` silently got the first daemon's tools and policy.
- **Decision**: `/health` reports a config ID (a short hash of the config file's absolute path, symlinks resolved); the client compares it with its own and exits with an error naming the way out (its own `port:`, or stop that daemon) when they differ; a daemon without the field is relayed to with a warning.
- **Why**: running in process instead would not work (the approval port is taken, `mesh approve` would reach the other process); a hash identifies the config without disclosing its path on an unauthenticated route.
- **Where**: `cmd/mesh7/proxy_stdio.go` (`configID`, `proxyDecision`), `proxy/handler.go` (`ConfigID`); v0.17.1.
