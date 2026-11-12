package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/internal/opcli"
)

// runValidate serves `mesh7 validate [--config file]`: load the config and its
// policy files exactly as the daemon would, and say whether they are sound.
// Nothing is started and no running mesh is contacted. Exit code 0 when the
// config loads, 1 when it does not.
func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "config.yaml", "Path to config YAML")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() == 1 { // `mesh7 validate my.yaml` reads as naturally as --config
		*configPath = fs.Arg(0)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "invalid: %v\n", err)
		return 1
	}
	rules := 0
	for _, p := range cfg.Policies {
		rules += len(p.Rules)
	}
	names := map[string]bool{}
	for _, s := range cfg.MCPServers {
		if s.Name == "" {
			fmt.Fprintln(stderr, "invalid: mcp_servers: a server has no name")
			return 1
		}
		if names[s.Name] {
			fmt.Fprintf(stderr, "invalid: mcp_servers: duplicate name %q\n", s.Name)
			return 1
		}
		names[s.Name] = true
		if _, err := newClient(s); err != nil {
			fmt.Fprintf(stderr, "invalid: mcp_servers %q: %v\n", s.Name, err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "ok: %s\n  %d upstream server(s), %d CLI tool(s), %d policy(ies) with %d rule(s)\n",
		*configPath, len(cfg.MCPServers), len(cfg.CLITools), len(cfg.Policies), rules)
	return 0
}

// printHelp is `mesh7 help`: one page for the whole binary.
func printHelp(w io.Writer) {
	fmt.Fprint(w, `mesh7: a proxy between agents and their tools. It relays, traces, escalates.

run the mesh:
  mesh7 --config <file>          Run in the foreground (HTTP on the config's port)
  mesh7 serve                    Run as the daemon (see mesh7 serve --help)
  mesh7 --config <file> --mcp    Speak MCP on stdio for one agent (--mcp-agent);
                                 proxies to the daemon when one runs

prepare and check:
  mesh7 validate [--config <file>]   Load config and policies as the daemon
                                     would; exit 1 with the reason if unsound
  mesh7 discover                     Inspect upstreams, draft a starter policy
  mesh7 trace                        Read and verify the trace

operate a running mesh (MESH_URL, default http://localhost:9090):
  mesh7 pending | show <id> | approve <id> | deny <id> | watch
  mesh7 halt | halts | resume <halt-id>
  mesh7 upstream list | reload | restart <name>
  mesh7 pin list | accept <tool>... | accept --server <name>

Each operator command explains itself with: mesh7 <command> with no argument,
or the full text below.

`)
	opcli.Usage("mesh7", w)
}
