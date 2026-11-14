package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

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
	// RequireAuth rejects callers with no credential (agent resolves to
	// "anonymous"). Mirrors proxy.Handler.RequireAuth so a single config flag
	// governs both data planes.
	RequireAuth      bool
	SupervisorMode   bool
	SupervisorAgents []string
	ApprovalChannel  string
	HideDenied       bool          // see Server.HideDenied
	ApprovalWait     time.Duration // see Server.ApprovalWait

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

	// Headers that disagree with the body are refused before anything else
	// (era_server.go): a gateway may have routed on them.
	if why := headerMismatch(r.Header, req); why != "" {
		writeJSONRPCErrorData(w, req.ID, errHeaderMismatch, "Bad Request: the request headers and body disagree: "+why,
			map[string]any{"mismatch": why}, http.StatusBadRequest)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")
	ident, jwtErr := h.extractIdentity(r)
	if jwtErr != nil {
		writeJSONRPCError(w, req.ID, -32600, jwtErr.Error(), http.StatusUnauthorized)
		return
	}
	agentID := ident.AgentID

	// A modern request stands alone: identity from this request's
	// credential, no session to open, find or hand back.
	if requestVersion(req.Params) != "" {
		if h.RequireAuth && agentID == "anonymous" {
			writeJSONRPCError(w, req.ID, -32600, "authentication required", http.StatusUnauthorized)
			return
		}
		srv := h.newServer(ident, sessionLabel(r.Header))
		srv.ClientSession = srv.SessionID
		resp := srv.HandleRequestWith(req, requestTraceContext(r, req))
		if resp.JSONRPC == "" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
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

	// RequireAuth applies to this transport exactly as it does to the REST
	// data plane: no credential means no session, and no call on one.
	if h.RequireAuth && agentID == "anonymous" {
		writeJSONRPCError(w, req.ID, -32600, "authentication required", http.StatusUnauthorized)
		return
	}

	if !isInit && sessionID != "" {
		h.mu.Lock()
		existing, exists := h.sessions[sessionID]
		h.mu.Unlock()
		if !exists {
			writeJSONRPCError(w, req.ID, -32600, "Unknown session", http.StatusNotFound)
			return
		}
		// The session ID is not a credential. Identity is resolved on every
		// request and must still match the agent that opened the session,
		// otherwise whoever holds the ID inherits that agent's policy — the
		// caller's own identity would never be evaluated.
		if existing.AgentID != agentID || existing.UserID != ident.UserID {
			writeJSONRPCError(w, req.ID, -32600, "session belongs to another agent", http.StatusForbidden)
			return
		}
	}

	srv := h.getOrCreateSession(sessionID, ident, sessionLabel(r.Header))
	resp := srv.HandleRequestWith(req, requestTraceContext(r, req))

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

	ident, jwtErr := h.extractIdentity(r)
	if jwtErr != nil {
		http.Error(w, jwtErr.Error(), http.StatusUnauthorized)
		return
	}
	agentID := ident.AgentID
	if h.RequireAuth && agentID == "anonymous" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	h.mu.Lock()
	existing, exists := h.sessions[sessionID]
	// Closing a session is a write: only its owner may do it, otherwise any
	// holder of the ID could cut another agent's session.
	if exists && (existing.AgentID != agentID || existing.UserID != ident.UserID) {
		h.mu.Unlock()
		http.Error(w, "session belongs to another agent", http.StatusForbidden)
		return
	}
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

func (h *HTTPHandler) getOrCreateSession(sessionID string, ident auth.Identity, label string) *Server {
	agentID := ident.AgentID
	h.mu.Lock()
	defer h.mu.Unlock()

	if sessionID != "" {
		if srv, ok := h.sessions[sessionID]; ok {
			return srv
		}
	}

	// New sessions always get a server-minted, unguessable ID — a
	// client-supplied ID is never adopted (callers pass "" on initialize).
	srv := h.newServer(ident, trace.NewID())
	// the client's label is kept beside the minted ID, never in its place:
	// set once, at creation, so no request races a later write
	srv.ClientSession = label
	h.sessions[srv.SessionID] = srv

	slog.Info("MCP HTTP session created", "session_id", srv.SessionID, "agent", agentID)
	return srv
}

// newServer builds the Server that handles a caller's requests: one per
// legacy session, or one per modern request, whose session is only the
// client's X-Session-Id label, if any (see era_server.go).
func (h *HTTPHandler) newServer(ident auth.Identity, sessionID string) *Server {
	return &Server{
		Registry:         h.Registry,
		Policy:           h.Policy,
		Traces:           h.Traces,
		Approvals:        h.Approvals,
		Handler:          h.Handler,
		MCPManager:       h.MCPManager,
		AgentID:          ident.AgentID,
		UserID:           ident.UserID,
		SessionID:        sessionID,
		SupervisorMode:   h.SupervisorMode,
		SupervisorAgents: h.SupervisorAgents,
		ApprovalChannel:  h.ApprovalChannel,
		ApprovalWait:     h.ApprovalWait,
		HideDenied:       h.HideDenied,
	}
}

func (h *HTTPHandler) extractIdentity(r *http.Request) (auth.Identity, error) {
	return auth.ResolveIdentity(r.Header.Get("Authorization"), h.JWTValidator, h.AllowLegacyAgent)
}

func writeJSONRPCError(w http.ResponseWriter, id any, code int, msg string, httpStatus int) {
	writeJSONRPCErrorData(w, id, code, msg, nil, httpStatus)
}

func writeJSONRPCErrorData(w http.ResponseWriter, id any, code int, msg string, data any, httpStatus int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg, Data: data},
	})
}
