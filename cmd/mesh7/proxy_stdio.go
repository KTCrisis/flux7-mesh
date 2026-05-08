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
	slog.Info("daemon detected — proxying stdio to HTTP", "port", port, "agent", agentID)
	base := fmt.Sprintf("http://localhost:%d", port)
	return runStdioProxyWith(base, agentID, os.Stdin, os.Stdout)
}

func runStdioProxyWith(base string, agentID string, in io.Reader, out io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	var sessionID string

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
		req.Header.Set("Authorization", "Bearer agent:"+agentID)
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
