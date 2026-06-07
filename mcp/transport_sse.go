package mcp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type sseTransport struct {
	name    string
	sseURL  string
	headers map[string]string

	postURL   string // discovered from SSE "endpoint" event
	postMu    sync.RWMutex
	postReady chan struct{} // closed when postURL is discovered

	client     *http.Client   // SSE stream (no timeout)
	postClient *http.Client   // POST requests (30s timeout)
	resp       *http.Response // SSE connection response (kept open)
}

func newSSETransport(name, sseURL string, headers map[string]string) *sseTransport {
	return &sseTransport{
		name:       name,
		sseURL:     sseURL,
		headers:    headers,
		postReady:  make(chan struct{}),
		client:     &http.Client{Timeout: 0},                // no timeout for SSE stream
		postClient: &http.Client{Timeout: 30 * time.Second}, // POST requests
	}
}

func (t *sseTransport) Start() error {
	req, err := http.NewRequest("GET", t.sseURL, nil)
	if err != nil {
		return fmt.Errorf("build SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}

	t.resp, err = t.client.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect: %w", err)
	}
	if t.resp.StatusCode != 200 {
		t.resp.Body.Close()
		return fmt.Errorf("SSE connect: status %d", t.resp.StatusCode)
	}

	slog.Info("MCP client: SSE connected", "server", t.name, "url", t.sseURL)
	return nil
}

func (t *sseTransport) WriteRequest(data []byte) error {
	// Wait for the endpoint event (with timeout)
	select {
	case <-t.postReady:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("SSE endpoint not discovered within timeout")
	}

	t.postMu.RLock()
	postURL := t.postURL
	t.postMu.RUnlock()

	req, err := http.NewRequest("POST", postURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}

	resp, err := t.postClient.Do(req)
	if err != nil {
		return fmt.Errorf("SSE POST: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("SSE POST: status %d", resp.StatusCode)
	}
	return nil
}

func (t *sseTransport) ReadLoop(onMessage func([]byte)) {
	scanner := bufio.NewScanner(t.resp.Body)
	// SSE can have large data fields
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				t.handleSSEEvent(eventType, data, onMessage)
			}
			eventType = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// Ignore id:, retry:, comments (:)
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		slog.Error("MCP client: SSE read error", "server", t.name, "error", err)
	}
}

func (t *sseTransport) handleSSEEvent(eventType, data string, onMessage func([]byte)) {
	switch eventType {
	case "endpoint":
		// The server tells us where to POST JSON-RPC requests.
		postURL := strings.TrimSpace(data)
		if !strings.HasPrefix(postURL, "http") {
			postURL = resolveRelativeURL(t.sseURL, postURL)
		}
		// A compromised or malicious upstream could point the POST endpoint at
		// an arbitrary host — exfiltrating every tool call (params, secrets) or
		// turning the mesh into an SSRF relay. Only accept an endpoint that
		// shares the origin of the SSE connection we opened.
		if !sameOrigin(t.sseURL, postURL) {
			slog.Warn("MCP client: SSE endpoint origin mismatch — ignoring",
				"server", t.name, "sse_url", t.sseURL, "endpoint", postURL)
			return
		}
		t.postMu.Lock()
		alreadySet := t.postURL != ""
		t.postURL = postURL
		t.postMu.Unlock()
		if !alreadySet {
			close(t.postReady)
		}
		slog.Info("MCP client: SSE endpoint discovered", "server", t.name, "postURL", postURL)

	case "message", "":
		// JSON-RPC response
		if len(data) > 0 {
			onMessage([]byte(data))
		}

	default:
		slog.Debug("MCP client: SSE unknown event", "server", t.name, "type", eventType)
	}
}

func (t *sseTransport) Close() error {
	if t.resp != nil && t.resp.Body != nil {
		t.resp.Body.Close()
	}
	return nil
}

// sameOrigin reports whether two URLs share scheme, host and port. url.Host
// includes the port, so comparing scheme + host covers all three. A parse
// failure on either side is treated as a mismatch (fail closed).
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// resolveRelativeURL resolves a relative path against an SSE URL base.
func resolveRelativeURL(base, relative string) string {
	// Find the scheme + host from the base URL
	idx := strings.Index(base, "://")
	if idx == -1 {
		return relative
	}
	hostEnd := strings.Index(base[idx+3:], "/")
	if hostEnd == -1 {
		return base + relative
	}
	return base[:idx+3+hostEnd] + relative
}
