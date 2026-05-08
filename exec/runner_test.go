package exec

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/registry"
)

func TestValidateArgs_NoAllowlist(t *testing.T) {
	// Without allowlist, any non-dangerous args are fine
	if err := ValidateArgs(nil, []string{"--flag", "value", "-n", "prod"}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateArgs_Allowlist(t *testing.T) {
	allowed := []string{"-n", "--namespace", "-o", "--output"}

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"allowed short", []string{"-n", "prod"}, false},
		{"allowed long", []string{"--namespace", "prod"}, false},
		{"allowed with equals", []string{"--namespace=prod"}, false},
		{"multiple allowed", []string{"-n", "prod", "-o", "json"}, false},
		{"disallowed flag", []string{"--kubeconfig", "/etc/shadow"}, true},
		{"positional only", []string{"pods", "my-pod"}, false},
		{"mixed allowed and positional", []string{"-n", "prod", "pods"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArgs(allowed, tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestValidateArgs_ShellInjection(t *testing.T) {
	dangerous := []struct {
		name string
		arg  string
	}{
		{"semicolon", "foo; rm -rf /"},
		{"and-and", "foo && evil"},
		{"or-or", "foo || evil"},
		{"pipe", "foo | evil"},
		{"backtick", "`evil`"},
		{"dollar-paren", "$(evil)"},
		{"dollar-brace", "${evil}"},
		{"newline", "foo\nevil"},
		{"carriage-return", "foo\revil"},
	}

	for _, tt := range dangerous {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateArgs(nil, []string{tt.arg})
			if err == nil {
				t.Errorf("expected error for dangerous arg %q, got nil", tt.arg)
			}
		})
	}
}

func TestRunner_Echo(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin: "echo",
	}

	result, err := runner.Run(context.Background(), meta, "", []string{"hello", "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if got := strings.TrimSpace(result.Stdout); got != "hello world" {
		t.Errorf("expected stdout 'hello world', got %q", got)
	}
}

func TestRunner_NonZeroExit(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin: "false", // always exits 1
	}

	result, err := runner.Run(context.Background(), meta, "", nil)
	if err != nil {
		t.Fatalf("non-zero exit should not be an error, got %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}

func TestRunner_Timeout(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin:     "sleep",
		Timeout: 100 * time.Millisecond,
	}

	_, err := runner.Run(context.Background(), meta, "", []string{"10"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestRunner_OutputCap(t *testing.T) {
	runner := &Runner{MaxOutputBytes: 100}
	meta := &registry.CLIToolMeta{
		Bin: "yes", // infinite output
		Timeout: 500 * time.Millisecond,
	}

	// yes will be killed by timeout, but output should be capped
	result, _ := runner.Run(context.Background(), meta, "", nil)
	if len(result.Stdout) > 100 {
		t.Errorf("expected stdout capped at 100 bytes, got %d", len(result.Stdout))
	}
}

func TestRunner_Subcommand(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin:     "echo",
		Command: "plan",
	}

	// "echo plan --target foo" should print "plan --target foo"
	result, err := runner.Run(context.Background(), meta, "plan", []string{"--target", "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "plan --target foo" {
		t.Errorf("expected 'plan --target foo', got %q", got)
	}
}

func TestRunner_AllowedArgsEnforced(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin:         "echo",
		AllowedArgs: []string{"-n"},
	}

	// Allowed flag
	_, err := runner.Run(context.Background(), meta, "", []string{"-n", "hello"})
	if err != nil {
		t.Fatalf("expected allowed flag to work, got %v", err)
	}

	// Disallowed flag
	_, err = runner.Run(context.Background(), meta, "", []string{"--evil", "hello"})
	if err == nil {
		t.Fatal("expected error for disallowed flag")
	}
}

func TestRunner_EnvIsolation(t *testing.T) {
	runner := &Runner{}
	meta := &registry.CLIToolMeta{
		Bin: "env",
		Env: map[string]string{"MY_CUSTOM": "value123"},
	}

	result, err := runner.Run(context.Background(), meta, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Stdout, "MY_CUSTOM=value123") {
		t.Error("expected custom env var in output")
	}
	// Should NOT contain random env vars from parent
	if strings.Contains(result.Stdout, "SHELL=") {
		t.Error("expected env isolation — SHELL should not be inherited")
	}
}

func TestExtractCommand_CatchAll(t *testing.T) {
	meta := &registry.CLIToolMeta{Bin: "terraform", IsCatchAll: true}
	params := map[string]any{
		"command": "init",
		"args":    []any{"-backend=false"},
	}

	cmd, args := ExtractCommand(params, meta)
	if cmd != "init" {
		t.Errorf("expected command 'init', got %q", cmd)
	}
	if len(args) != 1 || args[0] != "-backend=false" {
		t.Errorf("expected args [-backend=false], got %v", args)
	}
}

func TestExtractCommand_Flags(t *testing.T) {
	meta := &registry.CLIToolMeta{Bin: "kubectl", Command: "get"}
	params := map[string]any{
		"flags": map[string]any{
			"n": "prod",
			"o": "json",
		},
	}

	cmd, args := ExtractCommand(params, meta)
	if cmd != "get" {
		t.Errorf("expected command 'get', got %q", cmd)
	}
	// Flags should produce short -n and -o (single char = single dash)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-n") || !strings.Contains(joined, "prod") {
		t.Errorf("expected -n prod in args, got %v", args)
	}
}

func TestExtractCommand_MixedArgsAndFlags(t *testing.T) {
	meta := &registry.CLIToolMeta{Bin: "kubectl", Command: "get"}
	params := map[string]any{
		"args":  []any{"pods", "my-pod"},
		"flags": map[string]any{"namespace": "prod"},
	}

	cmd, args := ExtractCommand(params, meta)
	if cmd != "get" {
		t.Errorf("expected command 'get', got %q", cmd)
	}
	// Should have positional args + flags
	if len(args) < 3 {
		t.Errorf("expected at least 3 args, got %v", args)
	}
}

func TestLimitWriter(t *testing.T) {
	var buf strings.Builder
	lw := &limitWriter{w: &buf, max: 10}

	lw.Write([]byte("12345"))
	lw.Write([]byte("67890"))
	lw.Write([]byte("EXCESS"))

	if buf.Len() != 10 {
		t.Errorf("expected 10 bytes, got %d", buf.Len())
	}
	if buf.String() != "1234567890" {
		t.Errorf("expected '1234567890', got %q", buf.String())
	}
}
