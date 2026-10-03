package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/internal/callmeta"
)

// upstream answers the handshake and records the params of each tools/call.
func upstream(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "t"}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "tools/call":
			mu.Lock()
			calls = append(calls, req.Params)
			mu.Unlock()
			result = map[string]any{"content": []any{}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}))
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), calls...)
	}
}

func connected(t *testing.T, url string, forwardIdentity bool) *MCPClient {
	t.Helper()
	c := NewStreamableHTTPClient("memory", url, nil)
	c.ForwardIdentity = forwardIdentity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallToolSendsMeta(t *testing.T) {
	srv, calls := upstream(t)
	defer srv.Close()
	meta := map[string]any{
		callmeta.Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		callmeta.Agent:       "scout7",
	}
	ctx := callmeta.With(context.Background(), meta)

	if _, err := connected(t, srv.URL, true).CallTool(ctx, "memory_store", map[string]any{"key": "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := connected(t, srv.URL, false).CallTool(ctx, "memory_store", map[string]any{"key": "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := connected(t, srv.URL, true).CallTool(context.Background(), "memory_store", nil); err != nil {
		t.Fatal(err)
	}

	got := calls()
	if len(got) != 3 {
		t.Fatalf("calls: %d", len(got))
	}
	m0, _ := got[0]["_meta"].(map[string]any)
	if m0[callmeta.Agent] != "scout7" || m0[callmeta.Traceparent] == nil {
		t.Errorf("opted-in upstream: _meta = %v", got[0]["_meta"])
	}
	m1, _ := got[1]["_meta"].(map[string]any)
	if _, leaked := m1[callmeta.Agent]; leaked || m1[callmeta.Traceparent] == nil {
		t.Errorf("upstream without forward_identity: _meta = %v (trace kept, agent dropped)", got[1]["_meta"])
	}
	if _, ok := got[2]["_meta"]; ok {
		t.Errorf("no metadata, no _meta field: %v", got[2])
	}
}
