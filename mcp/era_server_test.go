package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/halt"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// envelope is the 2026-07-28 per-request envelope a modern client sends.
func envelope(version string) map[string]any {
	return map[string]any{
		metaProtocolVersion:    version,
		metaClientInfo:         map[string]any{"name": "t", "version": "1"},
		metaClientCapabilities: map[string]any{},
	}
}

// postModern sends a modern request: envelope in the body and, unless the
// caller overrides them, the three headers matching it. A header set to ""
// in override is left out.
func postModern(h *HTTPHandler, agent, method string, params map[string]any, override map[string]string) *httptest.ResponseRecorder {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = envelope(modernProtocolVersion)
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: method, Params: params})
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if agent != "" {
		r.Header.Set("Authorization", "Bearer agent:"+agent)
	}
	hdr := map[string]string{"MCP-Protocol-Version": modernProtocolVersion, "Mcp-Method": method}
	if name, ok := params["name"].(string); ok && method == "tools/call" {
		hdr["Mcp-Name"] = name
	}
	for k, v := range override {
		hdr[k] = v
	}
	for k, v := range hdr {
		if v != "" {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeResp(t *testing.T, w *httptest.ResponseRecorder) rpcResponse {
	t.Helper()
	var resp rpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return resp
}

// A modern client needs no initialize and no session: server/discover names
// the versions, tools/list answers at once, and no session ID comes back.
func TestHTTPModernNeedsNoSession(t *testing.T) {
	h := testHTTPHandler()

	w := postModern(h, "claude", "server/discover", nil, nil)
	if w.Code != 200 {
		t.Fatalf("discover status = %d: %s", w.Code, w.Body)
	}
	disc, _ := decodeResp(t, w).Result.(map[string]any)
	versions, _ := disc["supportedVersions"].([]any)
	if len(versions) == 0 || versions[0] != modernProtocolVersion {
		t.Errorf("supportedVersions = %v", disc["supportedVersions"])
	}

	w = postModern(h, "claude", "tools/list", nil, nil)
	if w.Code != 200 {
		t.Fatalf("tools/list status = %d: %s", w.Code, w.Body)
	}
	list, _ := decodeResp(t, w).Result.(map[string]any)
	if list["resultType"] != "complete" || list["cacheScope"] != "private" {
		t.Errorf("tools/list = %v", list)
	}
	if id := w.Header().Get("Mcp-Session-Id"); id != "" {
		t.Errorf("modern request handed a session %q", id)
	}
	h.mu.Lock()
	n := len(h.sessions)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("%d sessions kept for modern requests", n)
	}
}

// Identity is resolved on every modern request and the policy applies to it.
func TestHTTPModernIdentityPerRequest(t *testing.T) {
	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "echo", Source: "openapi",
		Params: []registry.Param{{Name: "msg", In: "body", Type: "string"}}})
	pol := policy.NewEngine([]config.Policy{
		{Name: "alice", Agent: "alice", Rules: []config.Rule{{Tools: []string{"echo"}, Action: "allow"}}},
		{Name: "default", Agent: "*", Rules: []config.Rule{{Tools: []string{"*"}, Action: "deny"}}},
	})
	traces := trace.NewStore(100)
	approvals := approval.NewStore(30)
	handler := proxy.NewHandler(reg, pol, traces)
	h := NewHTTPHandler(reg, pol, traces, approvals, handler, nil, false, nil, "queue")

	call := func(agent string) string {
		w := postModern(h, agent, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"msg": "x"}}, nil)
		res, _ := decodeResp(t, w).Result.(map[string]any)
		content, _ := res["content"].([]any)
		first, _ := content[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}
	if strings.Contains(call("alice"), "Policy denied") {
		t.Error("alice denied")
	}
	if !strings.Contains(call("bob"), "Policy denied") {
		t.Error("bob allowed: identity not resolved per request")
	}
}

// Headers that disagree with the body are refused with -32020, in either era:
// a gateway in front may have routed or filtered on them.
func TestHTTPHeaderMismatch(t *testing.T) {
	h := testHTTPHandler()
	call := map[string]any{"name": "echo", "arguments": map[string]any{"msg": "x"}}

	cases := []struct {
		name     string
		override map[string]string
	}{
		{"lying Mcp-Name", map[string]string{"Mcp-Name": "other"}},
		{"missing Mcp-Name", map[string]string{"Mcp-Name": ""}},
		{"lying Mcp-Method", map[string]string{"Mcp-Method": "tools/list"}},
		{"missing Mcp-Method", map[string]string{"Mcp-Method": ""}},
		{"missing version header", map[string]string{"MCP-Protocol-Version": ""}},
	}
	for _, c := range cases {
		params := map[string]any{"name": call["name"], "arguments": call["arguments"]}
		w := postModern(h, "claude", "tools/call", params, c.override)
		if w.Code != 400 {
			t.Errorf("%s: status = %d, want 400", c.name, w.Code)
			continue
		}
		if resp := decodeResp(t, w); resp.Error == nil || resp.Error.Code != errHeaderMismatch {
			t.Errorf("%s: error = %+v, want -32020", c.name, resp.Error)
		}
	}

	// A legacy request carrying a lying Mcp-Name is refused too.
	body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call", Params: call})
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header.Set("Mcp-Name", "other")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if resp := decodeResp(t, w); w.Code != 400 || resp.Error == nil || resp.Error.Code != errHeaderMismatch {
		t.Errorf("legacy lying Mcp-Name: status %d, error %+v", w.Code, resp.Error)
	}
}

// A modern request naming a version mesh7 does not speak learns which ones
// it does.
func TestModernUnsupportedVersion(t *testing.T) {
	s := testServer()
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list",
		Params: map[string]any{"_meta": envelope("2099-01-01")}})
	if resp.Error == nil || resp.Error.Code != errUnsupportedProtocolVersion {
		t.Fatalf("error = %+v, want -32022", resp.Error)
	}
	data, _ := resp.Error.Data.(map[string]any)
	if supported, _ := data["supported"].([]string); len(supported) == 0 || supported[0] != modernProtocolVersion {
		t.Errorf("supported = %v", data["supported"])
	}
}

// Legacy clients are served exactly as before: initialize still answers
// 2024-11-05, ping still works, and only modern requests lose ping.
func TestLegacyUnchanged(t *testing.T) {
	s := testServer()
	init := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize", Params: map[string]any{}})
	if res, _ := init.Result.(map[string]any); res["protocolVersion"] != legacyProtocolVersion {
		t.Errorf("initialize = %v", init.Result)
	}
	if ping := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "ping"}); ping.Error != nil {
		t.Errorf("legacy ping: %+v", ping.Error)
	}
	list := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: float64(3), Method: "tools/list"})
	if res, _ := list.Result.(map[string]any); res["resultType"] != nil {
		t.Errorf("legacy tools/list carries resultType: %v", res)
	}
	modernPing := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: float64(4), Method: "ping",
		Params: map[string]any{"_meta": envelope(modernProtocolVersion)}})
	if modernPing.Error == nil {
		t.Error("modern ping answered; the 2026-07-28 revision removed it")
	}
}

// A modern client that labels its conversation with X-Session-Id is reached
// by a session-scoped halt, and its calls group by session and by the trace
// it carries in _meta.
func TestHTTPModernSessionLabelAndTrace(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "read", Source: "openapi", Method: "GET", Path: "/r", BaseURL: backend.URL})
	pol := policy.NewEngine([]config.Policy{{Name: "p", Agent: "*", Rules: []config.Rule{
		{Tools: []string{"*"}, Action: "allow"}}}})
	traces := trace.NewStore(50)
	handler := proxy.NewHandler(reg, pol, traces)
	handler.Halts, _ = halt.NewStore(nil)
	h := NewHTTPHandler(reg, pol, traces, approval.NewStore(30), handler, nil, false, nil, "queue")

	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	call := func(session string) string {
		meta := envelope(modernProtocolVersion)
		meta["traceparent"] = tp
		body, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
			Params: map[string]any{"name": "read", "arguments": map[string]any{}, "_meta": meta}})
		r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer agent:claude")
		r.Header.Set("MCP-Protocol-Version", modernProtocolVersion)
		r.Header.Set("Mcp-Method", "tools/call")
		r.Header.Set("Mcp-Name", "read")
		r.Header.Set("X-Session-Id", session)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		res, _ := decodeResp(t, w).Result.(map[string]any)
		content, _ := res["content"].([]any)
		first, _ := content[0].(map[string]any)
		text, _ := first["text"].(string)
		return text
	}

	if _, _, err := handler.Halts.Start(halt.ScopeSession, "conv-42", "test", "operator"); err != nil {
		t.Fatal(err)
	}
	if got := call("conv-42"); !strings.Contains(strings.ToLower(got), "halt") {
		t.Errorf("halted session served: %q", got)
	}
	if got := call("conv-7"); strings.Contains(strings.ToLower(got), "halt") {
		t.Errorf("other session halted: %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("backend hits = %d, want 1", hits.Load())
	}

	bySession := traces.QueryBySession("conv-7", 10)
	if len(bySession) != 1 {
		t.Fatalf("traces for conv-7 = %d, want 1", len(bySession))
	}
	if bySession[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %q, want the one from _meta", bySession[0].TraceID)
	}
}

func TestSessionLabel(t *testing.T) {
	for v, want := range map[string]string{
		"conv-42":                "conv-42",
		"":                       "",
		"has space":              "",
		"tab\there":              "",
		strings.Repeat("a", 129): "",
		strings.Repeat("a", 128): strings.Repeat("a", 128),
	} {
		h := http.Header{}
		h.Set("X-Session-Id", v)
		if got := sessionLabel(h); got != want {
			t.Errorf("sessionLabel(%q) = %q, want %q", v, got, want)
		}
	}
}
