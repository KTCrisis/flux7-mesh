package mcp

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeForwarder stands in for the upstream MCP servers: every tools/call
// returns the configured result or error.
type fakeForwarder struct {
	result any
	err    error
}

func (f fakeForwarder) CallTool(context.Context, string, string, map[string]any) (any, error) {
	return f.result, f.err
}
func (f fakeForwarder) ServerStatuses() any { return nil }

func callFsRead(t *testing.T, fwd fakeForwarder) map[string]any {
	t.Helper()
	s := testServer()
	s.Handler.MCPForwarder = fwd
	responses := sendRPC(t, s, rpcRequest{
		JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "fs.read_file", "arguments": map[string]any{"path": "/x"}},
	})
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("rpc error: %+v", responses[0].Error)
	}
	result, ok := responses[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T", responses[0].Result)
	}
	return result
}

// An upstream CallToolResult reaches the client as the upstream sent it:
// isError, structuredContent and non-text blocks are not flattened into text.
func TestMCPResultPassesThrough(t *testing.T) {
	upstream := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "Access denied"},
			map[string]any{"type": "image", "data": "iVBORw0KGgo=", "mimeType": "image/png"},
		},
		"structuredContent": map[string]any{"reason": "outside allowed directories"},
		"isError":           true,
	}
	got := callFsRead(t, fakeForwarder{result: upstream})
	if !reflect.DeepEqual(got, upstream) {
		t.Errorf("result altered by the proxy:\n got  %#v\n want %#v", got, upstream)
	}
}

// A backend failure (upstream down, transport error) is a tool-level error
// a programmatic client can see, not an apparent success.
func TestBackendErrorSetsIsError(t *testing.T) {
	got := callFsRead(t, fakeForwarder{err: errors.New("connection refused")})
	if got["isError"] != true {
		t.Errorf("isError = %v, want true", got["isError"])
	}
	content, _ := got["content"].([]any)
	first, _ := content[0].(map[string]any)
	if text, _ := first["text"].(string); text != "Backend error: connection refused" {
		t.Errorf("text = %q", text)
	}
}

// An MCP upstream answer without a content array is wrapped as text rather
// than forwarded malformed.
func TestMalformedMCPResultIsWrapped(t *testing.T) {
	got := callFsRead(t, fakeForwarder{result: map[string]any{"value": 42}})
	content, ok := got["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v", got["content"])
	}
	if _, has := got["value"]; has {
		t.Error("malformed result forwarded as is")
	}
}

// Non-MCP backends (OpenAPI, CLI) return plain data: still one text block.
func TestPlainBackendResultIsWrapped(t *testing.T) {
	got := toolResult(nil, map[string]any{"order": "123"})
	m, _ := got.(map[string]any)
	content, _ := m["content"].([]map[string]any)
	if len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("content = %#v", m["content"])
	}
	if _, has := m["isError"]; has {
		t.Error("plain result marked as error")
	}
}
