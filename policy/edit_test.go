package policy

import (
	"strings"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"gopkg.in/yaml.v3"
)

const sample = `name: claude
agent: "claude"

# -- Mail --

rules:
  # Reads are fine.
  - tools: ["gmail.list"]
    action: allow

  # Send email — allowed (mobile use)
  - tools: ["gmail-*.send"]
    action: allow   # keep this comment

  - tools: ["fs.*"]
    action: human_approval
    condition:
      field: path
      operator: starts_with
      value: ["/work"]

  - tools: [
      "a.b",
      "c.d"
    ]
    action: deny
`

func parse(t *testing.T, src []byte) config.Policy {
	t.Helper()
	var p config.Policy
	if err := yaml.Unmarshal(src, &p); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, src)
	}
	return p
}

func TestInsertRuleBeforeKeepsHeadComment(t *testing.T) {
	out, err := InsertRule([]byte(sample), "gmail-marc.send", "deny", 1, "2026-09-28")
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, out)
	if len(p.Rules) != 5 || p.Rules[1].Tools[0] != "gmail-marc.send" || p.Rules[1].Action != "deny" {
		t.Fatalf("rules = %+v", p.Rules)
	}
	// The new rule goes above the comment that heads the displaced rule.
	s := string(out)
	if !strings.Contains(s, "  - tools: [\"gmail-marc.send\"]\n    action: deny\n\n  # Send email") &&
		!strings.Contains(s, "    action: deny\n  # Send email") {
		t.Errorf("comment detached from its rule:\n%s", s)
	}
	if !strings.Contains(s, "  "+ConsoleMarker+", 2026-09-28\n") {
		t.Errorf("marker missing or misindented:\n%s", s)
	}
	// Everything else is untouched.
	if strings.Replace(s, "  "+ConsoleMarker+", 2026-09-28\n  - tools: [\"gmail-marc.send\"]\n    action: deny\n", "", 1) != sample {
		t.Errorf("edit changed more than the inserted block:\n%s", s)
	}
}

func TestInsertRuleAppendStepsOverClosingBracket(t *testing.T) {
	out, err := InsertRule([]byte(sample), "x.y", "allow", -1, "n")
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, out)
	last := p.Rules[len(p.Rules)-1]
	if last.Tools[0] != "x.y" || last.Action != "allow" {
		t.Fatalf("last rule = %+v\n%s", last, out)
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Error("trailing newline lost")
	}
}

func TestSetActionKeepsComment(t *testing.T) {
	out, err := SetAction([]byte(sample), 1, "human_approval")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    action: human_approval   # keep this comment\n") {
		t.Errorf("comment lost:\n%s", out)
	}
	if parse(t, out).Rules[1].Action != "human_approval" {
		t.Error("action not changed")
	}
}

func TestRemoveConsoleRuleOnly(t *testing.T) {
	// A hand-written exact rule is never removed.
	if _, removed, _ := RemoveConsoleRule([]byte(sample), "gmail.list"); removed {
		t.Fatal("removed a hand-written rule")
	}
	withRule, _ := InsertRule([]byte(sample), "gmail.list", "deny", 0, "n")
	out, removed, err := RemoveConsoleRule(withRule, "gmail.list")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if string(out) != sample {
		t.Errorf("insert then remove is not the identity:\n%s", out)
	}
}

func TestExactRule(t *testing.T) {
	for tool, want := range map[string]int{"gmail.list": 0, "gmail-*.send": 1, "fs.*": -1, "a.b": -1, "nope": -1} {
		got, err := ExactRule([]byte(sample), tool)
		if err != nil || got != want {
			t.Errorf("ExactRule(%q) = %d, %v; want %d", tool, got, err, want)
		}
	}
}
