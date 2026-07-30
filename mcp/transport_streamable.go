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

// streamableTransport speaks the MCP Streamable HTTP transport to an upstream
// server: every JSON-RPC request is a POST to a single endpoint.
//
// It differs from SSE in where the answer arrives. With SSE the client holds a
// long-lived stream and POSTs go out on the side; here the reply comes back in
// the POST's own HTTP response — either as one JSON object, or as an SSE
// stream when the server has several messages to send for that request.
//
// The transport interface assumes a reader loop fed by the connection, so
// WriteRequest pushes whatever it decodes into an internal channel and
// ReadLoop drains it. From the client's point of view nothing changes.
//
// Note this is the mirror image of mcp/http_server.go, which serves the same
// protocol to the mesh's own clients. They share no code and run in opposite
// directions.
type streamableTransport struct {
	name    string
	url     string
	headers map[string]string

	client *http.Client

	// sessionID is handed out by the server on initialize (Mcp-Session-Id) and
	// must be echoed on every later request. Servers that do not use sessions
	// simply never set it.
	sessionID string
	sessionMu sync.RWMutex

	incoming  chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func newStreamableTransport(name, endpoint string, headers map[string]string) *streamableTransport {
	return &streamableTransport{
		name:    name,
		url:     endpoint,
		headers: headers,
		client: &http.Client{
			Timeout: 5 * time.Minute,
			// A redirect to another host would quietly hand every tool call —
			// arguments, tokens, results — to a third party, or turn the mesh
			// into an SSRF relay. Same-origin redirects only.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) == 0 {
					return nil
				}
				if !sameOrigin(via[0].URL.String(), req.URL.String()) {
					return fmt.Errorf("cross-origin redirect refused: %s -> %s",
						via[0].URL.Host, req.URL.Host)
				}
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		incoming: make(chan []byte, 64),
		closed:   make(chan struct{}),
	}
}

// Start validates the endpoint. There is no connection to open: the session,
// if the server wants one, is created by the initialize POST that follows.
func (t *streamableTransport) Start() error {
	u, err := url.Parse(t.url)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint must be http or https, got %q", u.Scheme)
	}
	slog.Info("MCP client: streamable HTTP ready", "server", t.name, "url", t.url)
	return nil
}

func (t *streamableTransport) WriteRequest(data []byte) error {
	req, err := http.NewRequest("POST", t.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Both are advertised: the server picks one message or a stream of them.
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.sessionMu.RLock()
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}
	t.sessionMu.RUnlock()

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("streamable HTTP POST: %w", err)
	}

	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.sessionMu.Lock()
		if t.sessionID != id {
			t.sessionID = id
			slog.Debug("MCP client: session established", "server", t.name)
		}
		t.sessionMu.Unlock()
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return fmt.Errorf("streamable HTTP POST: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// 202 Accepted with no body is the legal answer to a notification.
	if resp.StatusCode == http.StatusAccepted {
		resp.Body.Close()
		return nil
	}

	ctype := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ctype, "text/event-stream"):
		// Several messages may follow for this one request; read them in the
		// background so WriteRequest does not block the caller until the
		// server is done talking.
		go t.consumeSSE(resp.Body)
	default:
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("streamable HTTP read body: %w", err)
		}
		if len(bytes.TrimSpace(body)) > 0 {
			t.deliver(body)
		}
	}
	return nil
}

// consumeSSE reads an event-stream response and delivers each data payload.
func (t *streamableTransport) consumeSSE(body io.ReadCloser) {
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		if strings.TrimSpace(payload) != "" {
			t.deliver([]byte(payload))
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// event:, id:, retry: and comments carry nothing we need here.
	}
	flush()

	if err := scanner.Err(); err != nil && err != io.EOF {
		slog.Error("MCP client: streamable HTTP stream error", "server", t.name, "error", err)
	}
}

// deliver hands a message to ReadLoop, unless the transport is closing.
func (t *streamableTransport) deliver(msg []byte) {
	select {
	case t.incoming <- msg:
	case <-t.closed:
	}
}

func (t *streamableTransport) ReadLoop(onMessage func([]byte)) {
	for {
		select {
		case msg := <-t.incoming:
			onMessage(msg)
		case <-t.closed:
			return
		}
	}
}

// Close ends the session server-side. A DELETE is best-effort: servers that do
// not track sessions answer 405, which is not an error worth reporting.
func (t *streamableTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })

	t.sessionMu.RLock()
	id := t.sessionID
	t.sessionMu.RUnlock()
	if id == "" {
		return nil
	}

	req, err := http.NewRequest("DELETE", t.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", id)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("MCP client: session delete failed", "server", t.name, "error", err)
		return nil
	}
	resp.Body.Close()
	return nil
}
