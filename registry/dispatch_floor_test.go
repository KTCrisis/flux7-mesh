package registry

import (
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
)

func TestDispatchFloorOnlyAppliesToCatchAll(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name:          "terraform",
			Bin:           "terraform",
			DefaultAction: "deny",
			Commands: map[string]config.CLICommandConfig{
				"plan": {},
			},
		},
	})

	// A declared command carries no floor — declaring it is the operator's grant.
	if got := r.Get("terraform.plan").DispatchFloor(); got != "" {
		t.Errorf("declared command floor = %q, want empty", got)
	}

	// The dispatcher carries the floor: it accepts any subcommand.
	if got := r.Get("terraform.__dispatch").DispatchFloor(); got != "deny" {
		t.Errorf("dispatcher floor = %q, want %q", got, "deny")
	}

	// An undeclared subcommand resolves to the dispatcher, and so inherits its floor.
	if got := r.ResolveCLI("terraform.destroy").DispatchFloor(); got != "deny" {
		t.Errorf("undeclared subcommand floor = %q, want %q", got, "deny")
	}
}

func TestDispatchFloorDefaultsToDenyWhenUnset(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{Name: "gh", Bin: "gh", Commands: map[string]config.CLICommandConfig{"pr": {}}},
	})

	if got := r.Get("gh.__dispatch").DispatchFloor(); got != "deny" {
		t.Errorf("unset default_action floor = %q, want %q", got, "deny")
	}
}

func TestDispatchFloorAbsentForStrictAndBare(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name:          "docker",
			Bin:           "docker",
			Strict:        true,
			DefaultAction: "deny",
			Commands:      map[string]config.CLICommandConfig{"ps": {}},
		},
		{
			Name:          "play7",
			Bin:           "play7",
			DefaultAction: "deny",
			Bare:          &config.CLICommandConfig{},
		},
	})

	// Strict tools register no dispatcher at all.
	if tool := r.Get("docker.__dispatch"); tool != nil {
		t.Errorf("strict tool registered a dispatcher: %+v", tool)
	}
	// Bare tools expose a single .run entry, which is a declared surface.
	if got := r.Get("play7.run").DispatchFloor(); got != "" {
		t.Errorf("bare tool floor = %q, want empty", got)
	}
}

func TestDispatchFloorNilSafe(t *testing.T) {
	var tool *Tool
	if got := tool.DispatchFloor(); got != "" {
		t.Errorf("nil tool floor = %q, want empty", got)
	}
	if got := (&Tool{Name: "http_thing", Source: "openapi"}).DispatchFloor(); got != "" {
		t.Errorf("non-CLI tool floor = %q, want empty", got)
	}
}
