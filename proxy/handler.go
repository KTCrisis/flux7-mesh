package proxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/auth"
	"github.com/KTCrisis/flux7-mesh/config"
	meshexec "github.com/KTCrisis/flux7-mesh/exec"
	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/internal/match"
	"github.com/KTCrisis/flux7-mesh/pin"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/ratelimit"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/supervisor"
	"github.com/KTCrisis/flux7-mesh/trace"
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
	Registry         *registry.Registry
	Policy           *policy.Engine
	PolicyEditing    *PolicyEditing           // nil: PUT /policies/... answers 501
	ApprovalSettings *ApprovalSettingsEditing // nil: PUT /approvals/settings answers 501
	Pins             *pin.Store               // nil: catalogue pinning off
	Traces           *trace.Store
	Approvals        *approval.Store
	RateLimiter      *ratelimit.Limiter
	Grants           *grant.Store
	Client           *http.Client
	MCPForwarder     MCPForwarder
	CLIRunner        *meshexec.Runner
	SupervisorCfg    config.SupervisorConfig
	JWTValidator     *auth.Validator
	AllowLegacyAgent bool         // allow plaintext "agent:" identity even when JWT is configured
	RequireAuth      bool         // reject anonymous data-plane callers with 401
	AdminToken       string       // guards the control plane; empty = loopback-only
	MCPHTTPHandler   http.Handler // MCP Streamable HTTP transport (POST/DELETE /mcp)

	// Build info (populated from main.go ldflags-injected vars).
	Version string
	// ConfigID identifies the config file this process serves (a hash of its
	// absolute path, not the path). An `mesh7 --mcp` client compares it with
	// its own before relaying to this daemon.
	ConfigID  string
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
	// --- Data plane: agents call these, never admin-gated ---
	case r.Method == "POST" && r.URL.Path == "/decide":
		h.handleDecide(w, r)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/tool/"):
		h.handleToolCall(w, r)
	case r.Method == "GET" && r.URL.Path == "/tools":
		h.handleListTools(w, r)
	case r.Method == "GET" && r.URL.Path == "/mcp-servers":
		h.handleMCPServers(w, r)
	case r.Method == "GET" && r.URL.Path == "/health":
		h.handleHealth(w, r)
	case r.Method == "GET" && r.URL.Path == "/version":
		h.handleVersion(w, r)
	case r.URL.Path == "/mcp" && h.MCPHTTPHandler != nil:
		h.MCPHTTPHandler.ServeHTTP(w, r)

	// --- Control plane: operator actions, require admin auth ---
	case r.Method == "GET" && r.URL.Path == "/tools/decisions":
		h.admin(r, w, h.handleToolDecisions)
	case r.Method == "GET" && r.URL.Path == "/tools/pins":
		h.admin(r, w, h.handlePinsPending)
	case r.Method == "POST" && r.URL.Path == "/tools/pins/accept":
		h.admin(r, w, h.handlePinsAccept)
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/policies/"):
		h.admin(r, w, h.handlePolicyEdit)
	case r.Method == "GET" && r.URL.Path == "/traces":
		h.admin(r, w, h.handleTraces)
	case r.Method == "GET" && r.URL.Path == "/traces/verify":
		h.admin(r, w, h.handleTraceVerify)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/traces/") && strings.HasSuffix(r.URL.Path, "/why"):
		h.admin(r, w, h.handleTraceWhy)
	case r.Method == "GET" && r.URL.Path == "/otel-traces":
		h.admin(r, w, h.handleOTELTraces)
	case r.Method == "GET" && r.URL.Path == "/approvals":
		h.admin(r, w, h.handleListApprovals)
	case r.Method == "GET" && r.URL.Path == "/approvals/settings":
		h.admin(r, w, h.handleGetApprovalSettings)
	case r.Method == "PUT" && r.URL.Path == "/approvals/settings":
		h.admin(r, w, h.handlePutApprovalSettings)
	case r.Method == "GET" && r.URL.Path == "/approvals/precedents":
		h.admin(r, w, h.handlePrecedents)
	case r.Method == "POST" && r.URL.Path == "/approvals/precedents/forget":
		h.admin(r, w, h.handleForgetPrecedents)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/approvals/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/approvals/"), "/"):
		h.admin(r, w, h.handleGetApproval)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/approve") && strings.HasPrefix(r.URL.Path, "/approvals/"):
		h.admin(r, w, h.handleApproveAction)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/deny") && strings.HasPrefix(r.URL.Path, "/approvals/"):
		h.admin(r, w, h.handleDenyAction)
	case r.Method == "GET" && r.URL.Path == "/grants":
		h.admin(r, w, h.handleListGrants)
	case r.Method == "POST" && r.URL.Path == "/grants":
		h.admin(r, w, h.handleCreateGrant)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/grants/"):
		h.admin(r, w, h.handleRevokeGrant)
	case r.Method == "GET" && r.URL.Path == "/sessions":
		h.admin(r, w, h.handleListSessions)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/sessions/"):
		h.admin(r, w, h.handleSessionEvents)
	case r.Method == "GET" && r.URL.Path == "/policies":
		h.admin(r, w, h.handlePolicies)
	case r.Method == "GET" && r.URL.Path == "/metrics":
		h.admin(r, w, h.handleMetrics)

	default:
		http.NotFound(w, r)
	}
}

// admin guards a control-plane handler. With AdminToken set, the caller must
// present `Authorization: Bearer <token>`. With AdminToken empty, only
// loopback callers are allowed — making the "localhost-only" posture explicit
// rather than silently exposing the control plane on every interface.
func (h *Handler) admin(r *http.Request, w http.ResponseWriter, next func(http.ResponseWriter, *http.Request)) {
	if h.adminAuthorized(r) {
		next(w, r)
		return
	}
	writeJSON(w, 401, map[string]string{"error": "control plane requires admin authorization"})
}

// requireAuthOK enforces the optional data-plane authentication gate. It returns
// true when the request may proceed. When RequireAuth is on and the caller has
// no usable identity (anonymous), it writes 401 and returns false.
func (h *Handler) requireAuthOK(w http.ResponseWriter, r *http.Request) bool {
	if !h.RequireAuth {
		return true
	}
	agentID, err := h.extractAgentID(r)
	if err != nil || agentID == "anonymous" {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return false
	}
	return true
}

func (h *Handler) adminAuthorized(r *http.Request) bool {
	if h.AdminToken != "" {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		return subtleConstEq(raw, h.AdminToken)
	}
	return isLoopback(r.RemoteAddr)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// subtleConstEq compares two strings in constant time to avoid leaking the
// admin token through response-timing differences.
func subtleConstEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (h *Handler) handleToolCall(w http.ResponseWriter, r *http.Request) {
	toolName := strings.TrimPrefix(r.URL.Path, "/tool/")
	ident, jwtErr := h.extractIdentity(r)
	if jwtErr != nil {
		writeJSON(w, 401, map[string]string{"error": jwtErr.Error()})
		return
	}
	agentID, userID := ident.AgentID, ident.UserID
	if h.RequireAuth && agentID == "anonymous" {
		writeJSON(w, 401, map[string]string{"error": "authentication required"})
		return
	}
	tc := trace.NewContext(r.Header.Get("Traceparent"), r.Header.Get("X-Trace-Id"))
	traceID := tc.TraceID
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
				TraceID:      traceID,
				SpanID:       tc.SpanID,
				ParentSpanID: tc.ParentSpanID,
				SessionID:    sessionID,
				AgentID:      agentID,
				UserID:       userID,
				Tool:         toolName,
				Params:       req.Params,
				Policy:       "rate_limited",
				PolicyRule:   preDecision.Rule,
				LatencyMs:    time.Since(start).Milliseconds(),
				Error:        err.Error(),
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

	// 4. Evaluate policy, then apply the tool's own floor (dynamic dispatchers only)
	decision := h.ApplyFloors(h.Policy.Evaluate(agentID, toolName, req.Params), tool)
	slog.Info("policy evaluated",
		"agent", agentID, "tool", toolName,
		"action", decision.Action, "rule", decision.Rule,
	)

	if decision.Action == "deny" {
		entry := trace.Entry{
			TraceID:      traceID,
			SpanID:       tc.SpanID,
			ParentSpanID: tc.ParentSpanID,
			SessionID:    sessionID,
			AgentID:      agentID,
			UserID:       userID,
			Tool:         toolName,
			Params:       req.Params,
			Policy:       "deny",
			PolicyRule:   decision.Rule,
			LatencyMs:    time.Since(start).Milliseconds(),
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
	var grantID, parentTraceID string
	if decision.Action == "human_approval" && h.Grants != nil {
		if g := h.Grants.Check(agentID, toolName); g != nil {
			slog.Info("grant override",
				"agent", agentID, "tool", toolName,
				"grant", g.ID, "remaining", g.Remaining().Truncate(time.Second),
				"origin_trace", g.Origin.TraceID)
			decision.Action = "allow"
			decision.Rule = "grant:" + g.ID
			decision.Reason = fmt.Sprintf("temporal grant %s (expires %s)", g.ID, g.ExpiresAt.Format(time.RFC3339))
			grantID = g.ID
			parentTraceID = g.Origin.TraceID
		}
	}

	// Check mem7 for past decisions (auto-approve routine patterns).
	// TryAutoResolveSafe skips auto-approve when the params carry an injection.
	if decision.Action == "human_approval" && h.Approvals != nil {
		if res := h.Approvals.TryAutoResolveSafe(agentID, toolName, req.Params); res != nil {
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
				TraceID:      traceID,
				SpanID:       tc.SpanID,
				ParentSpanID: tc.ParentSpanID,
				SessionID:    sessionID,
				AgentID:      agentID,
				UserID:       userID,
				Tool:         toolName,
				Params:       req.Params,
				Policy:       "human_approval",
				PolicyRule:   decision.Rule,
				LatencyMs:    time.Since(start).Milliseconds(),
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
		pending := h.Approvals.SubmitWithTrace(agentID, toolName, decision.Rule, req.Params, callbackURL, traceID)

		entry := trace.Entry{
			TraceID:      traceID,
			SpanID:       tc.SpanID,
			ParentSpanID: tc.ParentSpanID,
			SessionID:    sessionID,
			AgentID:      agentID,
			UserID:       userID,
			Tool:         toolName,
			Params:       req.Params,
			Policy:       "human_approval",
			PolicyRule:   decision.Rule,
			ApprovalID:   pending.ID,
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
			result, statusCode, err := h.Forward(tool, req.Params, tc)
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
	result, statusCode, err := h.Forward(tool, req.Params, tc)
	latency := time.Since(start).Milliseconds()
	inTok, outTok, tokSrc := resolveTokens(toolName, req.Params, result)

	// 5. Trace
	entry := trace.Entry{
		TraceID:               traceID,
		SpanID:                tc.SpanID,
		ParentSpanID:          tc.ParentSpanID,
		SessionID:             sessionID,
		AgentID:               agentID,
		UserID:                userID,
		Tool:                  toolName,
		Params:                req.Params,
		Policy:                "allow",
		PolicyRule:            decision.Rule,
		GrantID:               grantID,
		ParentTraceID:         parentTraceID,
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

// handleDecide evaluates policy without executing the tool.
// Returns the decision (allow/deny/human_approval) and traces it.
// dispatchFloor resolves a tool name the same way the call path does — exact
// match first, then the CLI catch-all — and returns its floor action. An
// undeclared subcommand such as terraform.destroy resolves to the dispatcher,
// so /decide answers exactly what the call path would enforce.
func (h *Handler) dispatchFloor(toolName string) string {
	return h.Floor(h.resolveTool(toolName))
}

func (h *Handler) resolveTool(toolName string) *registry.Tool {
	if tool := h.Registry.Get(toolName); tool != nil {
		return tool
	}
	return h.Registry.ResolveCLI(toolName)
}

// ApplyFloors tightens a policy decision by the tool's floors, each with its
// own rule and reason, so a refusal names what refused: the CLI dispatcher
// floor, or the catalogue pin of an upstream tool that is new or changed.
func (h *Handler) ApplyFloors(d policy.Decision, tool *registry.Tool) policy.Decision {
	if tool == nil {
		return d
	}
	d = policy.Tighten(d, tool.DispatchFloor())
	if st := h.Pins.Status(tool.Name); st.Floor() != "" {
		if t := policy.Tighten(d, st.Floor()); t.Action != d.Action {
			t.Rule = "pin:" + string(st)
			t.Reason = fmt.Sprintf("catalogue pin: %s is %s since its catalogue was accepted (policy %s returned %s)",
				tool.Name, st, d.Rule, d.Action)
			d = t
		}
	}
	return d
}

// Floor is the least permissive action a tool tolerates whatever the policy
// says: the dispatcher floor of a CLI catch-all, and the pin floor of an
// upstream tool that is new (deny) or changed (human_approval) since the
// catalogue was last accepted. The stricter of the two wins. Nil-safe.
func (h *Handler) Floor(tool *registry.Tool) string {
	if tool == nil {
		return ""
	}
	floor, pf := tool.DispatchFloor(), h.Pins.Floor(tool.Name)
	if floor == "" || pf == "" {
		return floor + pf // at most one is set
	}
	return policy.Tighten(policy.Decision{Action: floor}, pf).Action
}

func (h *Handler) handleDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Agent     string         `json:"agent"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"error": "invalid JSON body"})
		return
	}

	agentID := req.Agent
	var userID string
	if agentID == "" {
		ident, jwtErr := h.extractIdentity(r)
		agentID, userID = ident.AgentID, ident.UserID
		if jwtErr != nil {
			writeJSON(w, 401, map[string]any{"error": jwtErr.Error()})
			return
		}
	}
	if h.RequireAuth && agentID == "anonymous" {
		writeJSON(w, 401, map[string]any{"error": "authentication required"})
		return
	}
	toolName := req.Tool
	if toolName == "" {
		writeJSON(w, 400, map[string]any{"error": "tool field required"})
		return
	}

	tc := trace.NewContext(r.Header.Get("Traceparent"), r.Header.Get("X-Trace-Id"))
	traceID := tc.TraceID
	sessionID := extractSessionID(r)
	start := time.Now()

	decision := h.ApplyFloors(h.Policy.Evaluate(agentID, toolName, req.Arguments), h.resolveTool(toolName))

	var grantID, parentTraceID string
	if decision.Action == "human_approval" && h.Grants != nil {
		if g := h.Grants.Check(agentID, toolName); g != nil {
			decision.Action = "allow"
			decision.Rule = "grant:" + g.ID
			decision.Reason = fmt.Sprintf("temporal grant %s", g.ID)
			grantID = g.ID
			parentTraceID = g.Origin.TraceID
		}
	}

	if decision.Action == "human_approval" && h.Approvals != nil {
		if res := h.Approvals.TryAutoResolveSafe(agentID, toolName, req.Arguments); res != nil {
			decision.Action = "allow"
			decision.Rule = "supervisor:mem7"
			decision.Reason = res.Reasoning
		}
	}

	entry := trace.Entry{
		TraceID:       traceID,
		SpanID:        tc.SpanID,
		ParentSpanID:  tc.ParentSpanID,
		SessionID:     sessionID,
		AgentID:       agentID,
		UserID:        userID,
		Tool:          toolName,
		Params:        req.Arguments,
		Policy:        decision.Action,
		PolicyRule:    decision.Rule,
		GrantID:       grantID,
		ParentTraceID: parentTraceID,
		LatencyMs:     time.Since(start).Milliseconds(),
	}
	h.Traces.Record(entry)

	status := 200
	if decision.Action == "deny" {
		status = 403
	}

	writeJSON(w, status, map[string]any{
		"action":   decision.Action,
		"rule":     decision.Rule,
		"reason":   decision.Reason,
		"agent":    agentID,
		"tool":     toolName,
		"trace_id": traceID,
	})
}

// Forward sends the request to the appropriate backend (HTTP, MCP, or CLI).
// The trace context is propagated to HTTP backends via Traceparent (the mesh
// span as parent) and X-Trace-Id headers.
func (h *Handler) Forward(tool *registry.Tool, params map[string]any, tc trace.Context) (any, int, error) {
	switch tool.Source {
	case "mcp":
		return h.forwardMCP(tool, params)
	case "cli":
		return h.forwardCLI(tool, params)
	default:
		return h.forwardHTTP(tool, params, tc)
	}
}

// forwardHTTP sends the request to a REST backend.
func (h *Handler) forwardHTTP(tool *registry.Tool, params map[string]any, tc trace.Context) (any, int, error) {
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
	if tc.TraceID != "" {
		req.Header.Set("X-Trace-Id", tc.TraceID)
		req.Header.Set("Traceparent", tc.Traceparent())
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

	in := meshexec.ExtractCommand(params, meta)

	ctx := context.Background()
	result, err := h.CLIRunner.Run(ctx, meta, in)
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

func (h *Handler) handleListTools(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuthOK(w, r) {
		return
	}
	tools := h.Registry.All()
	out := make([]toolView, len(tools))
	for i, t := range tools {
		out[i] = toolView{Tool: t, Classification: registry.Classify(t)}
	}
	writeJSON(w, 200, out)
}

// toolView is a registry tool plus the mesh's reading of it. Embedding the
// *registry.Tool promotes its fields into the JSON object, so existing
// clients see the same shape with one field more.
type toolView struct {
	*registry.Tool
	Classification registry.Classification `json:"classification"`
}

// toolDecision is one row of GET /tools/decisions.
type toolDecision struct {
	Name           string                  `json:"name"`
	Source         string                  `json:"source"`
	MCPServer      string                  `json:"mcp_server,omitempty"`
	Classification registry.Classification `json:"classification"`
	Pin            pin.Status              `json:"pin,omitempty"` // "", pinned, new, changed
	policy.StaticDecision
}

// handleToolDecisions reports, for one agent, what the policy decides for
// every tool in the catalogue, before any call. It is control plane because it
// discloses the policy. Grants are not reflected: they belong to a session,
// not to the catalogue. The dispatcher floor is, since it holds for every call.
func (h *Handler) handleToolDecisions(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeJSON(w, 400, map[string]string{"error": "agent query parameter required"})
		return
	}
	tools := h.Registry.All()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	out := make([]toolDecision, 0, len(tools))
	for _, t := range tools {
		sd := h.Policy.Explain(agent, t.Name)
		floor := h.Floor(t)
		// Same floors, same rule names as a real call: a pin that overrides
		// the policy shows as pin:new or pin:changed, not as the policy.
		fd := h.ApplyFloors(policy.Decision{Action: sd.Action, Rule: sd.Rule}, t)
		sd.Action, sd.Rule = fd.Action, fd.Rule
		for i := range sd.Conditional {
			sd.Conditional[i].Action = policy.Tighten(policy.Decision{Action: sd.Conditional[i].Action}, floor).Action
		}
		out = append(out, toolDecision{
			Name:           t.Name,
			Source:         t.Source,
			MCPServer:      t.MCPServer,
			Classification: registry.Classify(t),
			Pin:            h.Pins.Status(t.Name),
			StaticDecision: sd,
		})
	}
	writeJSON(w, 200, out)
}

// handleTraceVerify walks the hash chain of the trace file and reports the
// first break, if any. A broken chain is still a 200: the report is the
// answer, and the caller decides what a break means.
func (h *Handler) handleTraceVerify(w http.ResponseWriter, r *http.Request) {
	st, err := h.Traces.VerifyChain()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, st)
}

func (h *Handler) handleTraces(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	tool := r.URL.Query().Get("tool")
	writeJSON(w, 200, h.Traces.Query(agent, tool, 100))
}

// handleTraceWhy answers "why was this call allowed?" by returning the chain of
// authority behind a trace, oldest first. Each hop is a call that motivated the
// grant which authorized the next one.
//
// A chain of one is a complete answer, not a failure: the call needed no grant,
// or the grant that covered it was issued without an origin.
// Query param: depth (default 10, capped at 50).
func (h *Handler) handleTraceWhy(w http.ResponseWriter, r *http.Request) {
	traceID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/traces/"), "/why")
	if traceID == "" || strings.Contains(traceID, "/") {
		writeJSON(w, 400, map[string]string{"error": "trace id required"})
		return
	}

	depth := 10
	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			depth = min(n, 50)
		}
	}

	chain := h.Traces.Chain(traceID, depth)
	if len(chain) == 0 {
		writeJSON(w, 404, map[string]string{"error": "trace not found"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"trace_id":     traceID,
		"chain_length": len(chain),
		"chain":        chain,
	})
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
	writeJSON(w, 200, trace.EntriesToOTLP(entries, "flux7-mesh"))
}

func (h *Handler) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuthOK(w, r) {
		return
	}
	if h.MCPForwarder == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, h.MCPForwarder.ServerStatuses())
}

func (h *Handler) handlePolicies(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, h.Policy.Policies())
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status":  "ok",
		"tools":   len(h.Registry.All()),
		"traces":  h.Traces.Stats(),
		"version": h.Version,
		"config":  h.ConfigID,
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

// extractSessionID reads an optional session ID from the X-Session-Id header.
// Returns empty string if not provided (no auto-generation).
func extractSessionID(r *http.Request) string {
	return r.Header.Get("X-Session-Id")
}

// extractIdentity reads the agent and, when the credential carries one, the
// user from the Authorization header.
// Legacy format "Bearer agent:<id>" bypasses JWT validation.
// If JWTValidator is configured, Bearer tokens are validated as JWT.
func (h *Handler) extractIdentity(r *http.Request) (auth.Identity, error) {
	return auth.ResolveIdentity(r.Header.Get("Authorization"), h.JWTValidator, h.AllowLegacyAgent)
}

// extractAgentID is extractIdentity for callers that only need the agent.
func (h *Handler) extractAgentID(r *http.Request) (string, error) {
	id, err := h.extractIdentity(r)
	return id.AgentID, err
}

// --- Approval endpoints ---

type approvalView struct {
	ID      string         `json:"id"`
	AgentID string         `json:"agent_id"`
	Tool    string         `json:"tool"`
	Params  map[string]any `json:"params"`
	// TraceID is the call awaiting this decision. An operator extending the
	// decision into a grant needs it, otherwise the grant has no origin to
	// record and the chain of authority starts empty.
	TraceID       string     `json:"trace_id,omitempty"`
	PolicyRule    string     `json:"policy_rule"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	Remaining     string     `json:"remaining,omitempty"`
	ResolvedBy    string     `json:"resolved_by,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	Reasoning     string     `json:"reasoning,omitempty"`
	Confidence    float64    `json:"confidence,omitempty"`
	InjectionRisk bool       `json:"injection_risk,omitempty"`
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
		TraceID:       pa.TraceID,
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

// handlePrecedents lists what mem7 remembers per tool and agent: human
// approvals (the only ones that count), other approvals, refusals, and
// whether the next call would pass on precedents alone.
func (h *Handler) handlePrecedents(w http.ResponseWriter, r *http.Request) {
	list, err := h.Approvals.Precedents()
	if err != nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "reason": err.Error(), "precedents": []any{}})
		return
	}
	writeJSON(w, 200, map[string]any{
		"enabled":       true,
		"min_approvals": h.Approvals.MemoryReader.MinApprovals(),
		"precedents":    list,
	})
}

// handleForgetPrecedents drops every decision of one tool and agent from
// mem7: that couple starts again from no precedent.
func (h *Handler) handleForgetPrecedents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tool  string `json:"tool"`
		Agent string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "expected JSON {\"tool\", \"agent\"}"})
		return
	}
	if h.Approvals == nil || h.Approvals.MemoryReader == nil {
		writeJSON(w, 409, map[string]string{"error": "auto-approval from precedents is off"})
		return
	}
	result, err := h.Approvals.MemoryReader.ForgetPrecedents(body.Tool, body.Agent)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	slog.Info("precedents forgotten", "tool", body.Tool, "agent", body.Agent, "mem7", result)
	writeJSON(w, 200, map[string]string{"tool": body.Tool, "agent": body.Agent, "result": result})
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
				detail.ActiveGrants = append(detail.ActiveGrants, toGrantView(g))
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

	// Optional origin: the approval or the call this grant answers. Both are
	// free-form on purpose — the mesh does not require an operator to justify a
	// grant, it only records the justification when one is offered.
	ApprovalID string `json:"approval_id,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
}

type grantView struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Tools      string `json:"tools"`
	ExpiresAt  string `json:"expires_at"`
	Remaining  string `json:"remaining"`
	GrantedBy  string `json:"granted_by"`
	ApprovalID string `json:"approval_id,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
}

func toGrantView(g *grant.Grant) grantView {
	return grantView{
		ID:         g.ID,
		Agent:      g.Agent,
		Tools:      g.Tools,
		ExpiresAt:  g.ExpiresAt.Format(time.RFC3339),
		Remaining:  g.Remaining().Truncate(time.Second).String(),
		GrantedBy:  g.GrantedBy,
		ApprovalID: g.Origin.ApprovalID,
		TraceID:    g.Origin.TraceID,
	}
}

func (h *Handler) handleListGrants(w http.ResponseWriter, _ *http.Request) {
	if h.Grants == nil {
		writeJSON(w, 200, []any{})
		return
	}
	grants := h.Grants.List()
	views := make([]grantView, len(grants))
	for i, g := range grants {
		views[i] = toGrantView(g)
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
	g := h.Grants.AddWithOrigin(req.Agent, req.Tools, "http:"+r.RemoteAddr, dur, grant.Origin{
		ApprovalID: req.ApprovalID,
		TraceID:    req.TraceID,
	})
	writeJSON(w, 201, toGrantView(g))
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
