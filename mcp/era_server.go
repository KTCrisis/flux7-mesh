package mcp

import (
	"net/http"

	"github.com/KTCrisis/flux7-mesh/internal/callmeta"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// Grouping the calls of one conversation. The 2026-07-28 revision has no
// session, so the protocol no longer hands mesh7 an ID per conversation. Two
// sources replace it, both from the client:
//
//   - the W3C trace context, which agent frameworks propagate per run: the
//     Traceparent header, or the traceparent the revision standardizes in
//     params._meta (SEP-414). Calls sharing it share a trace_id
//     (GET /traces?trace=<id>).
//   - X-Session-Id, the label mesh7's REST API already reads: it fills the
//     session of a modern request, so traces group by it and a session-scoped
//     halt reaches it. It is declared by the client, not proven: a client that
//     changes it escapes a session halt, never an agent halt.

// metaTraceparent returns the trace context a request carries in
// params._meta, or "".
func metaTraceparent(params map[string]any) string {
	meta, _ := params["_meta"].(map[string]any)
	tp, _ := meta[callmeta.Traceparent].(string)
	return tp
}

// requestTraceContext is the trace a request joins: the Traceparent header
// first, then the traceparent in _meta, then X-Trace-Id; a fresh trace when
// none is valid.
func requestTraceContext(r *http.Request, req rpcRequest) trace.Context {
	tp := r.Header.Get("Traceparent")
	if _, _, ok := trace.ParseTraceparent(tp); !ok {
		tp = metaTraceparent(req.Params)
	}
	return trace.NewContext(tp, r.Header.Get("X-Trace-Id"))
}

// sessionLabel returns the X-Session-Id of a request when it is a sane label
// (1 to 128 visible ASCII characters), else "".
func sessionLabel(h http.Header) string {
	v := h.Get("X-Session-Id")
	if len(v) == 0 || len(v) > 128 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] > '~' {
			return ""
		}
	}
	return v
}

// The export side of the protocol eras (see era.go for the upstream side).
//
// mesh7 serves both eras from the same Server. A modern request declares its
// protocol version in params._meta and stands alone: no initialize, no
// session. A legacy request is anything else, served exactly as before.
// initialize still answers 2024-11-05, so what existing clients negotiate
// does not change.

// exportVersions is what server/discover and -32022 report: the versions this
// server actually answers.
var exportVersions = []string{modernProtocolVersion, legacyProtocolVersion}

// errHeaderMismatch is HeaderMismatchError (2026-07-28): the HTTP headers and
// the JSON-RPC body disagree.
const errHeaderMismatch = -32020

// requestVersion returns the protocol version a modern request declares in
// params._meta, or "" for a legacy request.
func requestVersion(params map[string]any) string {
	meta, _ := params["_meta"].(map[string]any)
	v, _ := meta[metaProtocolVersion].(string)
	return v
}

// handleDiscover answers server/discover. The tool list is filtered per agent
// and changes with policy reloads and catalogue pins, so nothing is cacheable
// beyond this caller and this moment.
func (s *Server) handleDiscover() map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": exportVersions,
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"ttlMs":             0,
		"cacheScope":        "private",
		"_meta":             map[string]any{metaServerInfo: map[string]any{"name": "flux7-mesh", "version": "0.1.0"}},
	}
}

// complete marks a result for a modern client. The map is copied: a tool
// result may be the upstream's own map, passed through unchanged.
func complete(result any) any {
	m, ok := result.(map[string]any)
	if !ok {
		return result
	}
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["resultType"] = "complete"
	return out
}

// headerMismatch checks the 2026-07-28 HTTP headers against the body and
// returns why they disagree, or "" when they agree.
//
// A gateway in front of the mesh may route or filter on Mcp-Name without
// reading the body, while the mesh judges the body. A request whose header
// names one tool and whose body calls another would pass such a filter under
// a false label, so a present header must match the body in either era. In
// the modern era the headers are also required.
func headerMismatch(h http.Header, req rpcRequest) string {
	bodyVersion := requestVersion(req.Params)
	headerVersion := h.Get("MCP-Protocol-Version")
	modern := bodyVersion != ""

	if modern && headerVersion != bodyVersion {
		if headerVersion == "" {
			return "the body envelope names protocol version " + bodyVersion + " but the MCP-Protocol-Version header is absent"
		}
		return "the body envelope names protocol version " + bodyVersion + " but the MCP-Protocol-Version header names " + headerVersion
	}

	method := h.Get("Mcp-Method")
	switch {
	case method == "" && modern:
		return "the body names method " + req.Method + " but the required Mcp-Method header is absent"
	case method != "" && method != req.Method:
		return "the body names method " + req.Method + " but the Mcp-Method header names " + method
	}

	if req.Method == "tools/call" {
		bodyName, _ := req.Params["name"].(string)
		name := h.Get("Mcp-Name")
		switch {
		case name == "" && modern:
			return "the body calls tool " + bodyName + " but the required Mcp-Name header is absent"
		case name != "" && name != bodyName:
			return "the body calls tool " + bodyName + " but the Mcp-Name header names " + name
		}
	}
	return ""
}
