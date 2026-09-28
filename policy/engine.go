package policy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/internal/match"
)

// Decision is the result of a policy evaluation.
type Decision struct {
	Action string `json:"action"` // allow, deny, human_approval
	Rule   string `json:"rule"`   // which policy/rule matched
	Reason string `json:"reason"` // human-readable explanation
}

// Engine evaluates tool calls against configured policies.
// Thread-safe: Evaluate uses RLock, Reload uses Lock.
type Engine struct {
	mu       sync.RWMutex
	policies []config.Policy
}

func NewEngine(policies []config.Policy) *Engine {
	e := &Engine{}
	e.policies = sortPolicies(policies)
	return e
}

// Reload atomically swaps the policy set. The caller is responsible for
// validation before calling Reload — an empty slice is accepted (fail-closed).
// Policies returns a snapshot of the current policy set (sorted by specificity).
func (e *Engine) Policies() []config.Policy {
	e.mu.RLock()
	out := make([]config.Policy, len(e.policies))
	copy(out, e.policies)
	e.mu.RUnlock()
	return out
}

func (e *Engine) Reload(policies []config.Policy) {
	sorted := sortPolicies(policies)
	e.mu.Lock()
	e.policies = sorted
	e.mu.Unlock()
}

func sortPolicies(policies []config.Policy) []config.Policy {
	sorted := make([]config.Policy, len(policies))
	copy(sorted, policies)
	sort.SliceStable(sorted, func(i, j int) bool {
		return agentSpecificity(sorted[i].Agent) > agentSpecificity(sorted[j].Agent)
	})
	return sorted
}

// agentSpecificity scores an agent pattern: exact match > partial wildcard > catch-all.
func agentSpecificity(pattern string) int {
	if pattern == "*" {
		return 0
	}
	if strings.ContainsAny(pattern, "*?") {
		return 1
	}
	return 2
}

// Evaluate checks if an agent can call a tool with given params.
// Returns the first matching rule's decision. Default: deny.
func (e *Engine) Evaluate(agentID string, toolName string, params map[string]any) Decision {
	e.mu.RLock()
	policies := e.policies
	e.mu.RUnlock()

	for _, pol := range policies {
		if !matchAgent(pol.Agent, agentID) {
			continue
		}

		for _, rule := range pol.Rules {
			if !matchTool(rule.Tools, toolName) {
				continue
			}

			// Check condition if present
			if rule.Condition != nil {
				if !evaluateCondition(rule.Condition, params) {
					continue // condition not met, try next rule
				}
			}

			return Decision{
				Action: rule.Action,
				Rule:   pol.Name,
				Reason: fmt.Sprintf("policy=%s tool=%s action=%s", pol.Name, toolName, rule.Action),
			}
		}
	}

	// No rule matched → fail closed
	return Decision{
		Action: "deny",
		Rule:   "default",
		Reason: "no matching policy — fail closed",
	}
}

// StaticDecision is what the policy says about a tool before any call is
// made, for display and review. It is not a substitute for Evaluate: rules
// with a condition depend on the call's arguments, so they are listed in
// Conditional rather than decided. Action is where evaluation lands when none
// of them matches.
type StaticDecision struct {
	Action      string            `json:"action"`
	Rule        string            `json:"rule"`
	Conditional []ConditionalRule `json:"conditional,omitempty"`

	// Where the deciding rule lives: the policy file (empty for inline
	// policies and for the default deny) and the rule's index in that
	// policy (-1 for the default deny).
	SourceFile string `json:"source_file,omitempty"`
	RuleIndex  int    `json:"rule_index"`

	// PolicyAgent is the agent pattern of the deciding policy, so an editor
	// can tell a policy of this agent from a shared glob one.
	PolicyAgent string `json:"-"`
}

// ConditionalRule is a matching rule whose outcome depends on an argument.
// It is evaluated before Action, in this order, and wins when its condition
// holds for the call.
type ConditionalRule struct {
	Action   string `json:"action"`
	Rule     string `json:"rule"`
	Field    string `json:"field"`
	Operator string `json:"operator"`
}

// Explain walks the rules the way Evaluate does, without arguments: rules
// with a condition are collected instead of evaluated, and the first rule
// without one gives the fallthrough action.
func (e *Engine) Explain(agentID, toolName string) StaticDecision {
	e.mu.RLock()
	policies := e.policies
	e.mu.RUnlock()

	var out StaticDecision
	for _, pol := range policies {
		if !matchAgent(pol.Agent, agentID) {
			continue
		}
		for i, rule := range pol.Rules {
			if !matchTool(rule.Tools, toolName) {
				continue
			}
			if rule.Condition != nil {
				out.Conditional = append(out.Conditional, ConditionalRule{
					Action:   rule.Action,
					Rule:     pol.Name,
					Field:    rule.Condition.Field,
					Operator: rule.Condition.Operator,
				})
				continue
			}
			out.Action, out.Rule = rule.Action, pol.Name
			out.SourceFile, out.RuleIndex, out.PolicyAgent = pol.SourceFile, i, pol.Agent
			return out
		}
	}
	out.Action, out.Rule, out.RuleIndex = "deny", "default", -1
	return out
}

func matchAgent(pattern, agentID string) bool {
	return match.Glob(pattern, agentID)
}

func matchTool(tools []string, toolName string) bool {
	return match.GlobAny(tools, toolName)
}

// evaluateCondition checks a single condition against params.
//
// A condition that cannot be evaluated — missing field, unknown operator — is
// false. The rule is then skipped and evaluation continues, which ends at the
// default deny. That holds in both directions: an `allow` whose condition
// cannot be checked does not allow, and a `deny` whose condition cannot be
// checked does not deny but leaves nothing permitting the call either.
func evaluateCondition(cond *config.Condition, params map[string]any) bool {
	val := extractField(cond.Field, params)
	if val == nil {
		return false
	}

	// String operators run on the rendered value, whatever its type. A list of
	// shell arguments renders as `[rm -rf /]`, so `contains: "rm -rf"` sees it —
	// the alternative would be to silently ignore every tool that takes argv
	// rather than a command line.
	switch cond.Operator {
	case "contains", "not_contains", "starts_with", "not_starts_with":
		return evaluateString(cond, fmt.Sprintf("%v", val))
	}

	numVal, err := toFloat(val)
	if err != nil {
		// A non-numeric field compared with == or != is a string comparison.
		strVal := fmt.Sprintf("%v", val)
		switch cond.Operator {
		case "==":
			return anyOf(cond.Value.Strings, func(s string) bool { return strVal == s })
		case "!=":
			return !anyOf(cond.Value.Strings, func(s string) bool { return strVal == s })
		default:
			return false
		}
	}

	if !cond.Value.IsNum {
		// A numeric field against a string operand: only equality is meaningful,
		// and it is compared as text so `value: "42"` still behaves.
		strVal := fmt.Sprintf("%v", val)
		switch cond.Operator {
		case "==":
			return anyOf(cond.Value.Strings, func(s string) bool { return strVal == s })
		case "!=":
			return !anyOf(cond.Value.Strings, func(s string) bool { return strVal == s })
		}
		return false
	}

	switch cond.Operator {
	case "<":
		return numVal < cond.Value.Num
	case "<=":
		return numVal <= cond.Value.Num
	case ">":
		return numVal > cond.Value.Num
	case ">=":
		return numVal >= cond.Value.Num
	case "==":
		return numVal == cond.Value.Num
	case "!=":
		return numVal != cond.Value.Num
	default:
		return false
	}
}

// evaluateString applies the string operators. A list operand means "any of
// these" for the positive forms, and therefore "none of these" for the negated
// ones — which is what a deny list and an allow guard respectively need.
//
// Matching is case-sensitive. A rule that denies "rm -rf" does not stop
// "RM -RF", and pretending otherwise would invite the belief that this
// inspects intent rather than text.
func evaluateString(cond *config.Condition, haystack string) bool {
	needles := cond.Value.Strings
	if len(needles) == 0 {
		return false
	}

	switch cond.Operator {
	case "contains":
		return anyOf(needles, func(n string) bool { return strings.Contains(haystack, n) })
	case "not_contains":
		return !anyOf(needles, func(n string) bool { return strings.Contains(haystack, n) })
	case "starts_with":
		return anyOf(needles, func(n string) bool { return strings.HasPrefix(haystack, n) })
	case "not_starts_with":
		return !anyOf(needles, func(n string) bool { return strings.HasPrefix(haystack, n) })
	}
	return false
}

func anyOf(items []string, pred func(string) bool) bool {
	for _, it := range items {
		if pred(it) {
			return true
		}
	}
	return false
}

// extractField navigates a dotted path like "params.amount" in a nested map.
func extractField(field string, data map[string]any) any {
	parts := strings.Split(field, ".")
	var current any = data

	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}
