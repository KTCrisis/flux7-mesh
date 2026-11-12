package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/trace"
)

// catalogGeneration is the count of tool-catalogue changes caused by
// upstreams, 0 when the mesh has none to manage. It rides on /health so a
// client can learn cheaply that the tool list is worth reading again.
func (h *Handler) catalogGeneration() uint64 {
	if h.Upstreams == nil {
		return 0
	}
	return h.Upstreams.Generation()
}

func (h *Handler) recordUpstreamEdit(r *http.Request, tool string, params map[string]any, status int, errMsg string) {
	by := strings.TrimSpace(r.Header.Get("X-Edited-By"))
	if by == "" {
		by = "admin"
	}
	h.Traces.Record(trace.Entry{
		TraceID:    trace.NewID(),
		AgentID:    by,
		Tool:       tool,
		Params:     params,
		Policy:     "allow",
		PolicyRule: "control-plane",
		StatusCode: status,
		Error:      errMsg,
		Timestamp:  time.Now().UTC(),
	})
}

// handleUpstreamsReload serves POST /mcp-servers/reload: read `mcp_servers`
// again from the config file. Servers no longer declared are stopped;
// declarations that are new or changed are held, and nothing is started for
// them until POST /mcp-servers/{name}/accept. A config that does not load
// changes nothing and answers 422.
func (h *Handler) handleUpstreamsReload(w http.ResponseWriter, r *http.Request) {
	if h.Upstreams == nil {
		writeJSON(w, 501, map[string]string{"error": "this mesh does not manage upstreams"})
		return
	}
	diff, err := h.Upstreams.Reload()
	if err != nil {
		h.recordUpstreamEdit(r, "mesh.upstream_reload", map[string]any{}, 422, err.Error())
		writeJSON(w, 422, map[string]string{"error": "reload rejected, upstreams unchanged: " + err.Error()})
		return
	}
	h.recordUpstreamEdit(r, "mesh.upstream_reload", map[string]any{"diff": diff}, 200, "")
	writeJSON(w, 200, map[string]any{"diff": diff, "catalog": h.Upstreams.Generation()})
}

// upstreamName reads {name} from /mcp-servers/{name}/{verb}.
func upstreamName(r *http.Request, verb string) (string, bool) {
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(), "/mcp-servers/"), "/"+verb)
	name, err := url.PathUnescape(rest)
	if err != nil || name == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return name, true
}

// handleUpstreamsHeld serves GET /mcp-servers/held: the declarations that
// appeared or changed in the config file while the mesh ran, and what each
// would run. Admin only: a command line is not for agents to read.
func (h *Handler) handleUpstreamsHeld(w http.ResponseWriter, _ *http.Request) {
	if h.Upstreams == nil {
		writeJSON(w, 200, []any{})
		return
	}
	writeJSON(w, 200, h.Upstreams.Held())
}

// handleUpstreamAccept serves POST /mcp-servers/{name}/accept: start a held
// declaration. This is the operator's act that a restart used to be.
func (h *Handler) handleUpstreamAccept(w http.ResponseWriter, r *http.Request) {
	if h.Upstreams == nil {
		writeJSON(w, 501, map[string]string{"error": "this mesh does not manage upstreams"})
		return
	}
	name, ok := upstreamName(r, "accept")
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "want /mcp-servers/{name}/accept"})
		return
	}
	if err := h.Upstreams.Accept(name); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	h.recordUpstreamEdit(r, "mesh.upstream_accept", map[string]any{"server": name}, 200, "")
	writeJSON(w, 200, map[string]any{"server": name, "catalog": h.Upstreams.Generation()})
}

// handleUpstreamRestart serves POST /mcp-servers/{name}/restart.
func (h *Handler) handleUpstreamRestart(w http.ResponseWriter, r *http.Request) {
	if h.Upstreams == nil {
		writeJSON(w, 501, map[string]string{"error": "this mesh does not manage upstreams"})
		return
	}
	name, ok := upstreamName(r, "restart")
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "want /mcp-servers/{name}/restart"})
		return
	}
	if err := h.Upstreams.Restart(name); err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	h.recordUpstreamEdit(r, "mesh.upstream_restart", map[string]any{"server": name}, 200, "")
	writeJSON(w, 200, map[string]any{"server": name, "catalog": h.Upstreams.Generation()})
}
