package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/auth"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// HTTPHandler serves the MCP Streamable HTTP transport (POST/DELETE /mcp).
// Each Mcp-Session-Id maps to an isolated Server with its own agent context.
type HTTPHandler struct {
	Registry         *registry.Registry
	Policy           *policy.Engine
	Traces           *trace.Store
	Approvals        *approval.Store
	Handler          *proxy.Handler
	MCPManager       *Manager
	JWTValidator     *auth.Validator
	AllowLegacyAgent bool
	SupervisorMode   bool
	SupervisorAgents []string
	ApprovalChannel  string

	mu       sync.Mutex
	sessions map[string]*Server
}

// NewHTTPHandler creates a streamable-HTTP MCP handler.
func NewHTTPHandler(
	reg *registry.Registry,
	pol *policy.Engine,
	traces *trace.Store,
	approvals *approval.Store,
	handler *proxy.Handler,
	mcpMgr *Manager,
	supervisorMode bool,
	supervisorAgents []string,
	approvalChannel string,
) *HTTPHandler {
	return &HTTPHandler{
		Registry:         reg,
		Policy:           pol,
		Traces:           traces,
		Approvals:        approvals,
		Handler:          handler,
		MCPManager:       mcpMgr,
		SupervisorMode:   supervisorMode,
		SupervisorAgents: supervisorAgents,
		ApprovalChannel:  approvalChannel,
		sessions:         make(map[string]*Server),
	}
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		h.handlePost(w, r)
	case "DELETE":
		h.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *HTTPHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, nil, -32700, "Parse error", http.StatusBadRequest)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")
	agentID, jwtErr := h.extractAgentID(r)
	if jwtErr != nil {
		writeJSONRPCError(w, req.ID, -32600, jwtErr.Error(), http.StatusUnauthorized)
		return
	}

	isInit := req.Method == "initialize"

	// Session fixation defence: never adopt a client-supplied session ID on
	// initialize. The ID is always minted server-side, so an attacker cannot
	// pre-register an ID a victim might later reuse.
	if isInit {
		sessionID = ""
	}

	if !isInit && sessionID == "" {
		writeJSONRPCError(w, req.ID, -32600, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	if !isInit && sessionID != "" {
		h.mu.Lock()
		_, exists := h.sessions[sessionID]
		h.mu.Unlock()
		if !exists {
			writeJSONRPCError(w, req.ID, -32600, "Unknown session", http.StatusNotFound)
			return
		}
	}

	srv := h.getOrCreateSession(sessionID, agentID)
	resp := srv.HandleRequest(req)

	// Notifications — no response body.
	if resp.JSONRPC == "" {
		w.Header().Set("Mcp-Session-Id", srv.SessionID)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", srv.SessionID)
	json.NewEncoder(w).Encode(resp)
}

func (h *HTTPHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	_, exists := h.sessions[sessionID]
	if exists {
		delete(h.sessions, sessionID)
	}
	h.mu.Unlock()

	if !exists {
		http.NotFound(w, r)
		return
	}

	slog.Info("MCP HTTP session deleted", "session_id", sessionID)
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) getOrCreateSession(sessionID, agentID string) *Server {
	h.mu.Lock()
	defer h.mu.Unlock()

	if sessionID != "" {
		if srv, ok := h.sessions[sessionID]; ok {
			return srv
		}
	}

	// New sessions always get a server-minted, unguessable ID — a
	// client-supplied ID is never adopted (callers pass "" on initialize).
	srv := &Server{
		Registry:         h.Registry,
		Policy:           h.Policy,
		Traces:           h.Traces,
		Approvals:        h.Approvals,
		Handler:          h.Handler,
		MCPManager:       h.MCPManager,
		AgentID:          agentID,
		SessionID:        trace.NewID(),
		SupervisorMode:   h.SupervisorMode,
		SupervisorAgents: h.SupervisorAgents,
		ApprovalChannel:  h.ApprovalChannel,
	}
	h.sessions[srv.SessionID] = srv

	slog.Info("MCP HTTP session created", "session_id", srv.SessionID, "agent", agentID)
	return srv
}

func (h *HTTPHandler) extractAgentID(r *http.Request) (string, error) {
	return auth.ResolveAgentID(r.Header.Get("Authorization"), h.JWTValidator, h.AllowLegacyAgent)
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, msg string, httpStatus int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}
