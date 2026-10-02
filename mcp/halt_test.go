package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/halt"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// An agent connected over MCP is stopped by a halt even though its policy
// allows the tool, and the backend is never reached.
func TestMCPCallHalted(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "read", Source: "openapi", Method: "GET", Path: "/r", BaseURL: backend.URL})
	pol := policy.NewEngine([]config.Policy{{Name: "p", Agent: "*", Rules: []config.Rule{
		{Tools: []string{"*"}, Action: "allow"}}}})
	traces := trace.NewStore(50)
	h := proxy.NewHandler(reg, pol, traces)
	h.Halts, _ = halt.NewStore(nil)
	s := &Server{Registry: reg, Policy: pol, Traces: traces, Handler: h, AgentID: "claude", SessionID: "s1"}

	call := func() string {
		res := sendRPC(t, s, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
			Params: map[string]any{"name": "read", "arguments": map[string]any{}}})
		return extractText(t, res[0])
	}

	h.Halts.Start(halt.ScopeSession, "s1", "runaway loop", "marc")
	if out := call(); !strings.Contains(out, "halted by operator") || !strings.Contains(out, "runaway loop") {
		t.Fatalf("expected the halt message, got:\n%s", out)
	}
	if hits.Load() != 0 {
		t.Fatal("a halted call reached the backend")
	}

	active := h.Halts.Active()
	h.Halts.Resume(active[0].ID, "marc")
	call()
	if hits.Load() != 1 {
		t.Fatal("after resume the call must run")
	}
}
