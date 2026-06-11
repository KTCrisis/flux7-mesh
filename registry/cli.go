package registry

import (
	"fmt"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
)

// LoadCLI registers CLI tools into the registry.
// Each declared command gets its own tool entry (e.g. "terraform.plan").
// Non-strict tools also get a catch-all entry (e.g. "terraform.__dispatch")
// for dynamic subcommand dispatch.
func (r *Registry) LoadCLI(tools []config.CLIToolConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, cfg := range tools {
		defaultAction := cfg.DefaultAction
		if defaultAction == "" {
			defaultAction = "deny"
		}

		// Bare binary (no subcommands): single <name>.run tool, no catch-all
		if cfg.Bare != nil {
			name := cfg.Name + ".run"
			r.set(name, &Tool{
				Name:        name,
				Description: fmt.Sprintf("Run %s", cfg.Bin),
				Source:      "cli",
				Params:      cliCommandParams(*cfg.Bare),
				CLIMeta: &CLIToolMeta{
					Bin:           cfg.Bin,
					Command:       "",
					AllowedArgs:   cfg.Bare.AllowedArgs,
					Timeout:       parseDuration(cfg.Bare.Timeout, 30*time.Second),
					WorkingDir:    cfg.WorkingDir,
					Env:           cfg.Env,
					Strict:        true,
					DefaultAction: defaultAction,
				},
			})
			continue
		}

		// Register each declared command
		for cmdName, cmdCfg := range cfg.Commands {
			name := cfg.Name + "." + cmdName
			r.set(name, &Tool{
				Name:        name,
				Description: fmt.Sprintf("Run %s %s", cfg.Bin, cmdName),
				Source:      "cli",
				Params:      cliCommandParams(cmdCfg),
				CLIMeta: &CLIToolMeta{
					Bin:           cfg.Bin,
					Command:       cmdName,
					AllowedArgs:   cmdCfg.AllowedArgs,
					Timeout:       parseDuration(cmdCfg.Timeout, 30*time.Second),
					WorkingDir:    cfg.WorkingDir,
					Env:           cfg.Env,
					Strict:        cfg.Strict,
					DefaultAction: defaultAction,
				},
			})
		}

		// Non-strict: register catch-all for undeclared commands
		if !cfg.Strict {
			name := cfg.Name + ".__dispatch"
			r.set(name, &Tool{
				Name:        name,
				Description: fmt.Sprintf("Run %s <command> (any subcommand)", cfg.Bin),
				Source:      "cli",
				Params: []Param{
					{Name: "command", In: "body", Type: "string", Required: true},
					{Name: "args", In: "body", Type: "array"},
					{Name: "flags", In: "body", Type: "object"},
					{Name: "stdin", In: "body", Type: "string"},
				},
				CLIMeta: &CLIToolMeta{
					Bin:           cfg.Bin,
					WorkingDir:    cfg.WorkingDir,
					Env:           cfg.Env,
					DefaultAction: defaultAction,
					IsCatchAll:    true,
				},
			})
		}
	}
}

// ResolveCLI looks up a CLI tool by name, falling back to the catch-all
// dispatcher if no exact match is found. Returns nil if not found.
func (r *Registry) ResolveCLI(name string) *Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Exact match first
	if t := r.tools[name]; t != nil && t.Source == "cli" {
		return t
	}

	// Fallback: extract tool prefix and try __dispatch
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			dispatch := name[:i] + ".__dispatch"
			if t := r.tools[dispatch]; t != nil {
				return t
			}
			break
		}
	}
	return nil
}

// cliCommandParams builds the Param list for a declared CLI command.
func cliCommandParams(cmd config.CLICommandConfig) []Param {
	params := []Param{
		{Name: "args", In: "body", Type: "array"},
		{Name: "flags", In: "body", Type: "object"},
		{Name: "stdin", In: "body", Type: "string"},
	}
	return params
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
