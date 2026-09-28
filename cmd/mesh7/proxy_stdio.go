package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	writer := bufio.NewWriter(out)

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

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("proxy: daemon unreachable: %w", err)
		}

		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			sessionID = sid
		}

		if resp.StatusCode == http.StatusAccepted {
			resp.Body.Close()
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("proxy: read response: %w", err)
		}

		_, _ = writer.Write(bytes.TrimRight(body, "\r\n"))
		_ = writer.WriteByte('\n')
		_ = writer.Flush()
	}

	if sessionID != "" {
		req, _ := http.NewRequest("DELETE", base+"/mcp", nil)
		req.Header.Set("Mcp-Session-Id", sessionID)
		client.Do(req)
	}
	return scanner.Err()
}
