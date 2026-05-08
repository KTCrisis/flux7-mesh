package policy

import (
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
)

func testEngine() *Engine {
	return NewEngine([]config.Policy{
		{
			Name:  "support",
			Agent: "support-*",
			Rules: []config.Rule{
				{Tools: []string{"get_order", "get_customer"}, Action: "allow"},
				{Tools: []string{"create_refund"}, Action: "allow", Condition: &config.Condition{
					Field: "params.amount", Operator: "<", Value: 500,
				}},
				{Tools: []string{"create_refund"}, Action: "deny", Condition: &config.Condition{
					Field: "params.amount", Operator: ">=", Value: 500,
				}},
				{Tools: []string{"delete_customer"}, Action: "deny"},
			},
		},
		{
			Name:  "admin",
			Agent: "admin-*",
			Rules: []config.Rule{
				{Tools: []string{"*"}, Action: "allow"},
			},
		},
		{
			Name:  "default",
			Agent: "*",
			Rules: []config.Rule{
				{Tools: []string{"*"}, Action: "deny"},
			},
		},
	})
}

func TestEvaluateAllow(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("support-bot", "get_order", nil)
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow", d.Action)
	}
	if d.Rule != "support" {
		t.Errorf("rule = %q, want support", d.Rule)
	}
}

func TestEvaluateDeny(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("support-bot", "delete_customer", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny", d.Action)
	}
}

func TestEvaluateConditionAllow(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("support-bot", "create_refund", map[string]any{
		"params": map[string]any{"amount": 100.0},
	})
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow (amount < 500)", d.Action)
	}
}

func TestEvaluateConditionDeny(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("support-bot", "create_refund", map[string]any{
		"params": map[string]any{"amount": 999.0},
	})
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny (amount >= 500)", d.Action)
	}
}

func TestEvaluateWildcardAgent(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("admin-1", "delete_customer", nil)
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow (admin wildcard)", d.Action)
	}
}

func TestEvaluateDefaultDeny(t *testing.T) {
	e := testEngine()
	d := e.Evaluate("random-agent", "get_order", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny (default policy)", d.Action)
	}
}

func TestEvaluateNoMatchFailClosed(t *testing.T) {
	e := NewEngine([]config.Policy{})
	d := e.Evaluate("anyone", "anything", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny (fail closed)", d.Action)
	}
	if d.Rule != "default" {
		t.Errorf("rule = %q, want default", d.Rule)
	}
}

func TestEvaluateMCPNamespacedTools(t *testing.T) {
	e := NewEngine([]config.Policy{
		{
			Name:  "mcp-policy",
			Agent: "claude",
			Rules: []config.Rule{
				{Tools: []string{"filesystem.read_file", "filesystem.list_directory"}, Action: "allow"},
				{Tools: []string{"filesystem.write_file"}, Action: "deny"},
			},
		},
	})

	d := e.Evaluate("claude", "filesystem.read_file", nil)
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow", d.Action)
	}

	d = e.Evaluate("claude", "filesystem.write_file", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny", d.Action)
	}

	// Not in policy → fail closed
	d = e.Evaluate("claude", "filesystem.delete_file", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny (not listed)", d.Action)
	}
}

func TestSpecificPolicyBeforeWildcard(t *testing.T) {
	// Policies deliberately in wrong alphabetical order: default (*) before scout7.
	// The engine must sort by specificity so scout7 matches first.
	e := NewEngine([]config.Policy{
		{
			Name:  "default",
			Agent: "*",
			Rules: []config.Rule{
				{Tools: []string{"*"}, Action: "deny"},
			},
		},
		{
			Name:  "scout7",
			Agent: "scout7",
			Rules: []config.Rule{
				{Tools: []string{"searxng.*"}, Action: "allow"},
				{Tools: []string{"*"}, Action: "deny"},
			},
		},
	})

	d := e.Evaluate("scout7", "searxng.web_search", nil)
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow (specific policy should match before wildcard)", d.Action)
	}
	if d.Rule != "scout7" {
		t.Errorf("rule = %q, want scout7", d.Rule)
	}

	// Unknown agent still hits default deny
	d = e.Evaluate("unknown", "searxng.web_search", nil)
	if d.Action != "deny" {
		t.Errorf("action = %q, want deny (unknown agent → default)", d.Action)
	}
}

func TestPartialWildcardBeforeCatchAll(t *testing.T) {
	// Partial wildcard (support-*) should match before catch-all (*).
	e := NewEngine([]config.Policy{
		{
			Name:  "default",
			Agent: "*",
			Rules: []config.Rule{
				{Tools: []string{"*"}, Action: "deny"},
			},
		},
		{
			Name:  "support",
			Agent: "support-*",
			Rules: []config.Rule{
				{Tools: []string{"get_order"}, Action: "allow"},
			},
		},
	})

	d := e.Evaluate("support-bot", "get_order", nil)
	if d.Action != "allow" {
		t.Errorf("action = %q, want allow (partial wildcard before catch-all)", d.Action)
	}
	if d.Rule != "support" {
		t.Errorf("rule = %q, want support", d.Rule)
	}
}

func TestEvaluateToolGlobPattern(t *testing.T) {
	e := NewEngine([]config.Policy{
		{Name: "claude", Agent: "claude", Rules: []config.Rule{
			{Tools: []string{"weather.*"}, Action: "allow"},
			{Tools: []string{"gmail.gmail_read_*"}, Action: "allow"},
			{Tools: []string{"gmail.*"}, Action: "deny"},
		}},
	})

	// weather.* matches any weather tool
	d := e.Evaluate("claude", "weather.weather_forecast", nil)
	if d.Action != "allow" {
		t.Errorf("weather glob: action = %q, want allow", d.Action)
	}

	// gmail.gmail_read_* matches read tools
	d = e.Evaluate("claude", "gmail.gmail_read_email", nil)
	if d.Action != "allow" {
		t.Errorf("gmail read glob: action = %q, want allow", d.Action)
	}

	// gmail.* catches the rest as deny
	d = e.Evaluate("claude", "gmail.gmail_send_email", nil)
	if d.Action != "deny" {
		t.Errorf("gmail catch-all: action = %q, want deny", d.Action)
	}
}
