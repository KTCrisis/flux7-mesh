package mcp

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/auth"
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

	return NewHTTPHandler(reg, pol, traces, approvals, handler, nil, false, nil, "queue")
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

// The session ID is not a credential. These three tests replay, in-process,
// the calls that succeeded against a live instance on 2026-09-11: a session
// opened by one agent accepted later requests carrying no credential at all,
// then requests carrying a different agent's identity. Same defect class as
// CVE-2026-59822 (LiteLLM), added to the CISA KEV catalog on 2026-09-02.

func TestHTTPSessionRejectsAnotherAgent(t *testing.T) {
	h := testHTTPHandler()

	w := postMCPWithAgent(h, "", "alice", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
	})
	sessionID := w.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize returned no session ID")
	}

	w = postMCPWithAgent(h, sessionID, "mallory", rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/list",
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("session reuse by another agent: got %d, want %d — the holder of a session ID must not inherit its creator's identity", w.Code, http.StatusForbidden)
	}
}

func TestHTTPSessionRejectsMissingCredential(t *testing.T) {
	h := testHTTPHandler()

	w := postMCPWithAgent(h, "", "alice", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
	})
	sessionID := w.Header().Get("Mcp-Session-Id")

	// No Authorization header at all: the caller resolves to "anonymous",
	// which is not the agent that opened the session.
	w = postMCP(h, sessionID, rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/list",
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("session reuse with no credential: got %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestHTTPSessionAcceptsSameAgent(t *testing.T) {
	h := testHTTPHandler()

	w := postMCPWithAgent(h, "", "alice", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
	})
	sessionID := w.Header().Get("Mcp-Session-Id")

	// The whole point of the fix is that it costs a conforming client nothing:
	// Claude Code sends its credential on every request of a session.
	w = postMCPWithAgent(h, sessionID, "alice", rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/list",
	})
	if w.Code != http.StatusOK {
		t.Errorf("same agent reusing its own session: got %d, want %d", w.Code, http.StatusOK)
	}
}

func TestHTTPRequireAuthRejectsAnonymous(t *testing.T) {
	h := testHTTPHandler()
	h.RequireAuth = true

	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous initialize with RequireAuth: got %d, want %d — the config flag must govern this transport too", w.Code, http.StatusUnauthorized)
	}
}

func TestHTTPDeleteRejectsAnotherAgent(t *testing.T) {
	h := testHTTPHandler()

	w := postMCPWithAgent(h, "", "alice", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
	})
	sessionID := w.Header().Get("Mcp-Session-Id")

	r := httptest.NewRequest("DELETE", "/mcp", nil)
	r.Header.Set("Mcp-Session-Id", sessionID)
	r.Header.Set("Authorization", "Bearer agent:mallory")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Errorf("session delete by another agent: got %d, want %d", rec.Code, http.StatusForbidden)
	}

	// And the session must still be usable by its owner.
	w = postMCPWithAgent(h, sessionID, "alice", rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/list",
	})
	if w.Code != http.StatusOK {
		t.Errorf("owner after a refused delete: got %d, want %d — the session was destroyed anyway", w.Code, http.StatusOK)
	}
}

// End to end over the real MCP transport: a delegated JWT (azp = agent,
// sub = user) opens a session and calls a tool. The user must land in the
// trace entry — this is the deliverable line of the Kong PoC, the one that
// names the human behind the agent where a gateway log names only the agent.
func TestHTTPTraceCarriesDelegatedUser(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	validator := auth.NewValidatorWithKeys(map[string]any{"test-key": &priv.PublicKey})
	validator.SetAgentClaim("azp")
	validator.SetUserClaim("sub")

	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "echo", Description: "echo", Source: "openapi",
		Params: []registry.Param{{Name: "msg", In: "body", Type: "string"}}})
	// Deny keeps the call off any backend while still recording a trace entry.
	pol := policy.NewEngine([]config.Policy{
		{Name: "deny-all", Agent: "*", Rules: []config.Rule{{Tools: []string{"*"}, Action: "deny"}}},
	})
	traces := trace.NewStore(100)
	handler := proxy.NewHandler(reg, pol, traces)
	h := NewHTTPHandler(reg, pol, traces, approval.NewStore(30), handler, nil, false, nil, "queue")
	h.JWTValidator = validator

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"azp": "newsletter-agent", "sub": "marc",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "test-key"
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}

	bearer := func(sessionID, method, name string) *httptest.ResponseRecorder {
		params := map[string]any{}
		if name != "" {
			params = map[string]any{"name": name, "arguments": map[string]any{"msg": "hi"}}
		}
		body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
		r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+signed)
		if sessionID != "" {
			r.Header.Set("Mcp-Session-Id", sessionID)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := bearer("", "initialize", "")
	sessionID := w.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("no session ID from initialize")
	}
	bearer(sessionID, "tools/call", "echo")

	entries := traces.Query("", "", 10)
	found := false
	for _, e := range entries {
		if e.Tool == "echo" {
			found = true
			if e.AgentID != "newsletter-agent" {
				t.Errorf("agent_id: got %q, want newsletter-agent", e.AgentID)
			}
			if e.UserID != "marc" {
				t.Errorf("user_id: got %q, want marc — the delegated user was lost", e.UserID)
			}
		}
	}
	if !found {
		t.Fatal("no trace entry recorded for the tool call")
	}
}

// A gateway in front of the mesh (Kong, an agent runtime) sends a
// traceparent: the mesh's span for the call must join that trace, with the
// gateway's span as its parent, or the two halves never meet in a collector.
func TestHTTPToolsCallJoinsCallerTrace(t *testing.T) {
	h := testHTTPHandler()
	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sid := w.Header().Get("Mcp-Session-Id")

	const traceID, parent = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/call",
		Params: map[string]any{"name": "echo", "arguments": map[string]any{"msg": "hi"}}})
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mcp-Session-Id", sid)
	r.Header.Set("Traceparent", "00-"+traceID+"-"+parent+"-01")
	h.ServeHTTP(httptest.NewRecorder(), r)

	got := h.Traces.Query("", "echo", 10)
	if len(got) != 1 {
		t.Fatalf("expected one trace entry for echo, got %d", len(got))
	}
	e := got[0]
	if e.TraceID != traceID || e.ParentSpanID != parent {
		t.Fatalf("entry not in the caller's trace: trace %q parent %q", e.TraceID, e.ParentSpanID)
	}
	if len(e.SpanID) != 16 || e.SpanID == parent {
		t.Fatalf("the mesh span must be its own, got %q", e.SpanID)
	}
}

// Without a traceparent the call still gets a trace of its own, set before
// anything is recorded.
func TestHTTPToolsCallOwnTraceWithoutCaller(t *testing.T) {
	h := testHTTPHandler()
	w := postMCP(h, "", rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"})
	sid := w.Header().Get("Mcp-Session-Id")
	postMCP(h, sid, rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/call",
		Params: map[string]any{"name": "echo", "arguments": map[string]any{"msg": "hi"}}})

	got := h.Traces.Query("", "echo", 10)
	if len(got) != 1 || len(got[0].TraceID) != 32 || got[0].ParentSpanID != "" {
		t.Fatalf("expected one entry with its own trace and no parent, got %+v", got)
	}
}
