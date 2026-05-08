package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/KTCrisis/flux7-mesh/approval"
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
	SupervisorMode   bool
	SupervisorAgents []string

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
	agentID := extractAgentID(r)

	isInit := req.Method == "initialize"

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

	srv := &Server{
		Registry:         h.Registry,
		Policy:           h.Policy,
		Traces:           h.Traces,
		Approvals:        h.Approvals,
		Handler:          h.Handler,
		MCPManager:       h.MCPManager,
		AgentID:          agentID,
		SessionID:        sessionID,
		SupervisorMode:   h.SupervisorMode,
		SupervisorAgents: h.SupervisorAgents,
	}

	// SessionID is set during HandleRequest("initialize") if empty.
	// We register it after the first initialize call completes. For now,
	// if a session ID was provided, register it immediately.
	if sessionID != "" {
		h.sessions[sessionID] = srv
	} else {
		// Defer registration: the caller will call HandleRequest("initialize")
		// which sets srv.SessionID. We use an AfterInit hook via a wrapper.
		// Simpler approach: generate the ID now so the session is addressable.
		srv.SessionID = trace.NewID()
		h.sessions[srv.SessionID] = srv
	}

	slog.Info("MCP HTTP session created", "session_id", srv.SessionID, "agent", agentID)
	return srv
}

// extractAgentID reads the agent identity from the Authorization header.
// Supports "Bearer agent:<id>" (agent-mesh convention) and plain "Bearer <token>".
func extractAgentID(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "anonymous"
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if strings.HasPrefix(token, "agent:") {
		return strings.TrimPrefix(token, "agent:")
	}
	return "authenticated"
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
