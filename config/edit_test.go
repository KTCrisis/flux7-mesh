package config

import (
	"strings"
	"testing"
)

const sample = `# mesh config
approval:
  channel: queue   # daemon, no TTY
  wait_seconds: 3  # the supervisor's window

memory:
  url: http://localhost:9070

supervisor:
  enabled: false
`

func TestSetScalarsKeepsCommentsAndOrder(t *testing.T) {
	out, err := SetScalars([]byte(sample), []Scalar{
		{"approval", "wait_seconds", "2"},
		{"approval", "timeout_seconds", "600"}, // missing key: end of section
		{"supervisor", "min_approvals", "5"},   // missing key in another section
		{"supervisor", "auto_approve_writes", "false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"# mesh config",
		"  channel: queue   # daemon, no TTY",
		"  wait_seconds: 2 # the supervisor's window",
		"  timeout_seconds: 600\n\nmemory:",
		"  enabled: false\n  min_approvals: 5\n  auto_approve_writes: false",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestSetScalarsAppendsAMissingSection(t *testing.T) {
	out, err := SetScalars([]byte("mesh:\n  port: 9090\n"), []Scalar{{"approval", "wait_seconds", "3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "\n\napproval:\n  wait_seconds: 3\n") {
		t.Fatalf("section not appended:\n%s", out)
	}
}

func TestSetScalarsRefusesAMultilineValue(t *testing.T) {
	src := "approval:\n  channel:\n    - queue\n"
	if _, err := SetScalars([]byte(src), []Scalar{{"approval", "channel", "tty"}}); err == nil {
		t.Fatal("a non-scalar value must not be overwritten")
	}
}
