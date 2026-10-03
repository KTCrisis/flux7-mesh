// Package callmeta carries per-call metadata from the proxy to an upstream
// MCP server, in the `_meta` field of tools/call, the place the MCP
// specification reserves for it. The mesh puts the W3C trace context there,
// so the upstream can record which governed call produced what it stores,
// and, for upstreams that opt in with forward_identity, the agent the mesh
// authenticated.
package callmeta

import "context"

// Traceparent carries the W3C traceparent of the mesh's span.
const Traceparent = "traceparent"

// Agent carries the calling agent's identity, as the mesh established it.
// Sent only to upstreams configured with forward_identity: true.
const Agent = "art.flux7/agent"

type key struct{}

// With attaches the metadata of the next tools/call to ctx.
func With(ctx context.Context, meta map[string]any) context.Context {
	return context.WithValue(ctx, key{}, meta)
}

// From returns the metadata attached to ctx, or nil.
func From(ctx context.Context) map[string]any {
	meta, _ := ctx.Value(key{}).(map[string]any)
	return meta
}
