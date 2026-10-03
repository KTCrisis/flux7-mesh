package proxy

import (
	"context"
	"strings"
	"testing"

	"net/http/httptest"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/internal/callmeta"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// metaForwarder records the call metadata the proxy attached.
type metaForwarder struct{ meta map[string]any }

func (m *metaForwarder) CallTool(ctx context.Context, _, _ string, _ map[string]any) (any, error) {
	m.meta = callmeta.From(ctx)
	return map[string]any{"content": []any{}}, nil
}

func (m *metaForwarder) ServerStatuses() any { return nil }

func TestForwardMCPCarriesTraceAndAgent(t *testing.T) {
	reg := registry.New()
	reg.LoadMCP("memory", []registry.MCPToolDef{{Name: "memory_store"}})
	pol := policy.NewEngine([]config.Policy{
		{Name: "allow-all", Agent: "*", Rules: []config.Rule{{Tools: []string{"*"}, Action: "allow"}}},
	})
	handler := NewHandler(reg, pol, trace.NewStore(100))
	fw := &metaForwarder{}
	handler.MCPForwarder = fw

	req := newLoopbackReq("POST", "/tool/memory.memory_store", strings.NewReader(`{"params":{"key":"k","value":"v"}}`))
	req.Header.Set("Authorization", "Bearer agent:scout7")
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if fw.meta[callmeta.Agent] != "scout7" {
		t.Errorf("agent in _meta: %v", fw.meta)
	}
	tp, _ := fw.meta[callmeta.Traceparent].(string)
	if !strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Errorf("traceparent keeps the caller's trace id: %q", tp)
	}
}
