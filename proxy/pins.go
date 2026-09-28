package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/KTCrisis/flux7-mesh/trace"
)

// handlePinsPending serves GET /tools/pins: upstream tools that are new or
// changed since the catalogue was last accepted, with both descriptions.
func (h *Handler) handlePinsPending(w http.ResponseWriter, r *http.Request) {
	if h.Pins == nil {
		writeJSON(w, 501, map[string]string{"error": "pin_tools is off"})
		return
	}
	writeJSON(w, 200, h.Pins.Pending())
}

// handlePinsAccept serves POST /tools/pins/accept with {"tools": [...]} or
// {"server": "name"} (every pending tool of that server) and an optional
// "by". Each accepted tool is recorded in the trace chain.
func (h *Handler) handlePinsAccept(w http.ResponseWriter, r *http.Request) {
	if h.Pins == nil {
		writeJSON(w, 501, map[string]string{"error": "pin_tools is off"})
		return
	}
	var req struct {
		Tools  []string `json:"tools"`
		Server string   `json:"server"`
		By     string   `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return
	}
	tools := req.Tools
	if req.Server != "" {
		for _, v := range h.Pins.Pending() {
			if v.Server == req.Server {
				tools = append(tools, v.Tool)
			}
		}
	}
	if len(tools) == 0 {
		writeJSON(w, 400, map[string]string{"error": "nothing to accept: give tools or a server with pending tools"})
		return
	}
	before, err := h.Pins.Accept(tools)
	by := req.By
	if by == "" {
		by = "admin"
	}
	for _, v := range before {
		h.Traces.Record(trace.Entry{
			TraceID: trace.NewID(), AgentID: by, Tool: "mesh.pin_accept",
			Params: map[string]any{
				"tool": v.Tool, "server": v.Server, "was": string(v.Status),
				"pinned_description": v.Pinned, "accepted_description": v.Current,
				"fingerprint": v.Fingerprint,
			},
			Policy: "allow", PolicyRule: "control-plane", StatusCode: 200, Timestamp: time.Now(),
		})
	}
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": err.Error(), "accepted": len(before)})
		return
	}
	writeJSON(w, 200, map[string]any{"accepted": len(before), "pending": h.Pins.Pending()})
}
