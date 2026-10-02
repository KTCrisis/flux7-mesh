package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/halt"
	"github.com/KTCrisis/flux7-mesh/internal/match"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// HaltFor returns the emergency stop a call by agentID in sessionID falls
// under, or nil. Every decision path (HTTP, /decide, MCP) asks it first.
func (h *Handler) HaltFor(agentID, sessionID string) *halt.Halt {
	if h.Halts == nil {
		return nil
	}
	return h.Halts.Match(agentID, sessionID)
}

// RecordHalted traces a call refused by a halt. The policy is "halted", not
// "deny": the operator stopped the agent, no rule refused the call.
func (h *Handler) RecordHalted(hl *halt.Halt, tc trace.Context, sessionID, agentID, userID, tool string, params map[string]any, start time.Time) {
	h.Traces.Record(trace.Entry{
		TraceID: tc.TraceID, SpanID: tc.SpanID, ParentSpanID: tc.ParentSpanID,
		SessionID: sessionID, AgentID: agentID, UserID: userID,
		Tool: tool, Params: params,
		Policy: "halted", PolicyRule: "halt:" + hl.ID,
		LatencyMs: time.Since(start).Milliseconds(),
		Error:     hl.Message(),
	})
}

type haltRequest struct {
	Scope  string `json:"scope"`
	Target string `json:"target"`
	Reason string `json:"reason"`
	By     string `json:"by"`
}

// handleListHalts serves GET /halts: the stops in force.
func (h *Handler) handleListHalts(w http.ResponseWriter, _ *http.Request) {
	if h.Halts == nil {
		writeJSON(w, 200, []any{})
		return
	}
	active := h.Halts.Active()
	if active == nil {
		active = []*halt.Halt{}
	}
	writeJSON(w, 200, active)
}

// handleCreateHalt serves POST /halts. Besides blocking every new call in its
// scope, the stop denies the approvals waiting in it and revokes the grants
// of the halted agent (all grants for a global stop), kept for the resume.
// The whole action is recorded in the trace chain.
func (h *Handler) handleCreateHalt(w http.ResponseWriter, r *http.Request) {
	if h.Halts == nil {
		writeJSON(w, 501, map[string]string{"error": "emergency stop not configured"})
		return
	}
	var req haltRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return
	}
	by := operator(req.By, r)
	hl, created, err := h.Halts.Start(halt.Scope(req.Scope), req.Target, req.Reason, by)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !created {
		writeJSON(w, 200, map[string]any{"halt": hl, "already_active": true})
		return
	}

	revoked := h.revokeGrantsFor(hl)
	if len(revoked) > 0 {
		h.Halts.RecordRevoked(hl.ID, revoked)
	}
	denied := h.denyPendingFor(hl)

	h.Traces.Record(trace.Entry{
		TraceID: trace.NewID(), AgentID: by, Tool: "mesh.halt",
		Params: map[string]any{
			"halt_id": hl.ID, "scope": string(hl.Scope), "target": hl.Target, "reason": hl.Reason,
			"revoked_grants": grantIDs(revoked), "denied_approvals": denied,
		},
		Policy: "allow", PolicyRule: "control-plane", StatusCode: 201, Timestamp: time.Now(),
	})
	writeJSON(w, 201, map[string]any{
		"halt": hl, "revoked_grants": len(revoked), "denied_approvals": len(denied),
	})
}

// handleResumeHalt serves POST /halts/{id}/resume: the stop is lifted and the
// grants it revoked come back, unless they expired in the meantime.
func (h *Handler) handleResumeHalt(w http.ResponseWriter, r *http.Request) {
	if h.Halts == nil {
		writeJSON(w, 501, map[string]string{"error": "emergency stop not configured"})
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/halts/"), "/resume")
	var req haltRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // the body is optional: only "by"
	by := operator(req.By, r)

	hl, err := h.Halts.Resume(id, by)
	if errors.Is(err, halt.ErrNotFound) {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}

	var restored []string
	if h.Grants != nil {
		for _, g := range hl.RevokedGrants {
			if h.Grants.Restore(g) {
				restored = append(restored, g.ID)
			}
		}
	}
	h.Traces.Record(trace.Entry{
		TraceID: trace.NewID(), AgentID: by, Tool: "mesh.resume",
		Params: map[string]any{
			"halt_id": hl.ID, "scope": string(hl.Scope), "target": hl.Target,
			"restored_grants": restored,
		},
		Policy: "allow", PolicyRule: "control-plane", StatusCode: 200, Timestamp: time.Now(),
	})
	writeJSON(w, 200, map[string]any{"halt": hl, "restored_grants": len(restored)})
}

// revokeGrantsFor takes away the grants that would let the halted scope act
// once the stop is lifted carelessly: every grant for a global stop, the
// agent's own grants for an agent stop. Grants are not tied to a session, so a
// session stop revokes none: the agent keeps its grants in its other sessions.
func (h *Handler) revokeGrantsFor(hl *halt.Halt) []grant.Grant {
	if h.Grants == nil {
		return nil
	}
	switch hl.Scope {
	case halt.ScopeAll:
		return h.Grants.RevokeWhere(func(*grant.Grant) bool { return true })
	case halt.ScopeAgent:
		return h.Grants.RevokeWhere(func(g *grant.Grant) bool {
			return g.Agent == hl.Target || match.Glob(hl.Target, g.Agent)
		})
	}
	return nil
}

// denyPendingFor denies the approvals waiting in the halt's scope: the agents
// blocked on them get their answer now instead of at the timeout.
func (h *Handler) denyPendingFor(hl *halt.Halt) []string {
	if h.Approvals == nil {
		return nil
	}
	var inSession map[string]bool
	if hl.Scope == halt.ScopeSession {
		inSession = map[string]bool{}
		for _, e := range h.Traces.QueryBySession(hl.Target, 1_000_000) {
			inSession[e.TraceID] = true
		}
	}
	var denied []string
	for _, pa := range h.Approvals.ListPending() {
		covered := false
		switch hl.Scope {
		case halt.ScopeSession:
			covered = pa.TraceID != "" && inSession[pa.TraceID]
		default:
			covered = hl.Covers(pa.AgentID, "")
		}
		if covered && h.Approvals.Deny(pa.ID, "halt:"+hl.ID) == nil {
			denied = append(denied, pa.ID)
		}
	}
	return denied
}

// operator names who acted: the "by" field when given, else the caller's
// address, the same convention as the grants.
func operator(by string, r *http.Request) string {
	if by = strings.TrimSpace(by); by != "" {
		return by
	}
	return "http:" + r.RemoteAddr
}

func grantIDs(gs []grant.Grant) []string {
	ids := make([]string, len(gs))
	for i, g := range gs {
		ids[i] = g.ID
	}
	return ids
}
