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
	Action  string `json:"action"`  // allow, deny, human_approval
	Rule    string `json:"rule"`    // which policy/rule matched
	Reason  string `json:"reason"`  // human-readable explanation
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

func matchAgent(pattern, agentID string) bool {
	return match.Glob(pattern, agentID)
}

func matchTool(tools []string, toolName string) bool {
	return match.GlobAny(tools, toolName)
}

// evaluateCondition checks a single condition against params.
func evaluateCondition(cond *config.Condition, params map[string]any) bool {
	val := extractField(cond.Field, params)
	if val == nil {
		return false
	}

	numVal, err := toFloat(val)
	if err != nil {
		// String comparison for == and !=
		strVal := fmt.Sprintf("%v", val)
		target := fmt.Sprintf("%v", cond.Value)
		switch cond.Operator {
		case "==":
			return strVal == target
		case "!=":
			return strVal != target
		default:
			return false
		}
	}

	switch cond.Operator {
	case "<":
		return numVal < cond.Value
	case "<=":
		return numVal <= cond.Value
	case ">":
		return numVal > cond.Value
	case ">=":
		return numVal >= cond.Value
	case "==":
		return numVal == cond.Value
	case "!=":
		return numVal != cond.Value
	default:
		return false
	}
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
