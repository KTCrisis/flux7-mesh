# Session labels (lethal trifecta) deferred; stabilize the core first

- **Problem**: per-call policy cannot see a session that read private data, then untrusted content, then calls an egress tool (send_email, fetch); the composition is the attack, no single call is.
- **Decision**: no session labels for now. mesh7 stays a light per-call proxy; work goes to stabilizing the core (proxy MCP, YAML policy, human approval, traces, identity).
- **Why**: labels, session state, unlabelled defaults, catalogue gating and supervisor hand-off turn a proxy into an information-flow system; the risk is real mainly for unattended agents, and no client asks yet.
- **Where**: design notes kept in Claude memory (`project_mesh7_session_labels`); mitigation without code: recipient allowlist on `gmail_send_email` with `ends_with`. Revisit when a client or demo needs it.
