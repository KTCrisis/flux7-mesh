package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KTCrisis/agent-mesh/approval"
	"github.com/KTCrisis/agent-mesh/config"
	meshexec "github.com/KTCrisis/agent-mesh/exec"
	"github.com/KTCrisis/agent-mesh/grant"
	"github.com/KTCrisis/agent-mesh/internal/match"
	"github.com/KTCrisis/agent-mesh/policy"
	"github.com/KTCrisis/agent-mesh/ratelimit"
	"github.com/KTCrisis/agent-mesh/registry"
	"github.com/KTCrisis/agent-mesh/supervisor"
	"github.com/KTCrisis/agent-mesh/trace"
)

// ToolCallRequest is the JSON body sent by the agent.
type ToolCallRequest struct {
	Params map[string]any `json:"params"`
}

// ToolCallResponse is returned to the agent.
type ToolCallResponse struct {
	Result     any    `json:"result,omitempty"`
	TraceID    string `json:"trace_id"`
	ApprovalID string `json:"approval_id,omitempty"`
	Policy     string `json:"policy"`
	LatencyMs  int64  `json:"latency_ms"`
	Error      string `json:"error,omitempty"`
}

// MCPForwarder is the interface for forwarding calls to upstream MCP servers.
type MCPForwarder interface {
	CallTool(ctx context.Context, serverName string, toolName string, arguments map[string]any) (any, error)
	ServerStatuses() any
}

// Handler is the HTTP handler for the sidecar proxy.
type Handler struct {
	Registry     *registry.Registry
	Policy       *policy.Engine
	Traces       *trace.Store
	Approvals    *approval.Store
	RateLimiter  *ratelimit.Limiter
	Grants       *grant.Store
	Client        *http.Client
	MCPForwarder  MCPForwarder
	CLIRunner     *meshexec.Runner
	SupervisorCfg config.SupervisorConfig
	MCPHTTPHandler http.Handler // MCP Streamable HTTP transport (POST/DELETE /mcp)

	// Build info (populated from main.go ldflags-injected vars).
	Version   string
	Commit    string
	BuildDate string
}

func NewHandler(reg *registry.Registry, pol *policy.Engine, traces *trace.Store) *Handler {
	return &Handler{
		Registry: reg,
		Policy:   pol,
		Traces:   traces,
		Client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ServeHTTP routes requests to the appropriate handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/tool/"):
		h.handleToolCall(w, r)
	case r.Method == "GET" && r.URL.Path == "/tools":
		h.handleListTools(w, r)
	case r.Method == "GET" && r.URL.Path == "/traces":
		h.handleTraces(w, r)
	case r.Method == "GET" && r.URL.Path == "/otel-traces":
		h.handleOTELTraces(w, r)
	case r.Method == "GET" && r.URL.Path == "/mcp-servers":
		h.handleMCPServers(w, r)
	case r.Method == "GET" && r.URL.Path == "/approvals":
		h.handleListApprovals(w, r)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/approvals/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/approvals/"), "/"):
		h.handleGetApproval(w, r)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/approve") && strings.HasPrefix(r.URL.Path, "/approvals/"):
		h.handleApproveAction(w, r)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/deny") && strings.HasPrefix(r.URL.Path, "/approvals/"):
		h.handleDenyAction(w, r)
	case r.Method == "GET" && r.URL.Path == "/grants":
		h.handleListGrants(w, r)
	case r.Method == "POST" && r.URL.Path == "/grants":
		h.handleCreateGrant(w, r)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/grants/"):
		h.handleRevokeGrant(w, r)
	case r.Method == "GET" && r.URL.Path == "/sessions":
		h.handleListSessions(w, r)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sessions/"):
		h.handleSessionEvents(w, r)
	case r.Method == "GET" && r.URL.Path == "/health":
		h.handleHealth(w, r)
	case r.Method == "GET" && r.URL.Path == "/version":
		h.handleVersion(w, r)
	case r.Method == "GET" && r.URL.Path == "/metrics":
		h.handleMetrics(w, r)
	case r.URL.Path == "/mcp" && h.MCPHTTPHandler != nil:
		h.MCPHTTPHandler.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handleToolCall(w http.ResponseWriter, r *http.Request) {
	toolName := strings.TrimPrefix(r.URL.Path, "/tool/")
	agentID := extractAgentID(r)
	traceID := extractTraceID(r)
	if traceID == "" {
		traceID = trace.NewID()
	}
	sessionID := extractSessionID(r)
	start := time.Now()

	// 1. Parse request body
	var req ToolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, ToolCallResponse{Error: "invalid JSON body", Policy: "error"})
		return
	}

	// 2. Look up tool in registry (with CLI fallback for dynamic dispatch)
	tool := h.Registry.Get(toolName)
	if tool == nil {
		tool = h.Registry.ResolveCLI(toolName)
	}
	if tool == nil {
		writeJSON(w, 404, ToolCallResponse{Error: fmt.Sprintf("unknown tool: %s", toolName), Policy: "error"})
		return
	}

	// 3. Rate limit check (before policy — fail fast)
	if h.RateLimiter != nil {
		paramsKey := fmt.Sprintf("%v", req.Params)
		// Pre-check with a preliminary policy match to get the policy name
		preDecision := h.Policy.Evaluate(agentID, toolName, req.Params)
		if err := h.RateLimiter.Check(agentID, preDecision.Rule, toolName, paramsKey); err != nil {
			entry := trace.Entry{
				TraceID:    traceID,
				SessionID:  sessionID,
				AgentID:    agentID,
				Tool:       toolName,
				Params:     req.Params,
				Policy:     "rate_limited",
				PolicyRule: preDecision.Rule,
				LatencyMs:  time.Since(start).Milliseconds(),
				Error:      err.Error(),
			}
			h.Traces.Record(entry)
			writeJSON(w, 429, ToolCallResponse{
				TraceID: entry.TraceID,
				Policy:  "rate_limited",
				Error:   err.Error(),
			})
			return
		}
	}

	// 4. Evaluate policy
	decision := h.Policy.Evaluate(agentID, toolName, req.Params)
	slog.Info("policy evaluated",
		"agent", agentID, "tool", toolName,
		"action", decision.Action, "rule", decision.Rule,
	)

	if decision.Action == "deny" {
		entry := trace.Entry{
			TraceID:    traceID,
			SessionID:  sessionID,
			AgentID:    agentID,
			Tool:       toolName,
			Params:     req.Params,
			Policy:     "deny",
			PolicyRule: decision.Rule,
			LatencyMs:  time.Since(start).Milliseconds(),
		}
		h.Traces.Record(entry)
		writeJSON(w, 403, ToolCallResponse{
			TraceID: entry.TraceID,
			Policy:  "deny",
			Error:   decision.Reason,
		})
		return
	}

	// 5. Check temporal grants (bypass approval if granted)
	if decision.Action == "human_approval" && h.Grants != nil {
		if g := h.Grants.Check(agentID, toolName); g != nil {
			slog.Info("grant override",
				"agent", agentID, "tool", toolName,
				"grant", g.ID, "remaining", g.Remaining().Truncate(time.Second))
			decision.Action = "allow"
			decision.Rule = "grant:" + g.ID
			decision.Reason = fmt.Sprintf("temporal grant %s (expires %s)", g.ID, g.ExpiresAt.Format(time.RFC3339))
		}
	}

	// Check mem7 for past decisions (auto-approve routine patterns)
	if decision.Action == "human_approval" && h.Approvals != nil {
		if res := h.Approvals.TryAutoResolve(agentID, toolName); res != nil {
			slog.Info("mem7 auto-approve",
				"agent", agentID, "tool", toolName, "reason", res.Reasoning)
			decision.Action = "allow"
			decision.Rule = "supervisor:mem7"
			decision.Reason = res.Reasoning
		}
	}

	if decision.Action == "human_approval" {
		if h.Approvals == nil {
			// Fallback: no approval store configured
			entry := trace.Entry{
				TraceID:    traceID,
				SessionID:  sessionID,
				AgentID:    agentID,
				Tool:       toolName,
				Params:     req.Params,
				Policy:     "human_approval",
				PolicyRule: decision.Rule,
				LatencyMs:  time.Since(start).Milliseconds(),
			}
			h.Traces.Record(entry)
			writeJSON(w, 202, ToolCallResponse{
				TraceID: entry.TraceID,
				Policy:  "human_approval",
				Error:   "action requires human approval",
			})
			return
		}

		callbackURL := r.Header.Get("X-Callback-URL")
		pending := h.Approvals.Submit(agentID, toolName, decision.Rule, req.Params, callbackURL)

		entry := trace.Entry{
			TraceID:    traceID,
			SessionID:  sessionID,
			AgentID:    agentID,
			Tool:       toolName,
			Params:     req.Params,
			Policy:     "human_approval",
			PolicyRule: decision.Rule,
			ApprovalID: pending.ID,
		}
		h.Traces.Record(entry)

		slog.Info("awaiting human approval",
			"approval_id", pending.ID, "agent", agentID, "tool", toolName)

		// Block until resolved
		resolution := <-pending.Result
		approvalMs := time.Since(start).Milliseconds()

		h.Traces.Update(entry.TraceID, func(e *trace.Entry) {
			e.ApprovalStatus = string(resolution.Status)
			e.ApprovedBy = resolution.ResolvedBy
			e.ApprovalMs = approvalMs
			e.SupervisorReasoning = resolution.Reasoning
			e.SupervisorConfidence = resolution.Confidence
		})

		switch resolution.Status {
		case approval.StatusApproved:
			if h.RateLimiter != nil {
				h.RateLimiter.Record(agentID, toolName, fmt.Sprintf("%v", req.Params))
			}
			result, statusCode, err := h.Forward(tool, req.Params, traceID)
			totalMs := time.Since(start).Milliseconds()
			inTok, outTok, tokSrc := resolveTokens(toolName, req.Params, result)
			h.Traces.Update(entry.TraceID, func(e *trace.Entry) {
				e.StatusCode = statusCode
				e.LatencyMs = totalMs
				e.EstimatedInputTokens = inTok
				e.EstimatedOutputTokens = outTok
				e.TokensSource = tokSrc
				if err != nil {
					e.Error = err.Error()
				}
			})
			resp := ToolCallResponse{
				Result:     result,
				TraceID:    entry.TraceID,
				ApprovalID: pending.ID,
				Policy:     "human_approval",
				LatencyMs:  totalMs,
			}
			if err != nil {
				resp.Error = err.Error()
				writeJSON(w, 502, resp)
				return
			}
			writeJSON(w, 200, resp)

		case approval.StatusDenied:
			writeJSON(w, 403, ToolCallResponse{
				TraceID:    entry.TraceID,
				ApprovalID: pending.ID,
				Policy:     "human_approval",
				LatencyMs:  approvalMs,
				Error:      "approval denied by " + resolution.ResolvedBy,
			})

		case approval.StatusTimeout:
			writeJSON(w, 408, ToolCallResponse{
				TraceID:    entry.TraceID,
				ApprovalID: pending.ID,
				Policy:     "human_approval",
				LatencyMs:  approvalMs,
				Error:      "approval timed out",
			})
		}
		return
	}

	// 5. Record rate limit usage
	if h.RateLimiter != nil {
		h.RateLimiter.Record(agentID, toolName, fmt.Sprintf("%v", req.Params))
	}

	// 6. Forward to backend
	result, statusCode, err := h.Forward(tool, req.Params, traceID)
	latency := time.Since(start).Milliseconds()
	inTok, outTok, tokSrc := resolveTokens(toolName, req.Params, result)

	// 5. Trace
	entry := trace.Entry{
		TraceID:               traceID,
		SessionID:             sessionID,
		AgentID:               agentID,
		Tool:                  toolName,
		Params:                req.Params,
		Policy:                "allow",
		PolicyRule:            decision.Rule,
		StatusCode:            statusCode,
		LatencyMs:             latency,
		EstimatedInputTokens:  inTok,
		EstimatedOutputTokens: outTok,
		TokensSource:          tokSrc,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	h.Traces.Record(entry)

	// 6. Respond
	resp := ToolCallResponse{
		Result:    result,
		TraceID:   entry.TraceID,
		Policy:    "allow",
		LatencyMs: latency,
	}
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, 502, resp)
		return
	}
	writeJSON(w, 200, resp)
}

// Forward sends the request to the appropriate backend (HTTP, MCP, or CLI).
// traceID is propagated to HTTP backends via Traceparent and X-Trace-Id headers.
func (h *Handler) Forward(tool *registry.Tool, params map[string]any, traceID string) (any, int, error) {
	switch tool.Source {
	case "mcp":
		return h.forwardMCP(tool, params)
	case "cli":
		return h.forwardCLI(tool, params)
	default:
		return h.forwardHTTP(tool, params, traceID)
	}
}

// forwardHTTP sends the request to a REST backend.
func (h *Handler) forwardHTTP(tool *registry.Tool, params map[string]any, traceID string) (any, int, error) {
	// Build URL with path params (URL-encoded)
	reqURL := tool.BaseURL + tool.Path
	for k, v := range params {
		placeholder := "{" + k + "}"
		if strings.Contains(reqURL, placeholder) {
			reqURL = strings.Replace(reqURL, placeholder, url.PathEscape(fmt.Sprintf("%v", v)), 1)
		}
	}

	// Build query params for GET/DELETE (URL-encoded)
	var body io.Reader
	if tool.Method == "GET" || tool.Method == "DELETE" {
		q := url.Values{}
		for k, v := range params {
			if !strings.Contains(tool.Path, "{"+k+"}") {
				q.Set(k, fmt.Sprintf("%v", v))
			}
		}
		if encoded := q.Encode(); encoded != "" {
			sep := "?"
			if strings.Contains(reqURL, "?") {
				sep = "&"
			}
			reqURL += sep + encoded
		}
	} else {
		// POST/PUT/PATCH: send params as JSON body
		jsonBody, err := json.Marshal(params)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal params: %w", err)
		}
		body = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(tool.Method, reqURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if traceID != "" {
		req.Header.Set("X-Trace-Id", traceID)
		req.Header.Set("Traceparent", fmt.Sprintf("00-%s-0000000000000000-01", traceID))
	}
	for k, v := range tool.Headers {
		req.Header.Set(k, v)
	}

	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("backend error: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	var result any
	if err := json.Unmarshal(respBody, &result); err != nil {
		// Non-JSON response — return as string
		result = string(respBody)
	}

	return result, resp.StatusCode, nil
}

// forwardMCP forwards the call to an upstream MCP server.
func (h *Handler) forwardMCP(tool *registry.Tool, params map[string]any) (any, int, error) {
	if h.MCPForwarder == nil {
		return nil, 0, fmt.Errorf("no MCP forwarder configured")
	}

	// Strip namespace prefix to get the original tool name
	originalName := strings.TrimPrefix(tool.Name, tool.MCPServer+".")

	ctx := context.Background()
	result, err := h.MCPForwarder.CallTool(ctx, tool.MCPServer, originalName, params)
	if err != nil {
		return nil, 502, err
	}
	return result, 200, nil
}

// forwardCLI executes a CLI command and returns the result.
func (h *Handler) forwardCLI(tool *registry.Tool, params map[string]any) (any, int, error) {
	if h.CLIRunner == nil {
		return nil, 0, fmt.Errorf("no CLI runner configured")
	}
	meta := tool.CLIMeta
	if meta == nil {
		return nil, 0, fmt.Errorf("tool %s has no CLI metadata", tool.Name)
	}

	command, args := meshexec.ExtractCommand(params, meta)

	ctx := context.Background()
	result, err := h.CLIRunner.Run(ctx, meta, command, args)
	if err != nil {
		statusCode := 500
		if result != nil && result.ExitCode != 0 {
			statusCode = 422
		}
		return result, statusCode, err
	}

	statusCode := 200
	if result.ExitCode != 0 {
		statusCode = 422
	}
	return result, statusCode, nil
}

func (h *Handler) handleListTools(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, h.Registry.All())
}

func (h *Handler) handleTraces(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	tool := r.URL.Query().Get("tool")
	writeJSON(w, 200, h.Traces.Query(agent, tool, 100))
}

// handleOTELTraces returns recent trace entries converted to OTLP JSON format.
// Query params: agent, tool, limit (default 200).
// Returns a single resourceSpans payload compatible with OTLP consumers.
func (h *Handler) handleOTELTraces(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	tool := r.URL.Query().Get("tool")
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	entries := h.Traces.Query(agent, tool, limit)
	writeJSON(w, 200, trace.EntriesToOTLP(entries, "agent-mesh"))
}

func (h *Handler) handleMCPServers(w http.ResponseWriter, _ *http.Request) {
	if h.MCPForwarder == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, h.MCPForwarder.ServerStatuses())
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status":  "ok",
		"tools":   len(h.Registry.All()),
		"traces":  h.Traces.Stats(),
		"version": h.Version,
	})
}

// handleMetrics exposes operational counters in Prometheus exposition format.
//
// Currently published:
//   - agent_mesh_mem7_writes_attempted_total
//   - agent_mesh_mem7_writes_succeeded_total
//   - agent_mesh_mem7_writes_failed_total
//
// When mem7 is not configured the counters stay at zero — the endpoint is
// always served, so a scraper can detect a misconfiguration (attempts > 0,
// failed == attempts) without special-casing the disabled state.
func (h *Handler) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	var stats approval.MemoryWriterStats
	if h.Approvals != nil {
		stats = h.Approvals.MemoryWriter.Stats()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP agent_mesh_mem7_writes_attempted_total Total mem7 decision write attempts.\n")
	fmt.Fprintf(w, "# TYPE agent_mesh_mem7_writes_attempted_total counter\n")
	fmt.Fprintf(w, "agent_mesh_mem7_writes_attempted_total %d\n", stats.Attempted)
	fmt.Fprintf(w, "# HELP agent_mesh_mem7_writes_succeeded_total Successful mem7 decision writes (HTTP 2xx/3xx).\n")
	fmt.Fprintf(w, "# TYPE agent_mesh_mem7_writes_succeeded_total counter\n")
	fmt.Fprintf(w, "agent_mesh_mem7_writes_succeeded_total %d\n", stats.Succeeded)
	fmt.Fprintf(w, "# HELP agent_mesh_mem7_writes_failed_total Failed mem7 decision writes (marshal, transport, or HTTP >= 400).\n")
	fmt.Fprintf(w, "# TYPE agent_mesh_mem7_writes_failed_total counter\n")
	fmt.Fprintf(w, "agent_mesh_mem7_writes_failed_total %d\n", stats.Failed)
}

// handleVersion returns the build info injected at compile time via ldflags.
// Empty strings default to "dev"/"none"/"unknown" so the response is always populated.
func (h *Handler) handleVersion(w http.ResponseWriter, _ *http.Request) {
	version := h.Version
	if version == "" {
		version = "dev"
	}
	commit := h.Commit
	if commit == "" {
		commit = "none"
	}
	date := h.BuildDate
	if date == "" {
		date = "unknown"
	}
	writeJSON(w, 200, map[string]string{
		"version": version,
		"commit":  commit,
		"date":    date,
	})
}

func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, 200, h.Traces.QuerySessions(limit))
}

func (h *Handler) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if sessionID == "" {
		writeJSON(w, 400, map[string]string{"error": "missing session ID"})
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, 200, h.Traces.QueryBySession(sessionID, limit))
}

// extractTraceID reads a trace ID from incoming headers.
// Supports W3C Traceparent (extracts trace-id field) and X-Trace-Id.
// Returns empty string if none provided (trace store will generate one).
func extractTraceID(r *http.Request) string {
	// W3C Traceparent: "00-<trace-id>-<parent-id>-<flags>"
	if tp := r.Header.Get("Traceparent"); tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) >= 2 && len(parts[1]) == 32 {
			return parts[1]
		}
	}
	// Fallback: X-Trace-Id header (must be 32 hex chars per W3C)
	if id := r.Header.Get("X-Trace-Id"); len(id) == 32 && isHexString(id) {
		return id
	}
	return ""
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// extractSessionID reads an optional session ID from the X-Session-Id header.
// Returns empty string if not provided (no auto-generation).
func extractSessionID(r *http.Request) string {
	return r.Header.Get("X-Session-Id")
}

// extractAgentID reads the agent ID from the Authorization header.
// Format: "Bearer agent:<agent-id>" or just "Bearer <agent-id>"
func extractAgentID(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	auth = strings.TrimPrefix(auth, "Bearer ")
	auth = strings.TrimPrefix(auth, "agent:")
	if auth == "" {
		return "anonymous"
	}
	return auth
}

// --- Approval endpoints ---

type approvalView struct {
	ID            string         `json:"id"`
	AgentID       string         `json:"agent_id"`
	Tool          string         `json:"tool"`
	Params        map[string]any `json:"params"`
	PolicyRule    string         `json:"policy_rule"`
	Status        string         `json:"status"`
	CreatedAt     time.Time      `json:"created_at"`
	Remaining     string         `json:"remaining,omitempty"`
	ResolvedBy    string         `json:"resolved_by,omitempty"`
	ResolvedAt    *time.Time     `json:"resolved_at,omitempty"`
	Reasoning     string         `json:"reasoning,omitempty"`
	Confidence    float64        `json:"confidence,omitempty"`
	InjectionRisk bool           `json:"injection_risk,omitempty"`
}

// approvalDetailView extends approvalView with context for supervisor evaluation.
type approvalDetailView struct {
	approvalView
	RecentTraces []trace.Entry `json:"recent_traces,omitempty"`
	ActiveGrants []grantView   `json:"active_grants,omitempty"`
}

func (h *Handler) toApprovalView(pa *approval.PendingApproval) approvalView {
	params := pa.Params
	if !h.SupervisorCfg.ShouldExposeContent() {
		params = supervisor.RedactParams(params)
	}
	v := approvalView{
		ID:            pa.ID,
		AgentID:       pa.AgentID,
		Tool:          pa.Tool,
		Params:        params,
		PolicyRule:    pa.PolicyRule,
		Status:        string(pa.Status),
		CreatedAt:     pa.CreatedAt,
		ResolvedBy:    pa.ResolvedBy,
		Reasoning:     pa.Reasoning,
		Confidence:    pa.Confidence,
		InjectionRisk: supervisor.DetectInjection(pa.Params),
	}
	if pa.Status == approval.StatusPending && h.Approvals != nil {
		v.Remaining = pa.Remaining(h.Approvals.Timeout()).Truncate(time.Second).String()
	}
	if !pa.ResolvedAt.IsZero() {
		v.ResolvedAt = &pa.ResolvedAt
	}
	return v
}

func (h *Handler) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	if h.Approvals == nil {
		writeJSON(w, 200, []any{})
		return
	}
	status := r.URL.Query().Get("status")
	toolFilter := r.URL.Query().Get("tool")

	var list []*approval.PendingApproval
	if status == "pending" {
		list = h.Approvals.ListPending()
	} else {
		list = h.Approvals.List()
	}

	views := make([]approvalView, 0, len(list))
	for _, pa := range list {
		if toolFilter != "" && !match.Glob(toolFilter, pa.Tool) {
			continue
		}
		views = append(views, h.toApprovalView(pa))
	}
	writeJSON(w, 200, views)
}

func (h *Handler) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/approvals/")
	if h.Approvals == nil {
		writeJSON(w, 404, map[string]string{"error": "approval system not configured"})
		return
	}
	pa := h.Approvals.Get(id)
	if pa == nil {
		writeJSON(w, 404, map[string]string{"error": "approval not found"})
		return
	}

	detail := approvalDetailView{
		approvalView: h.toApprovalView(pa),
	}

	// Enrich with agent's recent trace history
	if h.Traces != nil {
		detail.RecentTraces = h.Traces.Query(pa.AgentID, "", 10)
	}

	// Enrich with agent's active grants
	if h.Grants != nil {
		for _, g := range h.Grants.List() {
			if match.Glob(g.Agent, pa.AgentID) {
				detail.ActiveGrants = append(detail.ActiveGrants, grantView{
					ID:        g.ID,
					Agent:     g.Agent,
					Tools:     g.Tools,
					ExpiresAt: g.ExpiresAt.Format(time.RFC3339),
					Remaining: g.Remaining().Truncate(time.Second).String(),
					GrantedBy: g.GrantedBy,
				})
			}
		}
	}

	writeJSON(w, 200, detail)
}

type resolveRequest struct {
	ResolvedBy string  `json:"resolved_by"`
	Reasoning  string  `json:"reasoning"`
	Confidence float64 `json:"confidence"`
}

func (h *Handler) handleApproveAction(w http.ResponseWriter, r *http.Request) {
	h.handleResolveAction(w, r, approval.StatusApproved)
}

func (h *Handler) handleDenyAction(w http.ResponseWriter, r *http.Request) {
	h.handleResolveAction(w, r, approval.StatusDenied)
}

func (h *Handler) handleResolveAction(w http.ResponseWriter, r *http.Request, status approval.Status) {
	// Extract ID from /approvals/{id}/approve or /approvals/{id}/deny
	path := strings.TrimPrefix(r.URL.Path, "/approvals/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]

	if h.Approvals == nil {
		writeJSON(w, 404, map[string]string{"error": "approval system not configured"})
		return
	}

	var req resolveRequest
	json.NewDecoder(r.Body).Decode(&req) // ignore error — body is optional
	if req.ResolvedBy == "" {
		req.ResolvedBy = "http:" + r.RemoteAddr
	}

	err := h.Approvals.Resolve(id, status, approval.ResolveOpts{
		ResolvedBy: req.ResolvedBy,
		Reasoning:  req.Reasoning,
		Confidence: req.Confidence,
	})
	if err == approval.ErrNotFound {
		writeJSON(w, 404, map[string]string{"error": "approval not found"})
		return
	}
	if err == approval.ErrAlreadyResolved {
		writeJSON(w, 409, map[string]string{"error": "approval already resolved"})
		return
	}

	writeJSON(w, 200, map[string]string{"status": string(status), "id": id})
}

// --- Grant endpoints ---

type grantRequest struct {
	Agent    string `json:"agent"`
	Tools    string `json:"tools"`
	Duration string `json:"duration"` // e.g. "30m", "2h"
}

type grantView struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Tools     string `json:"tools"`
	ExpiresAt string `json:"expires_at"`
	Remaining string `json:"remaining"`
	GrantedBy string `json:"granted_by"`
}

func (h *Handler) handleListGrants(w http.ResponseWriter, _ *http.Request) {
	if h.Grants == nil {
		writeJSON(w, 200, []any{})
		return
	}
	grants := h.Grants.List()
	views := make([]grantView, len(grants))
	for i, g := range grants {
		views[i] = grantView{
			ID:        g.ID,
			Agent:     g.Agent,
			Tools:     g.Tools,
			ExpiresAt: g.ExpiresAt.Format(time.RFC3339),
			Remaining: g.Remaining().Truncate(time.Second).String(),
			GrantedBy: g.GrantedBy,
		}
	}
	writeJSON(w, 200, views)
}

func (h *Handler) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	if h.Grants == nil {
		writeJSON(w, 500, map[string]string{"error": "grant store not configured"})
		return
	}
	var req grantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Agent == "" || req.Tools == "" || req.Duration == "" {
		writeJSON(w, 400, map[string]string{"error": "agent, tools, and duration are required"})
		return
	}
	dur, err := time.ParseDuration(req.Duration)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid duration: " + err.Error()})
		return
	}
	g := h.Grants.Add(req.Agent, req.Tools, "http:"+r.RemoteAddr, dur)
	writeJSON(w, 201, grantView{
		ID:        g.ID,
		Agent:     g.Agent,
		Tools:     g.Tools,
		ExpiresAt: g.ExpiresAt.Format(time.RFC3339),
		Remaining: g.Remaining().Truncate(time.Second).String(),
		GrantedBy: g.GrantedBy,
	})
}

func (h *Handler) handleRevokeGrant(w http.ResponseWriter, r *http.Request) {
	if h.Grants == nil {
		writeJSON(w, 404, map[string]string{"error": "grant store not configured"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/grants/")
	if !h.Grants.Revoke(id) {
		writeJSON(w, 404, map[string]string{"error": "grant not found"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "revoked", "id": id})
}

// resolveTokens returns real provider token counts when available (LLM tools),
// otherwise falls back to the chars/4 estimate on params and result.
// The third return value is "real" or "estimate".
func resolveTokens(toolName string, params map[string]any, result any) (int, int, string) {
	if in, out, ok := trace.ExtractLLMTokens(toolName, result); ok {
		return in, out, "real"
	}
	return trace.EstimateTokens(params), trace.EstimateTokens(result), "estimate"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Propagate trace ID in response header if present in body
	if m, ok := v.(ToolCallResponse); ok && m.TraceID != "" {
		w.Header().Set("X-Trace-Id", m.TraceID)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
