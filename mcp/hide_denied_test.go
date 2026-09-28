package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

func toolNames(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	res := sendRPC(t, s, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"})
	result, _ := res[0].Result.(map[string]any)
	names := map[string]bool{}
	switch tools := result["tools"].(type) {
	case []any:
		for _, x := range tools {
			m, _ := x.(map[string]any)
			names[m["name"].(string)] = true
		}
	case []MCPTool:
		for _, x := range tools {
			names[x.Name] = true
		}
	}
	return names
}

func hideServer(hide bool) *Server {
	reg := registry.New()
	for _, n := range []string{"open", "ask", "never", "maybe", "floored"} {
		reg.LoadManual(&registry.Tool{Name: "x." + n, Source: "openapi"})
	}
	reg.LoadManual(&registry.Tool{Name: "tf.__dispatch", Source: "cli",
		CLIMeta: &registry.CLIToolMeta{Bin: "terraform", IsCatchAll: true, DefaultAction: "deny"}})

	pol := policy.NewEngine([]config.Policy{
		{Name: "claude", Agent: "claude", Rules: []config.Rule{
			{Tools: []string{"x.open"}, Action: "allow"},
			{Tools: []string{"x.ask"}, Action: "human_approval"},
			// maybe: denied unless its argument matches, so it must stay visible
			{Tools: []string{"x.maybe"}, Action: "allow",
				Condition: &config.Condition{Field: "id", Operator: "==", Value: config.CondValue{Strings: []string{"1"}}}},
			// the policy allows the dispatcher, but its floor of deny wins
			{Tools: []string{"tf.*"}, Action: "allow"},
		}},
		{Name: "default", Agent: "*", Rules: []config.Rule{{Tools: []string{"*"}, Action: "deny"}}},
	})
	traces := trace.NewStore(10)
	return &Server{Registry: reg, Policy: pol, Traces: traces,
		Handler: proxy.NewHandler(reg, pol, traces), AgentID: "claude", HideDenied: hide}
}

func TestHideDeniedTools(t *testing.T) {
	names := toolNames(t, hideServer(true))
	for _, want := range []string{"x.open", "x.ask", "x.maybe"} {
		if !names[want] {
			t.Errorf("%s hidden, but the policy can let it through", want)
		}
	}
	for _, gone := range []string{"x.never", "x.floored", "tf.__dispatch"} {
		if names[gone] {
			t.Errorf("%s listed, but every path ends in deny", gone)
		}
	}
	// The mesh's own tools are not policy-listed and stay.
	if !names["mesh.catalog"] {
		t.Error("virtual mesh tools must stay listed")
	}
}

func TestHideDeniedToolsOffByDefault(t *testing.T) {
	names := toolNames(t, hideServer(false))
	for _, n := range []string{"x.never", "x.floored", "tf.__dispatch"} {
		if !names[n] {
			t.Errorf("%s hidden with the option off", n)
		}
	}
}

func TestHiddenToolIsStillDeniedWhenCalled(t *testing.T) {
	s := hideServer(true)
	res := sendRPC(t, s, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "x.never", "arguments": map[string]any{}}})
	// Hiding is not the boundary: the call is evaluated and refused as usual.
	if !strings.Contains(fmt.Sprint(res[0].Result), "Policy denied") {
		t.Errorf("a hand-written call to a hidden tool was not refused: %+v", res[0])
	}
}
