package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

func testHTTPHandler() *HTTPHandler {
	reg := registry.New()
	reg.LoadManual(&registry.Tool{
		Name: "echo", Description: "Echo back input", Source: "openapi",
		Params: []registry.Param{{Name: "msg", In: "body", Type: "string", Required: true}},
	})

	pol := policy.NewEngine([]config.Policy{
		{Name: "allow-all", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"*"}, Action: "allow"},
		}},
	})

	traces := trace.NewStore(100)
	approvals := approval.NewStore(30)
	handler := proxy.NewHandler(reg, pol, traces)
	handler.Approvals = approvals

	return NewHTTPHandler(reg, pol, traces, approvals, handler, nil, false, nil)
}

func postMCP(h *HTTPHandler, sessionID string, req rpcRequest) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		r.Header.Set("Mcp-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func postMCPWithAgent(h *HTTPHandler, sessionID, agentID string, req rpcRequest) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		r.Header.Set("Mcp-Session-Id", sessionID)
	}
	if agentID != "" {
		r.Header.Set("Authorization", "Bearer agent:"+agentID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHTTPInitialize(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "", rpcRequest{
		JSONRPC: "2.0",
		ID:      float64(1),
		Method:  "initialize",
	})

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	sessionID := w.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("expected Mcp-Session-Id in response header")
	}

	var resp rpcResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	if result["protocolVersion"] == nil {
		t.Fatal("expected protocolVersion in result")
	}
}

func TestHTTPToolsList(t *testing.T) {
	h := testHTTPHandler()

	// Initialize first
	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sessionID := w.Header().Get("Mcp-Session-Id")

	// List tools
	w = postMCP(h, sessionID, rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/list"})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp rpcResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}

	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	found := false
	for _, tool := range tools {
		tm := tool.(map[string]any)
		if tm["name"] == "echo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'echo' tool in list")
	}
}

func TestHTTPNotification202(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sessionID := w.Header().Get("Mcp-Session-Id")

	w = postMCP(h, sessionID, rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if w.Code != 202 {
		t.Fatalf("expected 202 for notification, got %d", w.Code)
	}
}

func TestHTTPMissingSessionID(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"})
	if w.Code != 400 {
		t.Fatalf("expected 400 for missing session ID on non-init, got %d", w.Code)
	}
}

func TestHTTPUnknownSessionID(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "nonexistent-session", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"})
	if w.Code != 404 {
		t.Fatalf("expected 404 for unknown session, got %d", w.Code)
	}
}

func TestHTTPDeleteSession(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sessionID := w.Header().Get("Mcp-Session-Id")

	// Delete
	r := httptest.NewRequest("DELETE", "/mcp", nil)
	r.Header.Set("Mcp-Session-Id", sessionID)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("expected 204, got %d", w.Code)
	}

	// Subsequent request should 404
	w = postMCP(h, sessionID, rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/list"})
	if w.Code != 404 {
		t.Fatalf("expected 404 after delete, got %d", w.Code)
	}
}

func TestHTTPAgentExtraction(t *testing.T) {
	h := testHTTPHandler()

	w := postMCPWithAgent(h, "", "test-reviewer", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sessionID := w.Header().Get("Mcp-Session-Id")

	h.mu.Lock()
	srv := h.sessions[sessionID]
	h.mu.Unlock()

	if srv.AgentID != "test-reviewer" {
		t.Fatalf("expected agent ID 'test-reviewer', got '%s'", srv.AgentID)
	}
}

func TestHTTPPing(t *testing.T) {
	h := testHTTPHandler()

	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sessionID := w.Header().Get("Mcp-Session-Id")

	w = postMCP(h, sessionID, rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "ping"})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp rpcResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

func TestHTTPMethodNotAllowed(t *testing.T) {
	h := testHTTPHandler()
	r := httptest.NewRequest("PUT", "/mcp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestHTTPDeleteUnknownSession(t *testing.T) {
	h := testHTTPHandler()
	r := httptest.NewRequest("DELETE", "/mcp", nil)
	r.Header.Set("Mcp-Session-Id", "nope")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHTTPSessionFixationRejected(t *testing.T) {
	h := testHTTPHandler()

	// Attacker initializes with a pre-chosen session ID.
	attackerID := "attacker-fixed-id"
	w := postMCP(h, attackerID, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	if w.Code != 200 {
		t.Fatalf("initialize failed: %d", w.Code)
	}
	got := w.Header().Get("Mcp-Session-Id")
	if got == attackerID {
		t.Fatal("server adopted the client-supplied session ID — fixation possible")
	}
	if got == "" {
		t.Fatal("expected a server-minted session ID")
	}

	// The attacker's chosen ID must NOT resolve to a session.
	h.mu.Lock()
	_, exists := h.sessions[attackerID]
	h.mu.Unlock()
	if exists {
		t.Fatal("client-supplied session ID was registered — fixation hole")
	}

	// A later request using the attacker's ID is rejected as unknown.
	w = postMCP(h, attackerID, rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/list"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("request with fixed id should be 404, got %d", w.Code)
	}
}
