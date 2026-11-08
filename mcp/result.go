package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/KTCrisis/flux7-mesh/registry"
)

// toolResult is the tools/call result the client receives for a backend
// result.
//
// An MCP upstream already answers with a CallToolResult, and mesh7 is a
// proxy: the result passes through unchanged, so isError, structuredContent
// and non-text content (images, audio, embedded resources) reach the client
// as the upstream sent them. Until v0.20 the whole result was serialized
// into one text block, which turned an upstream error into an apparent
// success and an image into base64 inside JSON.
//
// Any other backend (OpenAPI, CLI) returns plain data, which is wrapped as
// one text block, the only shape a tools/call result can carry it in. An
// MCP upstream whose answer has no content array is wrapped the same way
// rather than forwarded malformed.
func toolResult(tool *registry.Tool, result any) any {
	if tool != nil && tool.Source == "mcp" {
		if m, ok := result.(map[string]any); ok {
			if _, ok := m["content"].([]any); ok {
				return m
			}
		}
	}
	return textResult(result)
}

// textResult wraps plain backend data as a single text block.
func textResult(result any) map[string]any {
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("Failed to serialize result: %s", err.Error()))
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(b)},
		},
	}
}

// errorResult is a tool-level failure: the text tells the model what went
// wrong, isError tells a programmatic client the call did not succeed.
func errorResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"isError": true,
	}
}
