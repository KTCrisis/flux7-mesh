package exec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/registry"
)

// Result holds the output of a CLI command execution.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Runner executes CLI commands securely.
type Runner struct {
	MaxOutputBytes int // default 1MB (1<<20)
}

// Run executes a CLI command with security enforcement.
// It uses exec.Command directly (never sh -c) and enforces:
// - argument validation (allowlist + metacharacter rejection)
// - timeout via context
// - output size cap
// - env isolation
// - working directory sandboxing
func (r *Runner) Run(ctx context.Context, meta *registry.CLIToolMeta, command string, args []string) (*Result, error) {
	if err := ValidateArgs(meta.AllowedArgs, args); err != nil {
		return nil, fmt.Errorf("argument validation failed: %w", err)
	}

	// Build full argument list: [subcommand, ...args]
	fullArgs := make([]string, 0, 1+len(args))
	if command != "" {
		fullArgs = append(fullArgs, command)
	}
	fullArgs = append(fullArgs, args...)

	// Apply timeout
	timeout := meta.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := osexec.CommandContext(ctx, meta.Bin, fullArgs...)
	cmd.Env = buildEnv(meta.Env)
	if meta.WorkingDir != "" {
		cmd.Dir = meta.WorkingDir
	}

	// Capture stdout/stderr with size limit
	maxBytes := r.maxOutput()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, max: maxBytes}
	cmd.Stderr = &limitWriter{w: &stderr, max: maxBytes}

	err := cmd.Run()

	result := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("command timed out after %s", timeout)
	}

	if err != nil {
		// Non-zero exit is not an error — it's a valid result
		if _, ok := err.(*osexec.ExitError); ok {
			return result, nil
		}
		return result, fmt.Errorf("exec error: %w", err)
	}

	return result, nil
}

func (r *Runner) maxOutput() int {
	if r.MaxOutputBytes > 0 {
		return r.MaxOutputBytes
	}
	return 1 << 20 // 1MB
}

// dangerousMetachars are shell metacharacters that must never appear in arguments.
var dangerousMetachars = []string{";", "&&", "||", "|", "`", "$(", "${", "\n", "\r"}

// ValidateArgs checks arguments against an allowlist and rejects shell metacharacters.
// If allowed is nil, only metacharacter validation is performed.
func ValidateArgs(allowed []string, args []string) error {
	for _, arg := range args {
		if err := checkMetachars(arg); err != nil {
			return err
		}
	}

	if allowed == nil {
		return nil
	}

	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			// Positional value — allowed (metacharacters already checked)
			continue
		}
		if !matchesAllowed(allowed, arg) {
			return fmt.Errorf("flag not allowed: %s (allowed: %v)", arg, allowed)
		}
	}
	return nil
}

func checkMetachars(arg string) error {
	for _, mc := range dangerousMetachars {
		if strings.Contains(arg, mc) {
			return fmt.Errorf("dangerous character in argument: %q", mc)
		}
	}
	return nil
}

// matchesAllowed checks if a flag matches any entry in the allowed list.
// Handles both short (-n) and long (--namespace) flags, including -n=value and --namespace=value.
func matchesAllowed(allowed []string, flag string) bool {
	// Strip value from --flag=value or -f=value
	name := flag
	if idx := strings.Index(flag, "="); idx != -1 {
		name = flag[:idx]
	}
	for _, a := range allowed {
		if name == a {
			return true
		}
	}
	return false
}

// buildEnv creates an isolated environment with only essential vars + custom overrides.
func buildEnv(custom map[string]string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=en_US.UTF-8",
	}
	for k, v := range custom {
		env = append(env, k+"="+v)
	}
	return env
}

// ExtractCommand parses CLI call params into a command and argument list.
// Supports two formats:
//   - "args": ["--target", "foo"]
//   - "flags": {"target": "foo", "namespace": "prod"} → ["-target", "foo", "-namespace", "prod"]
//
// Both can be combined; flags are appended after args.
func ExtractCommand(params map[string]any, meta *registry.CLIToolMeta) (command string, args []string) {
	command = meta.Command

	// For catch-all dispatch, command comes from params
	if meta.IsCatchAll {
		if cmd, ok := params["command"].(string); ok {
			command = cmd
		}
	}

	// Extract positional args
	if rawArgs, ok := params["args"].([]any); ok {
		for _, a := range rawArgs {
			args = append(args, fmt.Sprintf("%v", a))
		}
	}

	// Extract named flags → convert to CLI args
	if flags, ok := params["flags"].(map[string]any); ok {
		for k, v := range flags {
			prefix := "-"
			if len(k) > 1 {
				prefix = "--"
			}
			args = append(args, prefix+k, fmt.Sprintf("%v", v))
		}
	}

	return command, args
}

// limitWriter caps bytes written to max. Extra bytes are silently discarded.
type limitWriter struct {
	w       io.Writer
	max     int
	written int
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	remaining := lw.max - lw.written
	if remaining <= 0 {
		return len(p), nil // discard silently
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := lw.w.Write(p)
	lw.written += n
	return n, err
}
