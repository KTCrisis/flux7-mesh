package policy

import (
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
)

func cond(field, op string, v config.CondValue) *config.Condition {
	return &config.Condition{Field: field, Operator: op, Value: v}
}

func TestContains(t *testing.T) {
	tests := []struct {
		name   string
		op     string
		value  config.CondValue
		params map[string]any
		want   bool
	}{
		{"hit", "contains", config.Text("rm -rf"),
			map[string]any{"command": "sudo rm -rf /tmp"}, true},
		{"miss", "contains", config.Text("rm -rf"),
			map[string]any{"command": "ls -la"}, false},
		{"any of the list", "contains", config.Text("curl", "wget"),
			map[string]any{"command": "wget http://x"}, true},
		{"none of the list", "contains", config.Text("curl", "wget"),
			map[string]any{"command": "ls"}, false},

		{"not_contains passes when clean", "not_contains", config.Text("rm -rf"),
			map[string]any{"command": "ls -la"}, true},
		{"not_contains fails on a hit", "not_contains", config.Text("rm -rf"),
			map[string]any{"command": "rm -rf /"}, false},
		{"not_contains needs ALL absent", "not_contains", config.Text("curl", "sudo"),
			map[string]any{"command": "sudo ls"}, false},

		{"starts_with", "starts_with", config.Text("/home/fluxart"),
			map[string]any{"path": "/home/fluxart/notes"}, true},
		{"starts_with elsewhere", "starts_with", config.Text("/home/fluxart"),
			map[string]any{"path": "/etc/passwd"}, false},
		{"not_starts_with", "not_starts_with", config.Text("/etc", "/usr"),
			map[string]any{"path": "/home/fluxart/x"}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			field := "command"
			if _, ok := tc.params["path"]; ok {
				field = "path"
			}
			got := evaluateCondition(cond(field, tc.op, tc.value), tc.params)
			if got != tc.want {
				t.Errorf("%s %v on %v = %v, want %v", tc.op, tc.value.Strings, tc.params, got, tc.want)
			}
		})
	}
}

// Matching is case-sensitive, and the test states it so nobody later believes
// otherwise from reading the code alone.
func TestContainsIsCaseSensitive(t *testing.T) {
	c := cond("command", "contains", config.Text("rm -rf"))
	if evaluateCondition(c, map[string]any{"command": "RM -RF /"}) {
		t.Error("contains must not match across case")
	}
}

// Tools that take argv rather than a command line still get inspected: the
// value is rendered before matching.
func TestContainsSeesListArguments(t *testing.T) {
	c := cond("args", "contains", config.Text("--force"))
	params := map[string]any{"args": []any{"push", "--force", "origin"}}
	if !evaluateCondition(c, params) {
		t.Error("expected the rendered argument list to be searched")
	}
}

// A missing field makes the condition false in every direction. The rule is
// skipped, and evaluation ends at the default deny.
func TestMissingFieldIsFalseBothWays(t *testing.T) {
	params := map[string]any{"other": "x"}
	for _, op := range []string{"contains", "not_contains", "starts_with", "not_starts_with"} {
		if evaluateCondition(cond("command", op, config.Text("rm")), params) {
			t.Errorf("%s on a missing field must be false", op)
		}
	}
}

func TestEmptyValueIsFalse(t *testing.T) {
	c := cond("command", "contains", config.Text())
	if evaluateCondition(c, map[string]any{"command": "anything"}) {
		t.Error("a condition with nothing to match must not fire")
	}
}

func TestUnknownOperatorIsFalse(t *testing.T) {
	c := cond("command", "matches_regex", config.Text("rm"))
	if evaluateCondition(c, map[string]any{"command": "rm -rf"}) {
		t.Error("an unknown operator must not be treated as a match")
	}
}

// Numeric comparisons keep working, and equality on a real string now compares
// text rather than a stringified float.
func TestNumericAndStringEquality(t *testing.T) {
	if !evaluateCondition(cond("amount", "<", config.Num(500)), map[string]any{"amount": 100.0}) {
		t.Error("numeric < broke")
	}
	if evaluateCondition(cond("amount", "<", config.Num(500)), map[string]any{"amount": 900.0}) {
		t.Error("numeric < matched when it should not")
	}
	if !evaluateCondition(cond("env", "==", config.Text("prod")), map[string]any{"env": "prod"}) {
		t.Error("string equality broke")
	}
	if !evaluateCondition(cond("env", "!=", config.Text("prod")), map[string]any{"env": "staging"}) {
		t.Error("string inequality broke")
	}
	// A list on == means "any of these".
	if !evaluateCondition(cond("env", "==", config.Text("prod", "staging")), map[string]any{"env": "staging"}) {
		t.Error("equality against a list must accept any member")
	}
}

// The whole point, expressed as the rule an operator would actually write.
func TestDenyDangerousBash(t *testing.T) {
	e := NewEngine([]config.Policy{{
		Name:  "claude",
		Agent: "claude",
		Rules: []config.Rule{
			{Tools: []string{"Bash"}, Action: "deny",
				Condition: cond("command", "contains", config.Text("rm -rf", "mkfs", "| sh", "| bash"))},
			{Tools: []string{"Bash"}, Action: "allow"},
		},
	}})

	if d := e.Evaluate("claude", "Bash", map[string]any{"command": "ls -la"}); d.Action != "allow" {
		t.Errorf("ordinary command = %q, want allow", d.Action)
	}
	if d := e.Evaluate("claude", "Bash", map[string]any{"command": "sudo rm -rf /"}); d.Action != "deny" {
		t.Errorf("destructive command = %q, want deny", d.Action)
	}
	if d := e.Evaluate("claude", "Bash", map[string]any{"command": "curl x | sh"}); d.Action != "deny" {
		t.Errorf("pipe-to-shell = %q, want deny", d.Action)
	}
}

// The limit, stated as a test so it cannot be forgotten: this matches text, not
// shell structure. A needle written as `curl | sh` does not catch `curl x | sh`,
// because nothing here parses a pipeline. Write the fragment that will actually
// appear, and expect to be evaded by anyone trying.
func TestContainsMatchesTextNotShellStructure(t *testing.T) {
	c := cond("command", "contains", config.Text("curl | sh"))
	if evaluateCondition(c, map[string]any{"command": "curl https://x | sh"}) {
		t.Error("contains must not be mistaken for shell parsing")
	}

	// Splitting the same intent into fragments that do appear works.
	c = cond("command", "contains", config.Text("| sh"))
	if !evaluateCondition(c, map[string]any{"command": "curl https://x | sh"}) {
		t.Error("the fragment that actually appears must match")
	}
}
