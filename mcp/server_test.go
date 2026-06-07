package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

func testServer() *Server {
	reg := registry.New()
	reg.LoadManual(&registry.Tool{
		Name: "get_order", Description: "Get an order", Source: "openapi",
		Params: []registry.Param{{Name: "order_id", In: "path", Type: "string", Required: true}},
	})
	reg.LoadMCP("fs", []registry.MCPToolDef{
		{Name: "read_file", Description: "Read a file", Params: []registry.Param{{Name: "path", In: "body", Type: "string", Required: true}}},
	})

	pol := policy.NewEngine([]config.Policy{
		{Name: "allow-reads", Agent: "claude", Rules: []config.Rule{
			{Tools: []string{"get_order", "fs.read_file"}, Action: "allow"},
		}},
		{Name: "default", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"*"}, Action: "deny"},
		}},
	})

	traces := trace.NewStore(100)
	handler := proxy.NewHandler(reg, pol, traces)

	return &Server{
		Registry: reg,
		Policy:   pol,
		Traces:   traces,
		Handler:  handler,
		AgentID:  "claude",
	}
}

func sendRPC(t *testing.T, s *Server, requests ...rpcRequest) []rpcResponse {
	t.Helper()

	var input bytes.Buffer
	for _, req := range requests {
		data, _ := json.Marshal(req)
		input.Write(data)
		input.WriteByte('\n')
	}

	var output bytes.Buffer
	err := s.Serve(&input, &output)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	var responses []rpcResponse
	decoder := json.NewDecoder(&output)
	for {
		var resp rpcResponse
		if err := decoder.Decode(&resp); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode response: %v", err)
		}
		responses = append(responses, resp)
	}
	return responses
}

func TestServerInitialize(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "initialize",
		Params: map[string]any{"protocolVersion": "2024-11-05"},
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	resp := responses[0]
	if resp.Error != nil {
		t.Fatalf("error: %v", resp.Error)
	}

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", resp.Result)
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "flux7-mesh" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

func TestServerToolsList(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/list",
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}

	result, _ := responses[0].Result.(map[string]any)
	tools, _ := result["tools"].([]any)
	// Normal (non-supervisor) agent: 2 registry + grant.list + mesh.catalog = 4.
	// approval.* and grant.create/revoke are operator-only and hidden here.
	if len(tools) != 4 {
		t.Errorf("tools = %d, want 4", len(tools))
	}
}

func TestServerToolsCallDeny(t *testing.T) {
	s := testServer()
	// Agent is "claude" but calling a tool not in allow list → deny via default
	s.AgentID = "anonymous"
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "get_order",
			"arguments": map[string]any{"order_id": "123"},
		},
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}

	// Denied calls return result with "Policy denied" text, not an RPC error
	result, _ := responses[0].Result.(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content in response")
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "Policy denied") {
		t.Errorf("expected 'Policy denied', got: %s", text)
	}
}

func TestServerToolsCallUnknown(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name": "nonexistent",
		},
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected RPC error for unknown tool")
	}
	if responses[0].Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", responses[0].Error.Code)
	}
}

func TestServerPing(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "ping",
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("error: %v", responses[0].Error)
	}
}

func TestServerUnknownMethod(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "nonexistent/method",
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if responses[0].Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", responses[0].Error.Code)
	}
}

func TestServerParseError(t *testing.T) {
	s := testServer()

	input := bytes.NewBufferString("not valid json\n")
	var output bytes.Buffer
	s.Serve(input, &output)

	var resp rpcResponse
	json.NewDecoder(&output).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
	if resp.Error.Code != -32700 {
		t.Errorf("error code = %d, want -32700", resp.Error.Code)
	}
}

func TestServerNotificationSkipped(t *testing.T) {
	s := testServer()
	// notifications/initialized should not produce a response
	responses := sendRPC(t, s,
		rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"},
		rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "ping"},
	)

	// Should only get 1 response (ping), not 2
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1 (notification should be skipped)", len(responses))
	}
}

func TestServerMultipleRequests(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s,
		rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "initialize"},
		rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"},
		rpcRequest{JSONRPC: "2.0", ID: float64(2), Method: "tools/list"},
		rpcRequest{JSONRPC: "2.0", ID: float64(3), Method: "ping"},
	)

	if len(responses) != 3 {
		t.Fatalf("responses = %d, want 3", len(responses))
	}
}

func TestServerToolsCallMissingName(t *testing.T) {
	s := testServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{},
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for missing tool name")
	}
}

// --- Approval tests (blocking flow — resolved via goroutine or HTTP API) ---

func approvalServer() *Server {
	reg := registry.New()
	reg.LoadManual(&registry.Tool{
		Name: "risky_tool", Description: "Risky", Source: "openapi",
	})

	pol := policy.NewEngine([]config.Policy{
		{Name: "approval-policy", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"risky_tool"}, Action: "human_approval"},
		}},
	})

	traces := trace.NewStore(100)
	handler := proxy.NewHandler(reg, pol, traces)

	return &Server{
		Registry:  reg,
		Policy:    pol,
		Traces:    traces,
		Approvals: approval.NewStore(5 * time.Second),
		Handler:   handler,
		AgentID:   "claude",
		// approval.resolve/pending are supervisor-only; these tests exercise
		// the resolution mechanism, so the test agent is a declared supervisor.
		SupervisorAgents: []string{"claude"},
	}
}

// extractText extracts the text from an MCP content response.
func extractText(t *testing.T, resp rpcResponse) string {
	t.Helper()
	result, _ := resp.Result.(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatal("expected content in response")
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// sendRPCAsync starts Serve in a goroutine and returns responses via channel.
// Needed because the blocking approval flow holds Serve until resolved.
func sendRPCAsync(t *testing.T, s *Server, requests ...rpcRequest) <-chan []rpcResponse {
	t.Helper()
	ch := make(chan []rpcResponse, 1)

	var input bytes.Buffer
	for _, req := range requests {
		data, _ := json.Marshal(req)
		input.Write(data)
		input.WriteByte('\n')
	}

	go func() {
		var output bytes.Buffer
		if err := s.Serve(&input, &output); err != nil {
			t.Errorf("Serve: %v", err)
		}
		var responses []rpcResponse
		decoder := json.NewDecoder(&output)
		for {
			var resp rpcResponse
			if err := decoder.Decode(&resp); err != nil {
				break
			}
			responses = append(responses, resp)
		}
		ch <- responses
	}()

	return ch
}

func TestServerApprovalReturnsImmediately(t *testing.T) {
	s := approvalServer()

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "risky_tool",
			"arguments": map[string]any{},
		},
	})
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}

	text := extractText(t, responses[0])
	if !strings.Contains(text, "Approval required") {
		t.Errorf("expected 'Approval required', got: %s", text)
	}
	if !strings.Contains(text, "approval.resolve") {
		t.Errorf("expected instructions for approval.resolve, got: %s", text)
	}

	// Approval should be pending in the store
	pending := s.Approvals.ListPending()
	if len(pending) != 1 {
		t.Errorf("pending = %d, want 1", len(pending))
	}
}

func TestServerApprovalResolveApprove(t *testing.T) {
	s := approvalServer()

	// Step 1: tool call returns immediately with pending approval
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "risky_tool",
			"arguments": map[string]any{},
		},
	})
	text := extractText(t, responses[0])
	if !strings.Contains(text, "Approval required") {
		t.Fatalf("expected pending response, got: %s", text)
	}

	pending := s.Approvals.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}

	// Step 2: resolve via approval.resolve tool
	responses = sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(2), Method: "tools/call",
		Params: map[string]any{
			"name": "approval.resolve",
			"arguments": map[string]any{
				"id":       pending[0].ID[:8],
				"decision": "approve",
			},
		},
	})
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("RPC error: %v", responses[0].Error)
	}

	// Should be resolved now
	remaining := s.Approvals.ListPending()
	if len(remaining) != 0 {
		t.Errorf("pending after approve = %d, want 0", len(remaining))
	}
}

func TestServerApprovalResolveDeny(t *testing.T) {
	s := approvalServer()

	// Step 1: tool call returns immediately with pending approval
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "risky_tool",
			"arguments": map[string]any{},
		},
	})
	text := extractText(t, responses[0])
	if !strings.Contains(text, "Approval required") {
		t.Fatalf("expected pending response, got: %s", text)
	}

	pending := s.Approvals.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}

	// Step 2: deny via approval.resolve tool
	responses = sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(2), Method: "tools/call",
		Params: map[string]any{
			"name": "approval.resolve",
			"arguments": map[string]any{
				"id":       pending[0].ID[:8],
				"decision": "deny",
			},
		},
	})
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}

	text = extractText(t, responses[0])
	if !strings.Contains(text, "Denied") {
		t.Errorf("expected denial, got: %s", text)
	}
}

func TestServerApprovalPendingList(t *testing.T) {
	s := approvalServer()

	// No pending initially
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "approval.pending",
			"arguments": map[string]any{},
		},
	})
	text := extractText(t, responses[0])
	if !strings.Contains(text, "No pending") {
		t.Errorf("expected 'No pending', got: %s", text)
	}

	// Trigger an approval (blocking) — resolve quickly so sendRPC returns
	s2 := approvalServer()
	s2.Approvals = approval.NewStore(300 * time.Millisecond)
	ch := sendRPCAsync(t, s2, rpcRequest{
		JSONRPC: "2.0", ID: float64(2), Method: "tools/call",
		Params: map[string]any{
			"name":      "risky_tool",
			"arguments": map[string]any{},
		},
	})

	// Wait for it to appear
	for i := 0; i < 50; i++ {
		if len(s2.Approvals.ListPending()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pending := s2.Approvals.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].Tool != "risky_tool" {
		t.Errorf("expected risky_tool, got %s", pending[0].Tool)
	}

	// Let it timeout so the goroutine finishes
	<-ch
}

func TestServerApprovalResolveNotFound(t *testing.T) {
	s := approvalServer()

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "approval.resolve",
			"arguments": map[string]any{"id": "nonexist", "decision": "approve"},
		},
	})

	if responses[0].Error == nil {
		t.Fatal("expected error for unknown approval ID")
	}
	if !strings.Contains(responses[0].Error.Message, "not found") {
		t.Errorf("expected 'not found', got: %s", responses[0].Error.Message)
	}
}

func TestServerApprovalResolveInvalidDecision(t *testing.T) {
	s := approvalServer()

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "approval.resolve",
			"arguments": map[string]any{"id": "abc", "decision": "maybe"},
		},
	})

	if responses[0].Error == nil {
		t.Fatal("expected error for invalid decision")
	}
	if !strings.Contains(responses[0].Error.Message, "Invalid") {
		t.Errorf("expected 'Invalid', got: %s", responses[0].Error.Message)
	}
}

func TestServerToolsListIncludesVirtualTools(t *testing.T) {
	s := approvalServer()
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/list",
	})

	result, _ := responses[0].Result.(map[string]any)
	tools, _ := result["tools"].([]any)

	found := map[string]bool{}
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		name, _ := tool["name"].(string)
		found[name] = true
	}

	if !found["approval.resolve"] {
		t.Error("approval.resolve not in tools list")
	}
	if !found["approval.pending"] {
		t.Error("approval.pending not in tools list")
	}
}

func TestServerApprovalFallbackNoStore(t *testing.T) {
	s := testServer()
	// Add a tool with human_approval policy but no Approvals store
	s.Registry.LoadManual(&registry.Tool{Name: "needs_approval", Source: "openapi"})
	s.Policy = policy.NewEngine([]config.Policy{
		{Name: "test", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"needs_approval"}, Action: "human_approval"},
		}},
	})

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{
			"name":      "needs_approval",
			"arguments": map[string]any{},
		},
	})

	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}

	text := extractText(t, responses[0])
	if !strings.Contains(text, "human approval") {
		t.Errorf("expected fallback text, got: %s", text)
	}
}

// TestUpstreamSchemaPassthrough verifies that JSON Schema constructs received
// from an upstream MCP server — anyOf, items, enum, nested objects — survive
// the round-trip through mesh7's registry and re-export layer.
//
// Regression test for the silent downgrade where optional parameters declared
// as `list[dict] | None` upstream were rewritten to {type: "string"} on
// tools/list re-export, causing Pydantic validation errors at the upstream
// server when clients faithfully serialised the argument as a string.
func TestUpstreamSchemaPassthrough(t *testing.T) {
	// Raw upstream tools/list response for a fictional tool with three kinds
	// of non-trivial parameter schemas: array, anyOf[array, null] and enum.
	upstreamJSON := []byte(`{
		"tools": [{
			"name": "create_diagram",
			"description": "Create a diagram",
			"inputSchema": {
				"type": "object",
				"properties": {
					"nodes": {
						"type": "array",
						"items": {"type": "object", "additionalProperties": true},
						"description": "List of nodes"
					},
					"subgraphs": {
						"anyOf": [
							{"type": "array", "items": {"type": "object"}},
							{"type": "null"}
						],
						"default": null,
						"description": "Optional groups"
					},
					"theme": {
						"type": "string",
						"enum": ["default", "dark", "professional"],
						"default": "default"
					}
				},
				"required": ["nodes"]
			}
		}]
	}`)

	var wrapper struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(upstreamJSON, &wrapper); err != nil {
		t.Fatalf("unmarshal upstream: %v", err)
	}
	if len(wrapper.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(wrapper.Tools))
	}
	tool := wrapper.Tools[0]

	// Verify the raw property JSON was preserved during upstream unmarshal.
	for _, name := range []string{"nodes", "subgraphs", "theme"} {
		prop := tool.InputSchema.Properties[name]
		if len(prop.Raw) == 0 {
			t.Errorf("property %q: Raw schema not preserved after unmarshal", name)
		}
	}

	// Convert into a registry MCPToolDef the way cmd/mesh7/main.go does,
	// then register and verify the RawSchema is carried on the Param.
	props := make(map[string]registry.MCPPropDef, len(tool.InputSchema.Properties))
	for name, p := range tool.InputSchema.Properties {
		props[name] = registry.MCPPropDef{Type: p.Type, RawSchema: p.Raw}
	}
	def := registry.NewMCPToolDef(tool.Name, tool.Description, props, tool.InputSchema.Required)

	reg := registry.New()
	reg.LoadMCP("upstream", []registry.MCPToolDef{def})

	registered := reg.All()
	var registeredTool *registry.Tool
	for _, rt := range registered {
		if rt.Name == "upstream.create_diagram" {
			registeredTool = rt
			break
		}
	}
	if registeredTool == nil {
		t.Fatalf("upstream.create_diagram not registered")
	}
	foundRaw := 0
	for _, p := range registeredTool.Params {
		if len(p.RawSchema) > 0 {
			foundRaw++
		}
	}
	if foundRaw != 3 {
		t.Errorf("expected 3 params with RawSchema, got %d", foundRaw)
	}

	// Run handleToolsList via a minimal server and check the emitted schema.
	pol := policy.NewEngine([]config.Policy{
		{Name: "allow-all", Agent: "*", Rules: []config.Rule{{Tools: []string{"*"}, Action: "allow"}}},
	})
	srv := &Server{
		Registry:  reg,
		Policy:    pol,
		Traces:    trace.NewStore(100),
		Approvals: approval.NewStore(time.Second),
	}

	resp := srv.handleToolsList()
	reEmitted, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	var parsed struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reEmitted, &parsed); err != nil {
		t.Fatalf("unmarshal re-emitted response: %v", err)
	}

	var createTool *struct {
		Name        string `json:"name"`
		InputSchema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"inputSchema"`
	}
	for i := range parsed.Tools {
		if parsed.Tools[i].Name == "upstream.create_diagram" {
			createTool = &parsed.Tools[i]
			break
		}
	}
	if createTool == nil {
		t.Fatalf("upstream.create_diagram not in re-emitted tools/list")
	}

	// The anyOf in `subgraphs` must survive verbatim — this is the regression.
	subgraphsJSON := string(createTool.InputSchema.Properties["subgraphs"])
	if !strings.Contains(subgraphsJSON, `"anyOf"`) {
		t.Errorf("subgraphs lost anyOf on re-export: %s", subgraphsJSON)
	}
	if strings.Contains(subgraphsJSON, `"type":"string"`) || strings.Contains(subgraphsJSON, `"type": "string"`) {
		t.Errorf("subgraphs was silently downgraded to type:string: %s", subgraphsJSON)
	}

	// The items in `nodes` must still be present.
	nodesJSON := string(createTool.InputSchema.Properties["nodes"])
	if !strings.Contains(nodesJSON, `"items"`) {
		t.Errorf("nodes lost items on re-export: %s", nodesJSON)
	}

	// The enum in `theme` must still be present.
	themeJSON := string(createTool.InputSchema.Properties["theme"])
	if !strings.Contains(themeJSON, `"enum"`) {
		t.Errorf("theme lost enum on re-export: %s", themeJSON)
	}
}

func TestSupervisorModeHidesApprovalTools(t *testing.T) {
	s := testServer()
	s.SupervisorMode = true
	s.Approvals = approval.NewStore(30 * time.Second)

	// tools/list should not contain approval tools
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if strings.Contains(string(data), "approval.resolve") {
		t.Error("approval.resolve should be hidden in supervisor mode")
	}

	// tools/call should reject approval tools
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "approval.pending", "arguments": map[string]any{},
	}})
	if resp.Error == nil {
		t.Error("approval.pending should be rejected in supervisor mode")
	}
}

func TestSupervisorAgentsWhitelist(t *testing.T) {
	s := testServer()
	s.SupervisorMode = true
	s.SupervisorAgents = []string{"supervisor-*"}
	s.AgentID = "supervisor-claude"
	s.Approvals = approval.NewStore(30 * time.Second)

	// Whitelisted agent should see approval tools
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(data), "approval.resolve") {
		t.Error("supervisor agent should see approval.resolve")
	}
	if !strings.Contains(string(data), "approval.pending") {
		t.Error("supervisor agent should see approval.pending")
	}

	// Whitelisted agent should be able to call approval tools
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "approval.pending", "arguments": map[string]any{},
	}})
	if resp.Error != nil {
		t.Errorf("supervisor agent should access approval.pending, got error: %s", resp.Error.Message)
	}
}

func TestGrantCreateBlockedForNormalAgent(t *testing.T) {
	s := testServer()
	s.AgentID = "worker-agent"
	s.Handler.Grants = grant.NewStore()

	// Not listed
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if strings.Contains(string(data), "grant.create") {
		t.Error("normal agent should not see grant.create")
	}
	if strings.Contains(string(data), "grant.revoke") {
		t.Error("normal agent should not see grant.revoke")
	}
	// grant.list stays visible (read-only)
	if !strings.Contains(string(data), "grant.list") {
		t.Error("grant.list should remain visible to all agents")
	}

	// Not callable even if invoked directly (hiding is not enforcement)
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "grant.create", "arguments": map[string]any{"tools": "*", "duration": "24h"},
	}})
	if resp.Error == nil {
		t.Error("normal agent must be rejected when calling grant.create — self-escalation hole")
	}
	if s.Handler.Grants.Check("worker-agent", "anything") != nil {
		t.Error("no grant should have been created for a blocked agent")
	}
}

func TestGrantCreateAllowedForSupervisor(t *testing.T) {
	s := testServer()
	s.SupervisorAgents = []string{"supervisor-*"}
	s.AgentID = "supervisor-claude"
	s.Handler.Grants = grant.NewStore()

	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(data), "grant.create") {
		t.Error("supervisor agent should see grant.create")
	}

	// Supervisor grants on behalf of another agent
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "grant.create", "arguments": map[string]any{
			"agent": "worker-agent", "tools": "filesystem.write_file", "duration": "1h",
		},
	}})
	if resp.Error != nil {
		t.Fatalf("supervisor should create grants, got error: %s", resp.Error.Message)
	}
	if s.Handler.Grants.Check("worker-agent", "filesystem.write_file") == nil {
		t.Error("grant for worker-agent should exist after supervisor created it")
	}
}

func TestSupervisorAgentsNonMatchBlocked(t *testing.T) {
	s := testServer()
	s.SupervisorMode = true
	s.SupervisorAgents = []string{"supervisor-*"}
	s.AgentID = "worker-agent"
	s.Approvals = approval.NewStore(30 * time.Second)

	// Non-matching agent should NOT see approval tools
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if strings.Contains(string(data), "approval.resolve") {
		t.Error("non-supervisor agent should not see approval.resolve")
	}

	// Non-matching agent should be rejected
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "approval.resolve", "arguments": map[string]any{"id": "abc", "decision": "approve"},
	}})
	if resp.Error == nil {
		t.Error("non-supervisor agent should be rejected for approval.resolve")
	}
}

// TestApprovalResolveBlockedForNormalAgent is the twin of the grant.create
// guard: a non-supervisor agent must not be able to resolve approvals (it would
// let it approve its own human_approval requests).
func TestApprovalResolveBlockedForNormalAgent(t *testing.T) {
	s := approvalServer()
	s.SupervisorAgents = nil // normal agent, no supervisor configured
	s.AgentID = "worker-bot"

	// Not listed
	resp := s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	data, _ := json.Marshal(resp.Result)
	if strings.Contains(string(data), "approval.resolve") {
		t.Error("normal agent should not see approval.resolve")
	}
	if strings.Contains(string(data), "approval.pending") {
		t.Error("normal agent should not see approval.pending")
	}

	// Not callable (hiding is not enforcement)
	resp = s.HandleRequest(rpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{
		"name": "approval.resolve", "arguments": map[string]any{"id": "abcd1234", "decision": "approve"},
	}})
	if resp.Error == nil {
		t.Error("normal agent must be rejected calling approval.resolve — self-approval hole")
	}
}

// --- approval.channel routing tests ---

func TestApprovalChannelAction(t *testing.T) {
	cases := []struct {
		channel    string
		tryTTY     bool
		failClosed bool
	}{
		{"queue", false, false},
		{"tty", true, true},
		{"tty-fallback", true, false},
		{"", true, false}, // unset = historical behavior
	}
	for _, c := range cases {
		tryTTY, failClosed := approvalChannelAction(c.channel)
		if tryTTY != c.tryTTY || failClosed != c.failClosed {
			t.Errorf("approvalChannelAction(%q) = (%v, %v), want (%v, %v)",
				c.channel, tryTTY, failClosed, c.tryTTY, c.failClosed)
		}
	}
}

func TestApprovalChannelQueueSkipsTTY(t *testing.T) {
	s := approvalServer()
	s.SupervisorAgents = nil // non-blocking flow
	s.ApprovalChannel = "queue"
	ttyCalled := false
	s.ttyPrompt = func(string, map[string]any) (*bool, string) {
		ttyCalled = true
		approved := true
		return &approved, "tty:test"
	}

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "risky_tool", "arguments": map[string]any{}},
	})

	if ttyCalled {
		t.Error("channel=queue must not attempt the TTY prompt")
	}
	if text := extractText(t, responses[0]); !strings.Contains(text, "Approval required") {
		t.Errorf("expected queued approval, got: %s", text)
	}
	if pending := s.Approvals.ListPending(); len(pending) != 1 {
		t.Errorf("pending = %d, want 1", len(pending))
	}
}

func TestApprovalChannelTTYFailClosed(t *testing.T) {
	s := approvalServer()
	s.SupervisorAgents = nil
	s.ApprovalChannel = "tty"
	// TTY unavailable: prompt returns nil (same contract as promptTTY without /dev/tty)
	s.ttyPrompt = func(string, map[string]any) (*bool, string) { return nil, "" }

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "risky_tool", "arguments": map[string]any{}},
	})

	text := extractText(t, responses[0])
	if !strings.Contains(text, "Approval denied") {
		t.Errorf("channel=tty without TTY must fail closed, got: %s", text)
	}
	if pending := s.Approvals.ListPending(); len(pending) != 0 {
		t.Errorf("fail-closed must not enqueue, pending = %d", len(pending))
	}
}

func TestApprovalChannelTTYFallbackUsesQueue(t *testing.T) {
	s := approvalServer()
	s.SupervisorAgents = nil
	s.ApprovalChannel = "tty-fallback"
	s.ttyPrompt = func(string, map[string]any) (*bool, string) { return nil, "" }

	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "risky_tool", "arguments": map[string]any{}},
	})

	if text := extractText(t, responses[0]); !strings.Contains(text, "Approval required") {
		t.Errorf("tty-fallback without TTY must enqueue, got: %s", text)
	}
	if pending := s.Approvals.ListPending(); len(pending) != 1 {
		t.Errorf("pending = %d, want 1", len(pending))
	}
}
