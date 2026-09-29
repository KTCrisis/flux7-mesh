package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/mcp"
	"github.com/KTCrisis/flux7-mesh/registry"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "discover":
			runDiscover(os.Args[2:])
			return
		case "serve":
			runServe(os.Args[2:])
			return
		case "trace":
			runTrace(os.Args[2:])
			return
		}
	}

	showVersion := flag.Bool("version", false, "Print version and exit")
	configPath := flag.String("config", "config.yaml", "Path to config YAML")
	specURL := flag.String("openapi", "", "OpenAPI spec URL to load")
	backendURL := flag.String("backend", "", "Backend base URL (overrides spec)")
	port := flag.Int("port", 0, "Port override (default from config or 9090)")
	mcpMode := flag.Bool("mcp", false, "Run as MCP server (stdio JSON-RPC instead of HTTP)")
	mcpAgent := flag.String("mcp-agent", "claude", "Agent ID for MCP mode policy evaluation")
	mcpSessionID := flag.String("mcp-session-id", "", "Session ID for MCP traces (auto-generated if empty)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("mesh7 %s (%s) built %s\n", version, commit, date)
		return
	}

	logDest := os.Stdout
	if *mcpMode {
		logDest = os.Stderr
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logDest, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// MCP auto-proxy: detect running daemon before heavy init
	if *mcpMode {
		if cfg, err := config.Load(*configPath); err == nil {
			p := cfg.Port
			if *port > 0 {
				p = *port
			}
			if running, theirs := daemonOnPort(p); running {
				ok, warn, err := proxyDecision(configID(*configPath), theirs, p)
				if err != nil {
					slog.Error("mesh7 --mcp", "error", err)
					fmt.Fprintln(os.Stderr, "mesh7:", err)
					os.Exit(1)
				}
				if warn != "" {
					slog.Warn(warn, "port", p)
				}
				if ok {
					if err := runStdioProxy(p, *mcpAgent); err != nil {
						slog.Error("proxy failed", "error", err)
						os.Exit(1)
					}
					return
				}
			}
		}
	}

	m, err := initMesh(*configPath, *port, *specURL, *backendURL)
	if err != nil {
		slog.Error("init failed", "error", err)
		os.Exit(1)
	}
	defer m.Close()

	if *mcpMode {
		addr := fmt.Sprintf(":%d", m.cfg.Port)
		go func() {
			slog.Info("approval API available", "url", fmt.Sprintf("http://localhost%s/approvals", addr))
			if err := http.ListenAndServe(addr, m.handler); err != nil {
				slog.Error("approval HTTP server failed", "error", err)
			}
		}()

		server := &mcp.Server{
			Registry:         m.reg,
			Policy:           m.pol,
			Traces:           m.traces,
			Approvals:        m.approvals,
			Handler:          m.handler,
			MCPManager:       m.mcpManager,
			AgentID:          *mcpAgent,
			SessionID:        *mcpSessionID,
			SupervisorMode:   m.cfg.Supervisor.IsEnabled(),
			SupervisorAgents: m.cfg.Supervisor.SupervisorAgents,
			ApprovalChannel:  m.cfg.Approval.Channel,
			ApprovalWait:     time.Duration(m.cfg.Approval.WaitSeconds * float64(time.Second)),
			HideDenied:       m.cfg.HideDeniedTools,
		}
		if err := server.Run(); err != nil {
			slog.Error("MCP server failed", "error", err)
			os.Exit(1)
		}
	} else {
		if err := m.ServeHTTP(); err != nil {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}

// convertMCPTools bridges mcp.MCPTool → registry.MCPToolDef.
func convertMCPTools(tools []mcp.MCPTool) []registry.MCPToolDef {
	defs := make([]registry.MCPToolDef, 0, len(tools))
	for _, t := range tools {
		props := make(map[string]registry.MCPPropDef, len(t.InputSchema.Properties))
		for name, p := range t.InputSchema.Properties {
			props[name] = registry.MCPPropDef{
				Type:      p.Type,
				RawSchema: p.Raw,
			}
		}
		def := registry.NewMCPToolDef(t.Name, t.Description, props, t.InputSchema.Required)
		if a := t.Annotations; a != nil {
			def.Annotations = &registry.Annotations{
				ReadOnly:    a.ReadOnlyHint,
				Destructive: a.DestructiveHint,
				Idempotent:  a.IdempotentHint,
				OpenWorld:   a.OpenWorldHint,
			}
		}
		defs = append(defs, def)
	}
	return defs
}
