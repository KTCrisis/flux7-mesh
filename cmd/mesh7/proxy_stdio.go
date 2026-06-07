package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func daemonRunning(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
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
