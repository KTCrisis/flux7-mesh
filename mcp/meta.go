package mcp

import (
	"context"

	"github.com/KTCrisis/flux7-mesh/internal/callmeta"
)

// callMeta returns the `_meta` of a tools/call to this upstream (see package
// callmeta): the agent is dropped unless the upstream accepts identities.
func (c *MCPClient) callMeta(ctx context.Context) map[string]any {
	meta := callmeta.From(ctx)
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]any, len(meta))
	for k, v := range meta {
		if k == callmeta.Agent && !c.ForwardIdentity {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
