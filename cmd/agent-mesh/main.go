package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/KTCrisis/agent-mesh/approval"
	"github.com/KTCrisis/agent-mesh/config"
	meshexec "github.com/KTCrisis/agent-mesh/exec"
	"github.com/KTCrisis/agent-mesh/grant"
	"github.com/KTCrisis/agent-mesh/mcp"
	"github.com/KTCrisis/agent-mesh/policy"
	"github.com/KTCrisis/agent-mesh/proxy"
	"github.com/KTCrisis/agent-mesh/ratelimit"
	"github.com/KTCrisis/agent-mesh/registry"
	"github.com/KTCrisis/agent-mesh/trace"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Subcommand: discover
	if len(os.Args) > 1 && os.Args[1] == "discover" {
		runDiscover(os.Args[2:])
		return
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
		fmt.Printf("agent-mesh %s (%s) built %s\n", version, commit, date)
		return
	}

	// Setup structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// 1. Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "policies", len(cfg.Policies))

	if *port > 0 {
		cfg.Port = *port
	}

	// 2. Build registry
	reg := registry.New()

	if *specURL != "" {
		slog.Info("loading OpenAPI spec", "url", *specURL)
		if err := reg.LoadOpenAPI(*specURL, *backendURL, nil); err != nil {
			slog.Error("failed to load OpenAPI spec", "error", err)
			os.Exit(1)
		}
		slog.Info("registry loaded", "tools", len(reg.All()))
		for _, t := range reg.All() {
			slog.Info("  tool registered", "name", t.Name, "method", t.Method, "path", t.Path)
		}
	} else {
		slog.Info("no OpenAPI spec provided — use --openapi to load REST tools")
	}

	// 2b. Load OpenAPI specs from config
	for _, oa := range cfg.OpenAPIs {
		if oa.URL != "" {
			slog.Info("loading OpenAPI spec from config", "url", oa.URL)
			if err := reg.LoadOpenAPI(oa.URL, oa.BackendURL, nil); err != nil {
				slog.Error("failed to load OpenAPI spec", "url", oa.URL, "error", err)
				os.Exit(1)
			}
		} else {
			slog.Info("loading OpenAPI spec from file", "file", oa.File)
			if err := reg.LoadOpenAPIFile(oa.File, oa.BackendURL); err != nil {
				slog.Error("failed to load OpenAPI spec", "file", oa.File, "error", err)
				os.Exit(1)
			}
		}
		slog.Info("OpenAPI tools loaded", "tools", len(reg.All()))
	}

	// 3. Build policy engine
	pol := policy.NewEngine(cfg.Policies)
	slog.Info("policy engine ready", "policies", len(cfg.Policies))

	// 4. Build trace store
	var traces *trace.Store
	if cfg.TraceFile != "" {
		var err error
		traces, err = trace.NewPersistentStore(10000, cfg.TraceFile)
		if err != nil {
			slog.Error("failed to open trace file", "path", cfg.TraceFile, "error", err)
			os.Exit(1)
		}
		defer traces.Close()
		slog.Info("trace store ready", "file", cfg.TraceFile)
	} else {
		traces = trace.NewStore(10000)
		slog.Info("trace store ready", "mode", "in-memory")
	}

	// 4b. OTEL exporter
	if cfg.OTELEndpoint != "" {
		otelExp := trace.NewOTELExporter(cfg.OTELEndpoint)
		traces.OTEL = otelExp
		defer otelExp.Close()
		slog.Info("OTEL exporter ready", "endpoint", cfg.OTELEndpoint)
	}

	// 5. Build approval store + notifier
	approvalTimeout := time.Duration(cfg.Approval.TimeoutSeconds) * time.Second
	approvals := approval.NewStore(approvalTimeout)
	approvals.Notifier = approval.NewNotifier(cfg.Approval.NotifyURL)
	if cfg.Approval.NotifyURL != "" {
		slog.Info("approval notify webhook configured", "url", cfg.Approval.NotifyURL)
	}
	if cfg.Memory.URL != "" {
		approvals.MemoryWriter = approval.NewMemoryWriter(cfg.Memory.URL, cfg.Memory.Token)
		slog.Info("memory writer configured — decisions will be persisted", "url", cfg.Memory.URL)
	}
	slog.Info("approval store ready", "timeout", approvalTimeout)
	if cfg.Supervisor.IsEnabled() {
		slog.Info("supervisor mode enabled — approval.resolve hidden from agents")
	}

	// 6. Build rate limiter
	limiter := ratelimit.New()
	for _, p := range cfg.Policies {
		if p.RateLimit != nil {
			limiter.SetLimit(p.Name, ratelimit.Limit{
				MaxPerMinute: p.RateLimit.MaxPerMinute,
				MaxTotal:     p.RateLimit.MaxTotal,
			})
			slog.Info("rate limit configured", "policy", p.Name,
				"max_per_minute", p.RateLimit.MaxPerMinute,
				"max_total", p.RateLimit.MaxTotal)
		}
	}

	// 7. Build grant store
	grants := grant.NewStore()
	slog.Info("grant store ready")

	// 8. Register CLI tools
	if len(cfg.CLITools) > 0 {
		reg.LoadCLI(cfg.CLITools)
		for _, ct := range cfg.CLITools {
			mode := "simple"
			if ct.Strict {
				mode = "strict"
			} else if len(ct.Commands) > 0 {
				mode = "fine-tuned"
			}
			slog.Info("CLI tool registered", "name", ct.Name, "bin", ct.Bin, "mode", mode, "commands", len(ct.Commands))
		}
	}

	// 9. Build handler
	handler := proxy.NewHandler(reg, pol, traces)
	handler.Approvals = approvals
	handler.RateLimiter = limiter
	handler.Grants = grants
	handler.SupervisorCfg = cfg.Supervisor
	handler.Version = version
	handler.Commit = commit
	handler.BuildDate = date
	if len(cfg.CLITools) > 0 {
		handler.CLIRunner = &meshexec.Runner{MaxOutputBytes: 1 << 20}
	}

	// 7. Connect upstream MCP servers (parallel)
	var mcpManager *mcp.Manager
	if len(cfg.MCPServers) > 0 {
		mcpManager = mcp.NewManager()
		handler.MCPForwarder = mcpManager

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var wg sync.WaitGroup
		for _, serverCfg := range cfg.MCPServers {
			wg.Add(1)
			go func(sc config.MCPServerConfig) {
				defer wg.Done()
				var client *mcp.MCPClient
				switch sc.Transport {
				case "stdio":
					client = mcp.NewStdioClient(sc.Name, sc.Command, sc.Args, sc.Env)
				case "sse":
					client = mcp.NewSSEClient(sc.Name, sc.URL, sc.Headers)
				default:
					slog.Error("unsupported MCP transport", "name", sc.Name, "transport", sc.Transport)
					return
				}

				if err := client.Connect(ctx); err != nil {
					slog.Error("failed to connect MCP server", "name", sc.Name, "error", err)
					return
				}
				mcpManager.Add(client)

				defs := convertMCPTools(client.Tools())
				reg.LoadMCP(sc.Name, defs)
				for _, d := range defs {
					slog.Info("  MCP tool registered", "name", sc.Name+"."+d.Name, "server", sc.Name)
				}
			}(serverCfg)
		}
		wg.Wait()
		cancel()

		slog.Info("MCP upstream servers connected", "count", len(mcpManager.All()))
	}

	// 8. MCP mode or HTTP mode
	if *mcpMode {
		// MCP: JSON-RPC over stdio — logs go to stderr to keep stdout clean
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

		// Start HTTP server in background for approval API
		addr := fmt.Sprintf(":%d", cfg.Port)
		go func() {
			slog.Info("approval API available", "url", fmt.Sprintf("http://localhost%s/approvals", addr))
			if err := http.ListenAndServe(addr, handler); err != nil {
				slog.Error("approval HTTP server failed", "error", err)
			}
		}()

		server := &mcp.Server{
			Registry:       reg,
			Policy:         pol,
			Traces:         traces,
			Approvals:      approvals,
			Handler:        handler,
			MCPManager:     mcpManager,
			AgentID:        *mcpAgent,
			SessionID:      *mcpSessionID,
			SupervisorMode: cfg.Supervisor.IsEnabled(),
		}
		if err := server.Run(); err != nil {
			slog.Error("MCP server failed", "error", err)
			os.Exit(1)
		}
	} else {
		addr := fmt.Sprintf(":%d", cfg.Port)
		slog.Info("agent-mesh sidecar starting", "addr", addr)
		slog.Info("endpoints",
			"tool_call", fmt.Sprintf("POST http://localhost%s/tool/{name}", addr),
			"list_tools", fmt.Sprintf("GET  http://localhost%s/tools", addr),
			"approvals", fmt.Sprintf("GET  http://localhost%s/approvals", addr),
			"mcp_servers", fmt.Sprintf("GET  http://localhost%s/mcp-servers", addr),
			"traces", fmt.Sprintf("GET  http://localhost%s/traces", addr),
			"otel_traces", fmt.Sprintf("GET  http://localhost%s/otel-traces", addr),
			"health", fmt.Sprintf("GET  http://localhost%s/health", addr),
			"version", fmt.Sprintf("GET  http://localhost%s/version", addr),
		)

		// Graceful shutdown: close MCP clients on SIGINT/SIGTERM
		srv := &http.Server{Addr: addr, Handler: handler}
		go func() {
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			<-sigCh
			slog.Info("shutting down...")
			if mcpManager != nil {
				mcpManager.CloseAll()
			}
			srv.Shutdown(context.Background())
		}()

		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}

// convertMCPTools bridges mcp.MCPTool → registry.MCPToolDef.
//
// The raw JSON schema of each property is passed through verbatim via
// MCPPropDef.RawSchema so that constructs like "anyOf", "items" and "enum"
// survive the round-trip from the upstream MCP server to the agent-mesh
// tools/list re-export. Without this, optional parameters using anyOf
// would be silently downgraded to {type: "string"} on export.
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
		defs = append(defs, registry.NewMCPToolDef(t.Name, t.Description, props, t.InputSchema.Required))
	}
	return defs
}
