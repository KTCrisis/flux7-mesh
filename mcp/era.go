package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Protocol eras of an upstream MCP server.
//
// Up to 2025-11-25 a client opens with initialize and the server may hand out
// a session. The 2026-07-28 revision drops both: every request stands alone
// and carries the protocol version, the client's identity and capabilities in
// a per-request envelope (params._meta), mirrored over HTTP by the
// MCP-Protocol-Version, Mcp-Method and Mcp-Name headers. A server may serve
// both eras (the official SDK's default) or the modern one only.
//
// mesh7 opens every upstream with initialize, as it always has, and switches
// to the modern era only when the server refuses initialize with -32022 and
// offers 2026-07-28. Starting legacy-first never disturbs an existing server:
// some old stdio servers exit on any request before initialize, which is why
// the official SDK probes them in a throwaway process. Reference capture of a
// modern exchange: docs/internal/capture-mcp-2026-07-28-client.log.
const (
	modernProtocolVersion = "2026-07-28"
	legacyProtocolVersion = "2024-11-05"

	// errUnsupportedProtocolVersion is the code a server answers to a
	// protocol version it does not speak; its data lists the ones it does.
	errUnsupportedProtocolVersion = -32022

	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

var clientInfo = map[string]any{"name": "flux7-mesh", "version": "0.1.0"}

// httpStatusError is an upstream HTTP answer of 400 or more. The body is kept
// so a JSON-RPC error inside it (such as -32022) can be read.
type httpStatusError struct {
	Status int
	Body   []byte
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("streamable HTTP POST: status %d: %s", e.Status, string(trimBytes(e.Body)))
}

// offersModern reports whether a failed initialize is a server that refuses
// the legacy era and offers 2026-07-28. The refusal arrives either as an HTTP
// error whose body is a JSON-RPC error, or as a JSON-RPC error in a normal
// response (stdio).
func offersModern(err error, resp rpcResponse) bool {
	rpcErr := resp.Error
	var hse *httpStatusError
	if err != nil && errors.As(err, &hse) {
		var body struct {
			Error *rpcError `json:"error"`
		}
		if json.Unmarshal(hse.Body, &body) == nil {
			rpcErr = body.Error
		}
	}
	if rpcErr == nil || rpcErr.Code != errUnsupportedProtocolVersion {
		return false
	}
	data, _ := rpcErr.Data.(map[string]any)
	supported, _ := data["supported"].([]any)
	return slices.Contains(supported, any(modernProtocolVersion))
}

// withEnvelope returns params with the modern per-request envelope added to
// its _meta, keeping what _meta already carries (trace context, agent). The
// caller's map is not modified.
func withEnvelope(params map[string]any) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	meta := map[string]any{}
	if m, ok := params["_meta"].(map[string]any); ok {
		for k, v := range m {
			meta[k] = v
		}
	}
	meta[metaProtocolVersion] = modernProtocolVersion
	meta[metaClientInfo] = clientInfo
	meta[metaClientCapabilities] = map[string]any{}
	out["_meta"] = meta
	return out
}

// modernResult checks a modern result and strips what only a modern client
// reads. mesh7 serves its own clients the legacy era, so resultType, the
// cache hints and the server's identity in _meta stop here. A resultType
// other than complete (input_required: the server asks the client for more
// before it answers) is not supported yet and becomes an error rather than
// an incomplete result passed off as an answer.
func modernResult(result any) (any, error) {
	m, ok := result.(map[string]any)
	if !ok {
		return result, nil
	}
	if rt, ok := m["resultType"].(string); ok && rt != "complete" {
		return nil, fmt.Errorf("upstream answered resultType %q, which mesh7 does not support yet", rt)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch k {
		case "resultType", "ttlMs", "cacheScope":
			continue
		}
		out[k] = v
	}
	if meta, ok := m["_meta"].(map[string]any); ok {
		kept := make(map[string]any, len(meta))
		for k, v := range meta {
			if k != metaServerInfo {
				kept[k] = v
			}
		}
		if len(kept) == 0 {
			delete(out, "_meta")
		} else {
			out["_meta"] = kept
		}
	}
	return out, nil
}

// modernHeaders derives the HTTP headers of a modern request from its body,
// so the two can never disagree: a modern server refuses a mismatch (-32020).
func modernHeaders(body []byte) map[string]string {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil || req.Method == "" {
		return nil
	}
	h := map[string]string{
		"MCP-Protocol-Version": modernProtocolVersion,
		"Mcp-Method":           req.Method,
	}
	switch req.Method {
	case "tools/call", "prompts/get":
		if req.Params.Name != "" {
			h["Mcp-Name"] = req.Params.Name
		}
	case "resources/read":
		if req.Params.URI != "" {
			h["Mcp-Name"] = req.Params.URI
		}
	}
	return h
}
