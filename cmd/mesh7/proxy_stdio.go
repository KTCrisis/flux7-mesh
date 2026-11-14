package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// configID names a config file without disclosing it: a short hash of its
// absolute path. Two processes serve the same config when their IDs match.
// Empty for an empty path (mesh7 started from --openapi alone).
func configID(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

// daemonOnPort reports whether a mesh7 daemon answers on port, and the ID of
// the config it serves ("" when the daemon predates the field).
func daemonOnPort(port int) (running bool, config string) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false, ""
	}
	var h struct {
		Config string `json:"config"`
	}
	json.NewDecoder(resp.Body).Decode(&h)
	return true, h.Config
}

// proxyDecision says whether `mesh7 --mcp` may relay to the daemon it found.
// It refuses a daemon serving another config: relaying would hand this
// project's agent that daemon's tools and policy, silently. Running in
// process instead would not work either: the approval API port is taken,
// and `mesh approve` would reach the other process.
func proxyDecision(mine, theirs string, port int) (ok bool, warn string, err error) {
	switch {
	case theirs == "":
		return true, "daemon does not report its config (older than v0.17.1): relaying without checking it serves this one", nil
	case theirs == mine:
		return true, "", nil
	default:
		return false, "", fmt.Errorf("a mesh7 daemon on port %d serves another config; not relaying to it. "+
			"Give this config its own `port:`, or stop that daemon", port)
	}
}

func runStdioProxy(port int, agentID string) error {
	// A pre-issued token (JWT) takes precedence over the legacy agent: identity.
	// Required when the daemon runs with JWT auth (strict mode rejects agent:).
	token := os.Getenv("MESH_AGENT_TOKEN")
	slog.Info("daemon detected — proxying stdio to HTTP", "port", port, "agent", agentID, "token", token != "")
	base := fmt.Sprintf("http://localhost:%d", port)
	return runStdioProxyWith(base, agentID, token, os.Stdin, os.Stdout)
}

// clientSessionLabel is the conversation this bridge serves, as the client
// that spawned it names it: MESH7_SESSION_LABEL if set, else the session id
// Claude Code puts in its MCP servers' environment. Sent as X-Session-Id, it
// lands in each trace as client_session, so a reader can join mesh7's traces
// to the client's own transcript. A /clear in Claude Code starts a new session
// without restarting the bridge: the label then names the session it started
// with.
func clientSessionLabel() string {
	if v := os.Getenv("MESH7_SESSION_LABEL"); v != "" {
		return v
	}
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}

func runStdioProxyWith(base, agentID, token string, in io.Reader, out io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	var sessionID string

	// With a token, present it as the Bearer credential (validated by the daemon
	// as a JWT). Without one, fall back to the legacy self-declared agent: form,
	// which only the daemon's non-JWT / allow_legacy modes accept.
	authHeader := "Bearer agent:" + agentID
	if token != "" {
		authHeader = "Bearer " + token
	}

	label := clientSessionLabel()

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	writer := bufio.NewWriter(out)

	// Two writers share stdout: the answers below and the catalogue watcher.
	var wmu sync.Mutex
	writeLine := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = writer.Write(bytes.TrimRight(b, "\r\n"))
		_ = writer.WriteByte('\n')
		_ = writer.Flush()
	}

	// The daemon cannot push to a client it only answers. This process can:
	// it watches the daemon's catalogue counter and tells the client when the
	// tool list is worth reading again (an upstream added, changed or removed).
	var initialized atomic.Bool
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchCatalog(watchCtx, base, func() {
			if initialized.Load() {
				writeLine([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
			}
		})
	}()
	// nothing is written to out once this function has returned
	defer func() { stopWatch(); <-watchDone }()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		req, err := http.NewRequest("POST", base+"/mcp", bytes.NewReader(line))
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", authHeader)
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
		}
		if label != "" {
			req.Header.Set("X-Session-Id", label)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("proxy: daemon unreachable: %w", err)
		}

		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			sessionID = sid
		}

		if resp.StatusCode == http.StatusAccepted {
			resp.Body.Close()
			if bytes.Contains(line, []byte(`"notifications/initialized"`)) {
				initialized.Store(true)
			}
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("proxy: read response: %w", err)
		}

		writeLine(body)
	}

	if sessionID != "" {
		req, _ := http.NewRequest("DELETE", base+"/mcp", nil)
		req.Header.Set("Mcp-Session-Id", sessionID)
		client.Do(req)
	}
	return scanner.Err()
}

// catalogPollInterval is how often a stdio proxy asks the daemon whether its
// tool catalogue moved.
var catalogPollInterval = 3 * time.Second

// watchCatalog calls onChange each time the daemon's catalogue counter
// (/health, "catalog") differs from the value last read, until ctx ends. The
// first value read, one interval after the start, is the baseline; a daemon
// that does not answer is simply asked again.
func watchCatalog(ctx context.Context, base string, onChange func()) {
	client := &http.Client{Timeout: 2 * time.Second}
	read := func() (uint64, bool) {
		req, err := http.NewRequestWithContext(ctx, "GET", base+"/health", nil)
		if err != nil {
			return 0, false
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, false
		}
		defer resp.Body.Close()
		var h struct {
			Catalog *uint64 `json:"catalog"`
		}
		if json.NewDecoder(resp.Body).Decode(&h) != nil || h.Catalog == nil {
			return 0, false // a daemon older than the counter: nothing to watch
		}
		return *h.Catalog, true
	}
	var last uint64
	known := false
	ticker := time.NewTicker(catalogPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur, ok := read()
		if !ok || ctx.Err() != nil {
			continue
		}
		if known && cur != last {
			onChange()
		}
		last, known = cur, true
	}
}
