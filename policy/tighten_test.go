package policy

import (
	"strings"
	"testing"
)

func TestTighten(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		floor    string
		want     string
	}{
		// No floor — decision untouched.
		{"empty floor keeps allow", "allow", "", "allow"},
		{"empty floor keeps deny", "deny", "", "deny"},

		// allow floor is a no-op by construction.
		{"allow floor never widens deny", "deny", "allow", "deny"},
		{"allow floor never widens approval", "human_approval", "allow", "human_approval"},
		{"allow floor keeps allow", "allow", "allow", "allow"},

		// A floor restricts.
		{"deny floor overrides allow", "allow", "deny", "deny"},
		{"deny floor overrides approval", "human_approval", "deny", "deny"},
		{"approval floor overrides allow", "allow", "human_approval", "human_approval"},
		{"approval floor never widens deny", "deny", "human_approval", "deny"},

		// An unknown floor value must not widen access. Config validation
		// rejects these, but a hand-edited registry entry must fail closed.
		{"unknown floor ranks as deny", "allow", "maybe", "maybe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Tighten(Decision{Action: tt.decision, Rule: "some-policy"}, tt.floor)
			if got.Action != tt.want {
				t.Errorf("Tighten(%q, %q).Action = %q, want %q", tt.decision, tt.floor, got.Action, tt.want)
			}
		})
	}
}

func TestTightenPreservesRuleWhenFloorDoesNotApply(t *testing.T) {
	in := Decision{Action: "deny", Rule: "claude", Reason: "explicit deny"}
	got := Tighten(in, "allow")
	if got != in {
		t.Errorf("Tighten() = %+v, want the decision unchanged (%+v)", got, in)
	}
}

func TestTightenReasonNamesBothSides(t *testing.T) {
	got := Tighten(Decision{Action: "allow", Rule: "claude"}, "deny")
	if got.Rule != "default_action" {
		t.Errorf("Rule = %q, want %q", got.Rule, "default_action")
	}
	// The trace must show what the policy said before the floor overrode it,
	// otherwise a deny looks like it came from the policy file.
	if !strings.Contains(got.Reason, "claude") || !strings.Contains(got.Reason, "allow") {
		t.Errorf("Reason = %q, want it to name the overridden policy and action", got.Reason)
	}
}
