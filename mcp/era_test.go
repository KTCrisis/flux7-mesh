package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/internal/callmeta"
)

// strictServer is a 2026-07-28-only MCP server, modelled on the official
// TypeScript SDK with legacy: 'reject' (docs/internal/capture-mcp-2026-07-28-client.log):
// initialize is refused with -32022, and every request must carry the three
// headers and the per-request envelope, headers and body agreeing.
type strictServer struct {
	callResult map[string]any // what tools/call answers

	mu       sync.Mutex
	methods  []string       // methods received, in order
	lastMeta map[string]any // _meta of the last tools/call
	sessions int            // Mcp-Session-Id headers seen
}

func (s *strictServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad body: %s", body)
			return
		}
		s.mu.Lock()
		s.methods = append(s.methods, req.Method)
		if r.Header.Get("Mcp-Session-Id") != "" {
			s.sessions++
		}
		s.mu.Unlock()

		fail := func(status, code int, msg string, data any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": code, "message": msg, "data": data}})
		}
		if req.Method == "initialize" {
			fail(400, -32022, "Unsupported protocol version: 2024-11-05",
				map[string]any{"supported": []string{"2026-07-28"}, "requested": "2024-11-05"})
			return
		}
		if r.Header.Get("MCP-Protocol-Version") != "2026-07-28" {
			fail(400, -32020, "MCP-Protocol-Version header absent", nil)
			return
		}
		if r.Header.Get("Mcp-Method") != req.Method {
			fail(400, -32020, "Mcp-Method header and body disagree", nil)
			return
		}
		if req.Method == "tools/call" && r.Header.Get("Mcp-Name") != req.Params["name"] {
			fail(400, -32020, "Mcp-Name header and body disagree", nil)
			return
		}
		meta, _ := req.Params["_meta"].(map[string]any)
		if meta[metaProtocolVersion] != "2026-07-28" || meta[metaClientInfo] == nil || meta[metaClientCapabilities] == nil {
			fail(400, -32602, "missing per-request envelope", nil)
			return
		}

		serverMeta := map[string]any{metaServerInfo: map[string]any{"name": "strict"}}
		var result map[string]any
		switch req.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}},
				"resultType": "complete", "ttlMs": 0, "cacheScope": "private", "_meta": serverMeta}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "ping", "inputSchema": map[string]any{"type": "object"}}},
				"resultType": "complete", "_meta": serverMeta}
		case "tools/call":
			s.mu.Lock()
			s.lastMeta = meta
			s.mu.Unlock()
			result = s.callResult
		default:
			fail(400, -32601, "method not found", nil)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

func connectStrict(t *testing.T, s *strictServer) *MCPClient {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	c := NewStreamableHTTPClient("strict", srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A server that speaks only 2026-07-28 is reached: initialize is refused,
// the client switches era, confirms it with server/discover, lists and calls
// tools with headers and envelope, and never holds a session.
func TestModernOnlyUpstream(t *testing.T) {
	s := &strictServer{callResult: map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "pong"}},
		"structuredContent": map[string]any{"ok": true},
		"resultType":        "complete",
		"_meta":             map[string]any{metaServerInfo: map[string]any{"name": "strict"}},
	}}
	c := connectStrict(t, s)

	if tools := c.Tools(); len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools = %+v", tools)
	}
	ctx := callmeta.With(context.Background(), map[string]any{
		callmeta.Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	})
	got, err := c.CallTool(ctx, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	// Modern-only fields stop at mesh7, which serves its clients the legacy era.
	want := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "pong"}},
		"structuredContent": map[string]any{"ok": true},
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Errorf("result = %s, want %s", gj, wj)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	wantMethods := []string{"initialize", "server/discover", "tools/list", "tools/call"}
	if len(s.methods) != len(wantMethods) {
		t.Fatalf("methods = %v, want %v", s.methods, wantMethods)
	}
	for i := range wantMethods {
		if s.methods[i] != wantMethods[i] {
			t.Errorf("methods = %v, want %v", s.methods, wantMethods)
			break
		}
	}
	if s.sessions != 0 {
		t.Errorf("Mcp-Session-Id sent %d times to a sessionless server", s.sessions)
	}
	// The trace context travels alongside the envelope.
	if s.lastMeta[callmeta.Traceparent] == nil {
		t.Errorf("traceparent lost from _meta: %v", s.lastMeta)
	}
}

// A result that is not complete (the server asks for more input first) is
// refused, not passed off as an answer.
func TestModernIncompleteResultIsAnError(t *testing.T) {
	s := &strictServer{callResult: map[string]any{
		"resultType": "input_required",
		"content":    []any{},
	}}
	c := connectStrict(t, s)
	if _, err := c.CallTool(context.Background(), "ping", map[string]any{}); err == nil {
		t.Fatal("input_required accepted as a result")
	}
}

// offersModern reads the refusal both from an HTTP error body (streamable
// HTTP) and from a JSON-RPC error response (stdio), and only when the server
// offers 2026-07-28.
func TestOffersModern(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"x","data":{"supported":["2026-07-28"]}}}`)
	if !offersModern(errors.Join(errors.New("write initialize"), &httpStatusError{Status: 400, Body: body}), rpcResponse{}) {
		t.Error("HTTP refusal offering 2026-07-28 not recognized")
	}
	stdio := rpcResponse{Error: &rpcError{Code: -32022, Data: map[string]any{"supported": []any{"2026-07-28"}}}}
	if !offersModern(nil, stdio) {
		t.Error("JSON-RPC refusal offering 2026-07-28 not recognized")
	}
	other := rpcResponse{Error: &rpcError{Code: -32022, Data: map[string]any{"supported": []any{"2099-01-01"}}}}
	if offersModern(nil, other) {
		t.Error("refusal without 2026-07-28 taken as modern")
	}
	if offersModern(&httpStatusError{Status: 500, Body: []byte("boom")}, rpcResponse{}) {
		t.Error("plain HTTP failure taken as modern")
	}
}

// Headers come from the body, so they cannot disagree with it.
func TestModernHeaders(t *testing.T) {
	h := modernHeaders([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ping"}}`))
	if h["MCP-Protocol-Version"] != "2026-07-28" || h["Mcp-Method"] != "tools/call" || h["Mcp-Name"] != "ping" {
		t.Errorf("headers = %v", h)
	}
	if h := modernHeaders([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); h["Mcp-Name"] != "" {
		t.Errorf("tools/list carries Mcp-Name: %v", h)
	}
}
