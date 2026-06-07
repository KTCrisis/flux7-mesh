package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestResolveRelativeURL(t *testing.T) {
	tests := []struct {
		base     string
		relative string
		want     string
	}{
		{"https://example.com/sse", "/messages", "https://example.com/messages"},
		{"https://example.com/path/sse", "/messages", "https://example.com/messages"},
		{"http://localhost:8080/sse", "/rpc", "http://localhost:8080/rpc"},
		{"https://example.com", "/messages", "https://example.com/messages"},
	}
	for _, tt := range tests {
		got := resolveRelativeURL(tt.base, tt.relative)
		if got != tt.want {
			t.Errorf("resolveRelativeURL(%q, %q) = %q, want %q", tt.base, tt.relative, got, tt.want)
		}
	}
}

func TestSSETransportIntegration(t *testing.T) {
	// Mock SSE MCP server
	var postMu sync.Mutex
	var lastPostBody []byte

	mux := http.NewServeMux()

	// SSE endpoint — sends endpoint event then streams responses
	responseCh := make(chan rpcResponse, 10)

	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", 500)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)

		// Send endpoint event
		fmt.Fprintf(w, "event: endpoint\ndata: /messages\n\n")
		flusher.Flush()

		// Stream responses
		for resp := range responseCh {
			data, _ := json.Marshal(resp)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
			flusher.Flush()
		}
	})

	// POST endpoint for JSON-RPC requests
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		r.Body.Close()

		postMu.Lock()
		lastPostBody = body
		postMu.Unlock()

		// Parse request and send response via SSE
		var req rpcRequest
		json.Unmarshal(body, &req)

		switch req.Method {
		case "initialize":
			responseCh <- rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "test-sse-server", "version": "1.0"},
				},
			}
		case "tools/list":
			responseCh <- rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"tools": []map[string]any{
						{
							"name":        "echo",
							"description": "Echo tool",
							"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
						},
					},
				},
			}
		case "tools/call":
			responseCh <- rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": "echoed"},
					},
				},
			}
		}

		w.WriteHeader(202)
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	defer close(responseCh)

	// Create SSE client and connect
	client := NewSSEClient("test-sse", server.URL+"/sse", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Wait a moment for SSE to establish
	time.Sleep(100 * time.Millisecond)

	err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	// Check status
	status, _ := client.Status()
	if status != "ready" {
		t.Errorf("status = %q, want ready", status)
	}

	// Check discovered tools
	tools := client.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want echo", tools[0].Name)
	}

	// Call a tool
	result, err := client.CallTool(ctx, "echo", map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}

	// Verify POST body was received
	postMu.Lock()
	if len(lastPostBody) == 0 {
		t.Error("no POST body received")
	}
	postMu.Unlock()
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://localhost:9070/sse", "http://localhost:9070/messages", true},
		{"https://mcp.internal/sse", "https://mcp.internal/rpc", true},
		{"http://localhost:9070/sse", "http://evil.com/steal", false},
		{"http://localhost:9070/sse", "http://localhost:9999/messages", false},  // diff port
		{"http://localhost:9070/sse", "https://localhost:9070/messages", false}, // diff scheme
		{"http://localhost:9070/sse", "://garbage", false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.a, c.b); got != c.want {
			t.Errorf("sameOrigin(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestSSEEndpointForeignOriginIgnored(t *testing.T) {
	tr := newSSETransport("evil", "http://localhost:9070/sse", nil)
	// A malicious upstream tries to redirect POSTs to an attacker host.
	tr.handleSSEEvent("endpoint", "http://attacker.example/exfil", func([]byte) {})
	tr.postMu.Lock()
	got := tr.postURL
	tr.postMu.Unlock()
	if got != "" {
		t.Fatalf("foreign-origin endpoint should be ignored, postURL=%q", got)
	}

	// A same-origin (relative) endpoint is accepted.
	tr.handleSSEEvent("endpoint", "/messages?id=1", func([]byte) {})
	tr.postMu.Lock()
	got = tr.postURL
	tr.postMu.Unlock()
	if got != "http://localhost:9070/messages?id=1" {
		t.Fatalf("same-origin endpoint should be accepted, got %q", got)
	}
}
