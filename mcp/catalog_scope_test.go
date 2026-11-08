package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// scopeServer: alice may read, never delete; a CLI tool carries a binary
// path. Grants exist for alice, for bob and for everyone.
func scopeServer(agent string, supervisors []string) *Server {
	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "read", Source: "openapi"})
	reg.LoadManual(&registry.Tool{Name: "delete", Source: "openapi"})
	reg.LoadManual(&registry.Tool{Name: "tf.plan", Source: "cli", CLIMeta: &registry.CLIToolMeta{Bin: "/opt/secret/terraform"}})
	pol := policy.NewEngine([]config.Policy{
		{Name: "p", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"read", "tf.plan"}, Action: "allow"},
			{Tools: []string{"*"}, Action: "deny"},
		}},
	})
	traces := trace.NewStore(10)
	h := proxy.NewHandler(reg, pol, traces)
	h.Grants = grant.NewStore()
	h.Grants.Add("alice", "read", "op", time.Hour)
	h.Grants.Add("bob", "delete", "op", time.Hour)
	h.Grants.Add("*", "tf.*", "op", time.Hour)
	return &Server{Registry: reg, Policy: pol, Traces: traces, Handler: h, AgentID: agent, SupervisorAgents: supervisors}
}

func catalogText(t *testing.T, s *Server) string {
	t.Helper()
	res, rpcErr := s.handleCatalog(map[string]any{})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	b, _ := json.Marshal(res)
	return string(b)
}

// An agent's catalogue lists only what it can reach: no tool it can never
// call (hide_denied off), no runtime details, no operator-only tools.
func TestCatalogScopedToAgent(t *testing.T) {
	text := catalogText(t, scopeServer("alice", nil))
	for _, leak := range []string{`\"delete\"`, "runtime", "/opt/secret", "approval.resolve", "grant.create"} {
		if strings.Contains(text, leak) {
			t.Errorf("agent catalogue leaks %s:\n%s", leak, text)
		}
	}
	for _, want := range []string{`\"read\"`, `\"tf.plan\"`, "mesh.catalog", "grant.list"} {
		if !strings.Contains(text, want) {
			t.Errorf("agent catalogue misses %s", want)
		}
	}
}

// A supervisor keeps the full view.
func TestCatalogFullForSupervisor(t *testing.T) {
	text := catalogText(t, scopeServer("sup7", []string{"sup7"}))
	for _, want := range []string{`\"delete\"`, "/opt/secret/terraform", "approval.resolve", "grant.create"} {
		if !strings.Contains(text, want) {
			t.Errorf("supervisor catalogue misses %s:\n%s", want, text)
		}
	}
}

// grant.list shows an agent the grants that apply to it, not another
// agent's; a supervisor sees them all.
func TestGrantListScopedToAgent(t *testing.T) {
	list := func(s *Server) string {
		res, _ := s.handleGrantList()
		b, _ := json.Marshal(res)
		return string(b)
	}
	alice := list(scopeServer("alice", nil))
	if !strings.Contains(alice, "agent: alice") || !strings.Contains(alice, "agent: *") {
		t.Errorf("alice misses her grants: %s", alice)
	}
	if strings.Contains(alice, "agent: bob") {
		t.Errorf("alice sees bob's grant: %s", alice)
	}
	if sup := list(scopeServer("sup7", []string{"sup7"})); !strings.Contains(sup, "agent: bob") {
		t.Errorf("supervisor misses bob's grant: %s", sup)
	}
}
