package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/KTCrisis/agent-mesh/approval"
	"github.com/KTCrisis/agent-mesh/internal/match"
	"github.com/KTCrisis/agent-mesh/policy"
	"github.com/KTCrisis/agent-mesh/proxy"
	"github.com/KTCrisis/agent-mesh/registry"
	"github.com/KTCrisis/agent-mesh/trace"
)

// JSON-RPC 2.0 types

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPTool is the MCP tool format.
type MCPTool struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	InputSchema MCPSchema `json:"inputSchema"`
}

// MCPSchema describes the input parameters of an MCP tool.
type MCPSchema struct {
	Type       string                `json:"type"`
	Properties map[string]MCPProp    `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

// MCPProp describes a single property in an MCP tool schema.
//
// When MCPProp is unmarshalled from an upstream MCP server response, the full
// raw JSON is preserved in Raw so it can be passed through verbatim on
// re-export. This keeps schema constructs like "anyOf", "items", "enum" and
// nested object schemas intact — agent-mesh stays a pure proxy and never has
// to understand JSON Schema itself. Virtual/local tools can still be built
// with a plain {Type, Description} literal; in that case Raw is empty and
// MarshalJSON falls back to the shallow form.
type MCPProp struct {
	Type        string          `json:"type,omitempty"`
	Description string          `json:"description,omitempty"`
	Raw         json.RawMessage `json:"-"`
}

// UnmarshalJSON preserves the raw JSON of a property while also decoding the
// shallow Type and Description fields for the registry's internal use.
func (p *MCPProp) UnmarshalJSON(data []byte) error {
	p.Raw = make(json.RawMessage, len(data))
	copy(p.Raw, data)

	var shallow struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	// A shallow decode failure is not fatal — we still have Raw for passthrough.
	_ = json.Unmarshal(data, &shallow)
	p.Type = shallow.Type
	p.Description = shallow.Description
	return nil
}

// MarshalJSON emits the preserved raw schema when available (upstream tools)
// and otherwise falls back to the shallow {type, description} form used by
// locally-defined virtual tools.
func (p MCPProp) MarshalJSON() ([]byte, error) {
	if len(p.Raw) > 0 {
		return p.Raw, nil
	}
	shallow := struct {
		Type        string `json:"type,omitempty"`
		Description string `json:"description,omitempty"`
	}{Type: p.Type, Description: p.Description}
	return json.Marshal(shallow)
}

// Server runs the MCP stdio protocol.
type Server struct {
	Registry         *registry.Registry
	Policy           *policy.Engine
	Traces           *trace.Store
	Approvals        *approval.Store
	Handler          *proxy.Handler
	MCPManager       *Manager
	AgentID          string   // agent ID for policy evaluation in MCP mode
	SessionID        string   // optional session ID (set externally or auto-generated at initialize)
	SupervisorMode   bool     // when true, hide approval.* virtual tools from agents
	SupervisorAgents []string // agent ID globs allowed to see approval tools in supervisor mode
}

// Run starts the MCP server on stdin/stdout.
func (s *Server) Run() error {
	return s.Serve(os.Stdin, os.Stdout)
}

// HandleRequest dispatches a single JSON-RPC request and returns the response.
// For notifications (no ID, e.g. "notifications/initialized"), the returned
// response has a zero JSONRPC field — callers should check this and skip
// writing a response (stdio) or return 202 (HTTP).
func (s *Server) HandleRequest(req rpcRequest) rpcResponse {
	slog.Debug("MCP request", "method", req.Method, "id", req.ID)

	var resp rpcResponse
	resp.JSONRPC = "2.0"
	resp.ID = req.ID

	switch req.Method {
	case "initialize":
		if s.SessionID == "" {
			s.SessionID = trace.NewID()
		}
		slog.Info("MCP session started", "session_id", s.SessionID)
		resp.Result = s.handleInitialize()
	case "notifications/initialized":
		return rpcResponse{}
	case "tools/list":
		resp.Result = s.handleToolsList()
	case "tools/call":
		resp.Result, resp.Error = s.handleToolsCall(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)}
	}
	return resp
}

// Serve runs the MCP server on the given reader/writer.
func (s *Server) Serve(r io.Reader, w io.Writer) error {
	slog.Info("MCP server starting", "agent", s.AgentID, "tools", len(s.Registry.All()))

	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				slog.Info("MCP server: stdin closed")
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(w, nil, -32700, "Parse error")
			continue
		}

		resp := s.HandleRequest(req)

		// Notifications produce a zero-value response — skip writing.
		if resp.JSONRPC == "" {
			continue
		}

		s.writeResponse(w, resp)
	}
}

func (s *Server) handleInitialize() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "agent-mesh",
			"version": "0.1.0",
		},
	}
}

func (s *Server) handleToolsList() map[string]any {
	tools := s.Registry.All()
	mcpTools := make([]MCPTool, 0, len(tools))

	for _, t := range tools {
		// Build input schema from tool params
		props := make(map[string]MCPProp)
		var required []string

		for _, p := range t.Params {
			if len(p.RawSchema) > 0 {
				// Upstream MCP tool: pass the raw schema through verbatim so
				// constructs like anyOf, items, enum and nested objects stay
				// intact for downstream agents. agent-mesh does not interpret
				// or rebuild JSON Schema — it just proxies it.
				props[p.Name] = MCPProp{Raw: p.RawSchema}
			} else {
				// Virtual/local tool, OpenAPI or CLI import: build a shallow
				// {type, description} property from the registry fields.
				propType := p.Type
				if propType == "" {
					propType = "string"
				}
				props[p.Name] = MCPProp{
					Type:        propType,
					Description: fmt.Sprintf("%s parameter (%s)", p.Name, p.In),
				}
			}
			if p.Required {
				required = append(required, p.Name)
			}
		}

		mcpTools = append(mcpTools, MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: MCPSchema{
				Type:       "object",
				Properties: props,
				Required:   required,
			},
		})
	}

	// Append virtual approval tools (hidden in supervisor mode, unless agent is a declared supervisor)
	if !s.SupervisorMode || match.GlobAny(s.SupervisorAgents, s.AgentID) {
		mcpTools = append(mcpTools, MCPTool{
			Name:        "approval.resolve",
			Description: "Approve or deny a pending approval request",
			InputSchema: MCPSchema{
				Type: "object",
				Properties: map[string]MCPProp{
					"id":       {Type: "string", Description: "Approval ID (full or 8-char prefix)"},
					"decision": {Type: "string", Description: "Decision: approve or deny"},
				},
				Required: []string{"id", "decision"},
			},
		}, MCPTool{
			Name:        "approval.pending",
			Description: "List all pending approval requests",
			InputSchema: MCPSchema{
				Type:       "object",
				Properties: map[string]MCPProp{},
			},
		})
	}

	// Append virtual grant tools
	mcpTools = append(mcpTools, MCPTool{
		Name:        "grant.create",
		Description: "Create a temporal grant — temporarily allow a tool pattern without approval. Like sudo for agents.",
		InputSchema: MCPSchema{
			Type: "object",
			Properties: map[string]MCPProp{
				"tools":    {Type: "string", Description: "Tool glob pattern (e.g. filesystem.write_*, gmail.*)"},
				"duration": {Type: "string", Description: "Duration (e.g. 30m, 2h, 1h30m)"},
			},
			Required: []string{"tools", "duration"},
		},
	}, MCPTool{
		Name:        "grant.list",
		Description: "List all active temporal grants",
		InputSchema: MCPSchema{
			Type:       "object",
			Properties: map[string]MCPProp{},
		},
	}, MCPTool{
		Name:        "grant.revoke",
		Description: "Revoke an active temporal grant",
		InputSchema: MCPSchema{
			Type: "object",
			Properties: map[string]MCPProp{
				"id": {Type: "string", Description: "Grant ID (full or prefix)"},
			},
			Required: []string{"id"},
		},
	})

	// Append virtual catalog tool
	mcpTools = append(mcpTools, MCPTool{
		Name:        "mesh.catalog",
		Description: "List all available tools grouped by source/category, with policy actions for the current agent",
		InputSchema: MCPSchema{
			Type: "object",
			Properties: map[string]MCPProp{
				"source": {Type: "string", Description: "Filter by source name (e.g. 'filesystem', 'git', 'gmail'). Omit to show all."},
			},
		},
	})

	return map[string]any{"tools": mcpTools}
}

func (s *Server) handleToolsCall(params map[string]any) (any, *rpcError) {
	start := time.Now()
	toolName, _ := params["name"].(string)
	arguments, _ := params["arguments"].(map[string]any)

	if toolName == "" {
		return nil, &rpcError{Code: -32602, Message: "Missing tool name"}
	}

	// Virtual tools — handled before registry lookup, no policy evaluation
	switch toolName {
	case "approval.resolve":
		if s.SupervisorMode && !match.GlobAny(s.SupervisorAgents, s.AgentID) {
			return nil, &rpcError{Code: -32601, Message: "approval.resolve is disabled — supervisor mode is active, approvals are handled by the external supervisor"}
		}
		return s.handleApprovalResolve(arguments)
	case "approval.pending":
		if s.SupervisorMode && !match.GlobAny(s.SupervisorAgents, s.AgentID) {
			return nil, &rpcError{Code: -32601, Message: "approval.pending is disabled — supervisor mode is active"}
		}
		return s.handleApprovalPending()
	case "grant.create":
		return s.handleGrantCreate(arguments)
	case "grant.list":
		return s.handleGrantList()
	case "grant.revoke":
		return s.handleGrantRevoke(arguments)
	case "mesh.catalog":
		return s.handleCatalog(arguments)
	}

	// Look up tool (with CLI fallback for dynamic dispatch)
	tool := s.Registry.Get(toolName)
	if tool == nil {
		tool = s.Registry.ResolveCLI(toolName)
	}
	if tool == nil {
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("Unknown tool: %s", toolName)}
	}

	// Evaluate policy
	decision := s.Policy.Evaluate(s.AgentID, toolName, arguments)
	slog.Info("MCP policy evaluated",
		"agent", s.AgentID, "tool", toolName,
		"action", decision.Action, "rule", decision.Rule,
	)

	if decision.Action == "deny" {
		s.Traces.Record(trace.Entry{
			SessionID:  s.SessionID,
			AgentID:    s.AgentID,
			Tool:       toolName,
			Params:     arguments,
			Policy:     "deny",
			PolicyRule: decision.Rule,
			LatencyMs:  time.Since(start).Milliseconds(),
		})
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Policy denied: %s", decision.Reason)},
			},
		}, nil
	}

	// Check mem7 for past decisions (auto-approve routine patterns)
	if decision.Action == "human_approval" && s.Approvals != nil {
		if res := s.Approvals.TryAutoResolve(s.AgentID, toolName); res != nil {
			slog.Info("mem7 auto-approve",
				"agent", s.AgentID, "tool", toolName, "reason", res.Reasoning)
			decision.Action = "allow"
			decision.Rule = "supervisor:mem7"
			decision.Reason = res.Reasoning
		}
	}

	if decision.Action == "human_approval" {
		entry := trace.Entry{
			SessionID:  s.SessionID,
			AgentID:    s.AgentID,
			Tool:       toolName,
			Params:     arguments,
			Policy:     "human_approval",
			PolicyRule: decision.Rule,
		}

		// Try TTY prompt first (interactive terminal), fall back to approval store
		approved, resolvedBy := s.promptTTY(toolName, arguments)
		if approved != nil {
			s.Traces.Record(entry)
			if *approved {
				entry.ApprovalStatus = string(approval.StatusApproved)
				entry.ApprovedBy = resolvedBy
				s.Traces.Update(entry.TraceID, func(e *trace.Entry) {
					e.ApprovalStatus = string(approval.StatusApproved)
					e.ApprovedBy = resolvedBy
				})

				result, statusCode, err := s.Handler.Forward(tool, arguments, "")
				inTok, outTok, tokSrc := resolveMCPTokens(toolName, arguments, result)
				s.Traces.Update(entry.TraceID, func(e *trace.Entry) {
					e.StatusCode = statusCode
					e.LatencyMs = time.Since(start).Milliseconds()
					e.EstimatedInputTokens = inTok
					e.EstimatedOutputTokens = outTok
					e.TokensSource = tokSrc
					if err != nil {
						e.Error = err.Error()
					}
				})
				if err != nil {
					return map[string]any{
						"content": []map[string]any{
							{"type": "text", "text": fmt.Sprintf("Backend error: %s", err.Error())},
						},
					}, nil
				}
				resultJSON, _ := json.MarshalIndent(result, "", "  ")
				return map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": string(resultJSON)},
					},
				}, nil
			}
			// Denied via TTY
			s.Traces.Update(entry.TraceID, func(e *trace.Entry) {
				e.ApprovalStatus = string(approval.StatusDenied)
				e.ApprovedBy = resolvedBy
			})
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "Approval denied by " + resolvedBy},
				},
			}, nil
		}

		// No TTY available — block on approval store, resolve via HTTP API or mesh CLI
		if s.Approvals == nil {
			s.Traces.Record(entry)
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "This action requires human approval but no approval store is configured."},
				},
			}, nil
		}

		pending := s.Approvals.Submit(s.AgentID, toolName, decision.Rule, arguments, "")
		entry.ApprovalID = pending.ID
		s.Traces.Record(entry)
		pending.TraceID = entry.TraceID

		shortID := pending.ID[:8]

		if s.SupervisorMode {
			slog.Info("approval pending (supervisor will resolve)",
				"approval_id", shortID, "agent", s.AgentID, "tool", toolName)

			// Block until supervisor resolves — agent waits
			resolution := <-pending.Result
			return s.handleResolution(pending, entry, resolution, toolName, arguments)
		}

		slog.Info("approval pending (non-blocking)",
			"approval_id", shortID, "agent", s.AgentID, "tool", toolName,
			"resolve_via", fmt.Sprintf("approval.resolve {id: %s, decision: approve} OR mesh approve %s", shortID, shortID))

		// Non-blocking: return immediately, let the caller resolve via approval.resolve tool
		remaining := pending.Remaining(s.Approvals.Timeout())
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf(
					"Approval required (id: %s). Tool: %s. Timeout: %ds.\n"+
						"Use approval.resolve with id=%s and decision=approve or deny.",
					shortID, toolName, int(remaining.Seconds()), shortID)},
			},
		}, nil
	}

	// Forward to backend
	result, statusCode, err := s.Handler.Forward(tool, arguments, "")
	inTok, outTok, tokSrc := resolveMCPTokens(toolName, arguments, result)

	// Trace
	entry := trace.Entry{
		SessionID:             s.SessionID,
		AgentID:               s.AgentID,
		Tool:                  toolName,
		Params:                arguments,
		Policy:                "allow",
		PolicyRule:            decision.Rule,
		StatusCode:            statusCode,
		LatencyMs:             time.Since(start).Milliseconds(),
		EstimatedInputTokens:  inTok,
		EstimatedOutputTokens: outTok,
		TokensSource:          tokSrc,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	s.Traces.Record(entry)

	if err != nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Backend error: %s", err.Error())},
			},
		}, nil
	}

	// Serialize result as text
	resultJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Failed to serialize result: %s", err.Error())},
			},
		}, nil
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(resultJSON)},
		},
	}, nil
}

func (s *Server) handleApprovalResolve(args map[string]any) (any, *rpcError) {
	id, _ := args["id"].(string)
	decision, _ := args["decision"].(string)

	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "Missing 'id' parameter"}
	}
	if decision != "approve" && decision != "deny" {
		return nil, &rpcError{Code: -32602, Message: "Invalid 'decision': must be 'approve' or 'deny'"}
	}

	if s.Approvals == nil {
		return nil, &rpcError{Code: -32000, Message: "No approval store configured"}
	}

	pa := s.Approvals.Get(id)
	if pa == nil {
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("Approval not found: %s", id)}
	}

	resolvedBy := "mcp:" + s.AgentID

	if decision == "deny" {
		if err := s.Approvals.Deny(id, resolvedBy); err != nil {
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Cannot resolve: %s", err.Error())},
				},
			}, nil
		}
		if pa.TraceID != "" {
			s.Traces.Update(pa.TraceID, func(e *trace.Entry) {
				e.ApprovalStatus = string(approval.StatusDenied)
				e.ApprovedBy = resolvedBy
				e.ApprovalMs = time.Since(pa.CreatedAt).Milliseconds()
			})
		}
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Denied. Tool %s was not executed.", pa.Tool)},
			},
		}, nil
	}

	// Approve: resolve then replay the original tool call
	if err := s.Approvals.Approve(id, resolvedBy); err != nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Cannot resolve: %s", err.Error())},
			},
		}, nil
	}

	tool := s.Registry.Get(pa.Tool)
	if tool == nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Approved but tool %s no longer exists in registry.", pa.Tool)},
			},
		}, nil
	}

	result, statusCode, err := s.Handler.Forward(tool, pa.Params, "")
	if pa.TraceID != "" {
		s.Traces.Update(pa.TraceID, func(e *trace.Entry) {
			e.ApprovalStatus = string(approval.StatusApproved)
			e.ApprovedBy = resolvedBy
			e.ApprovalMs = time.Since(pa.CreatedAt).Milliseconds()
			e.StatusCode = statusCode
			e.EstimatedInputTokens = trace.EstimateTokens(pa.Params)
			e.EstimatedOutputTokens = trace.EstimateTokens(result)
			if err != nil {
				e.Error = err.Error()
			}
		})
	}

	if err != nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Approved but backend error: %s", err.Error())},
			},
		}, nil
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(resultJSON)},
		},
	}, nil
}

// handleResolution processes a supervisor's resolution — replays the tool call if approved.
func (s *Server) handleResolution(
	pending *approval.PendingApproval,
	entry trace.Entry,
	resolution approval.Resolution,
	toolName string,
	arguments map[string]any,
) (any, *rpcError) {
	s.Traces.Update(entry.TraceID, func(e *trace.Entry) {
		e.ApprovalStatus = string(resolution.Status)
		e.ApprovedBy = resolution.ResolvedBy
		e.ApprovalMs = time.Since(pending.CreatedAt).Milliseconds()
		e.SupervisorReasoning = resolution.Reasoning
		e.SupervisorConfidence = resolution.Confidence
	})

	switch resolution.Status {
	case approval.StatusApproved:
		tool := s.Registry.Get(toolName)
		if tool == nil {
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Approved by supervisor but tool %s no longer exists.", toolName)},
				},
			}, nil
		}
		result, statusCode, err := s.Handler.Forward(tool, arguments, entry.TraceID)
		s.Traces.Update(entry.TraceID, func(e *trace.Entry) {
			e.StatusCode = statusCode
			e.LatencyMs = time.Since(pending.CreatedAt).Milliseconds()
			e.EstimatedInputTokens = trace.EstimateTokens(arguments)
			e.EstimatedOutputTokens = trace.EstimateTokens(result)
			if err != nil {
				e.Error = err.Error()
			}
		})
		if err != nil {
			return map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Approved by supervisor but backend error: %s", err.Error())},
				},
			}, nil
		}
		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(resultJSON)},
			},
		}, nil

	case approval.StatusDenied:
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("Denied by supervisor: %s", resolution.Reasoning)},
			},
		}, nil

	case approval.StatusTimeout:
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Approval timed out — no supervisor or human resolved it."},
			},
		}, nil
	}

	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf("Unexpected approval status: %s", resolution.Status)},
		},
	}, nil
}

func (s *Server) handleApprovalPending() (any, *rpcError) {
	if s.Approvals == nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "No approval store configured."},
			},
		}, nil
	}

	pending := s.Approvals.ListPending()
	if len(pending) == 0 {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "No pending approvals."},
			},
		}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Pending approvals (%d):\n", len(pending))
	for _, pa := range pending {
		age := time.Since(pa.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(&sb, "- ID: %s  tool: %s  agent: %s  age: %s\n", pa.ID[:8], pa.Tool, pa.AgentID, age)
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": sb.String()},
		},
	}, nil
}

func (s *Server) handleGrantCreate(args map[string]any) (any, *rpcError) {
	if s.Handler == nil || s.Handler.Grants == nil {
		return nil, &rpcError{Code: -32603, Message: "Grant store not configured"}
	}
	tools, _ := args["tools"].(string)
	duration, _ := args["duration"].(string)
	if tools == "" || duration == "" {
		return nil, &rpcError{Code: -32602, Message: "tools and duration are required"}
	}
	dur, err := time.ParseDuration(duration)
	if err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid duration: " + err.Error()}
	}
	g := s.Handler.Grants.Add(s.AgentID, tools, "mcp:"+s.AgentID, dur)
	slog.Info("grant created via MCP",
		"id", g.ID, "agent", g.Agent, "tools", g.Tools, "duration", duration)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fmt.Sprintf("Grant created: %s\n  agent: %s\n  tools: %s\n  expires: %s\n  remaining: %s",
				g.ID, g.Agent, g.Tools, g.ExpiresAt.Format(time.RFC3339), g.Remaining().Truncate(time.Second))},
		},
	}, nil
}

func (s *Server) handleGrantList() (any, *rpcError) {
	if s.Handler == nil || s.Handler.Grants == nil {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "No grant store configured."},
			},
		}, nil
	}
	grants := s.Handler.Grants.List()
	if len(grants) == 0 {
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "No active grants."},
			},
		}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Active grants (%d):\n", len(grants))
	for _, g := range grants {
		fmt.Fprintf(&sb, "- ID: %s  tools: %s  agent: %s  remaining: %s\n",
			g.ID[:8], g.Tools, g.Agent, g.Remaining().Truncate(time.Second))
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": sb.String()},
		},
	}, nil
}

func (s *Server) handleGrantRevoke(args map[string]any) (any, *rpcError) {
	if s.Handler == nil || s.Handler.Grants == nil {
		return nil, &rpcError{Code: -32603, Message: "Grant store not configured"}
	}
	id, _ := args["id"].(string)
	if id == "" {
		return nil, &rpcError{Code: -32602, Message: "id is required"}
	}
	if !s.Handler.Grants.Revoke(id) {
		return nil, &rpcError{Code: -32602, Message: "grant not found: " + id}
	}
	slog.Info("grant revoked via MCP", "id", id)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": "Grant revoked: " + id},
		},
	}, nil
}

func (s *Server) handleCatalog(args map[string]any) (any, *rpcError) {
	sourceFilter, _ := args["source"].(string)
	sourceFilter = strings.ToLower(sourceFilter)

	type catalogEntry struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Action      string `json:"action"`
	}
	type catalogGroup struct {
		Source  string         `json:"source"`
		Runtime string         `json:"runtime,omitempty"`
		Count   int            `json:"count"`
		Tools   []catalogEntry `json:"tools"`
	}

	groups := make(map[string]*catalogGroup)

	// Registry tools
	for _, t := range s.Registry.All() {
		var groupKey, sourceType string
		switch t.Source {
		case "mcp":
			groupKey = t.MCPServer
			sourceType = "mcp"
		case "cli":
			if i := strings.IndexByte(t.Name, '.'); i > 0 {
				groupKey = t.Name[:i]
			} else {
				groupKey = t.Name
			}
			sourceType = "cli"
		default:
			groupKey = "openapi"
			sourceType = "openapi"
		}

		if sourceFilter != "" && strings.ToLower(groupKey) != sourceFilter {
			continue
		}

		action := s.Policy.Evaluate(s.AgentID, t.Name, nil).Action

		g, ok := groups[groupKey]
		if !ok {
			g = &catalogGroup{Source: sourceType}
			groups[groupKey] = g
		}
		g.Tools = append(g.Tools, catalogEntry{
			Name:        t.Name,
			Description: t.Description,
			Action:      action,
		})
	}

	// Virtual tools (mesh group)
	if sourceFilter == "" || sourceFilter == "mesh" {
		g := &catalogGroup{Source: "virtual", Tools: []catalogEntry{
			{Name: "approval.resolve", Description: "Approve or deny a pending approval request", Action: "allow"},
			{Name: "approval.pending", Description: "List all pending approval requests", Action: "allow"},
			{Name: "grant.create", Description: "Create a temporal grant", Action: "allow"},
			{Name: "grant.list", Description: "List all active temporal grants", Action: "allow"},
			{Name: "grant.revoke", Description: "Revoke an active temporal grant", Action: "allow"},
			{Name: "mesh.catalog", Description: "This tool", Action: "allow"},
		}}
		groups["mesh"] = g
	}

	// Enrich MCP groups with runtime info from manager
	if s.MCPManager != nil {
		for _, client := range s.MCPManager.All() {
			g, ok := groups[client.Name]
			if !ok {
				continue
			}
			switch client.Transport {
			case "stdio":
				g.Runtime = client.Command + " " + strings.Join(client.Args, " ")
			case "sse":
				g.Runtime = client.URL
			}
		}
	}

	// Enrich CLI groups with binary path
	for key, g := range groups {
		if g.Source == "cli" && len(g.Tools) > 0 {
			// Get CLIMeta from first tool in group
			if t := s.Registry.Get(g.Tools[0].Name); t != nil && t.CLIMeta != nil {
				g.Runtime = t.CLIMeta.Bin
			} else if t := s.Registry.Get(key + ".__dispatch"); t != nil && t.CLIMeta != nil {
				g.Runtime = t.CLIMeta.Bin
			}
		}
	}

	// Sort tools within each group, compute counts
	totalTools := 0
	for _, g := range groups {
		sort.Slice(g.Tools, func(i, j int) bool { return g.Tools[i].Name < g.Tools[j].Name })
		g.Count = len(g.Tools)
		totalTools += g.Count
	}

	// Sort group keys
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build ordered output
	orderedGroups := make(map[string]*catalogGroup, len(groups))
	for _, k := range keys {
		orderedGroups[k] = groups[k]
	}

	result := map[string]any{
		"summary": fmt.Sprintf("%d tools across %d sources", totalTools, len(groups)),
		"groups":  orderedGroups,
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(out)},
		},
	}, nil
}

func (s *Server) writeResponse(w io.Writer, resp rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		slog.Error("MCP server: failed to marshal response", "error", err)
		return
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		slog.Error("MCP server: failed to write response", "error", err)
	}
}

// promptTTY tries to prompt the user directly via /dev/tty.
// Returns (approved *bool, resolvedBy string). If TTY is unavailable, returns (nil, "").
func (s *Server) promptTTY(toolName string, params map[string]any) (*bool, string) {
	if runtime.GOOS == "windows" {
		return nil, ""
	}

	// Skip TTY prompt when stdin is a pipe (MCP stdio mode via Claude Code / agent).
	// The TTY would open the agent-mesh terminal, not the caller's terminal,
	// causing an invisible blocking prompt.
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		slog.Debug("stdin is a pipe, skipping TTY prompt — use HTTP API or mesh CLI to approve")
		return nil, ""
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		slog.Debug("TTY not available, falling back to approval store", "error", err)
		return nil, ""
	}
	defer tty.Close()

	// Display approval prompt
	fmt.Fprintf(tty, "\n\033[1;33m>> APPROVAL REQUIRED\033[0m\n")
	fmt.Fprintf(tty, "   agent: %s\n", s.AgentID)
	fmt.Fprintf(tty, "   tool:  %s\n", toolName)
	for k, v := range params {
		str := fmt.Sprintf("%v", v)
		if len(str) > 80 {
			str = str[:80] + "..."
		}
		fmt.Fprintf(tty, "   %s: %s\n", k, str)
	}
	fmt.Fprintf(tty, "\n   \033[1m[a]pprove / [d]eny ?\033[0m ")

	// Read response
	reader := bufio.NewReader(tty)
	line, _ := reader.ReadString('\n')
	input := strings.TrimSpace(strings.ToLower(line))

	resolvedBy := "tty:" + os.Getenv("USER")
	switch input {
	case "a", "approve":
		fmt.Fprintf(tty, "   \033[32mApproved\033[0m\n\n")
		approved := true
		return &approved, resolvedBy
	default:
		fmt.Fprintf(tty, "   \033[31mDenied\033[0m\n\n")
		approved := false
		return &approved, resolvedBy
	}
}

func (s *Server) writeError(w io.Writer, id any, code int, msg string) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	}
	s.writeResponse(w, resp)
}

// resolveMCPTokens returns real provider token counts when the tool is a known
// LLM endpoint, otherwise the chars/4 estimate. The third return value is
// "real" or "estimate".
func resolveMCPTokens(toolName string, params map[string]any, result any) (int, int, string) {
	if in, out, ok := trace.ExtractLLMTokens(toolName, result); ok {
		return in, out, "real"
	}
	return trace.EstimateTokens(params), trace.EstimateTokens(result), "estimate"
}
