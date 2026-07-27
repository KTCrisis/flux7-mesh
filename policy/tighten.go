package policy

import "fmt"

// actionSeverity ranks actions from most to least permissive. Unknown actions
// rank as deny, so a typo in a config can never widen access.
func actionSeverity(action string) int {
	switch action {
	case "allow":
		return 0
	case "human_approval":
		return 1
	case "deny":
		return 2
	default:
		return 2
	}
}

// Tighten joins a decision with a floor action and keeps whichever is more
// restrictive. An empty floor leaves the decision untouched.
//
// The floor can only restrict, never widen: a floor of "allow" is a no-op by
// construction, and no floor can turn a policy deny into an allow. This keeps
// the policy engine the single authority on what is permitted, while letting a
// tool declaration refuse what the policy would have let through.
func Tighten(d Decision, floor string) Decision {
	if floor == "" || actionSeverity(floor) <= actionSeverity(d.Action) {
		return d
	}
	return Decision{
		Action: floor,
		Rule:   "default_action",
		Reason: fmt.Sprintf("dynamic dispatcher floor: default_action=%s (policy %s returned %s)", floor, d.Rule, d.Action),
	}
}
