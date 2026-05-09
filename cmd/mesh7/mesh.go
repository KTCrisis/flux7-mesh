package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	meshexec "github.com/KTCrisis/flux7-mesh/exec"
	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/mcp"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/ratelimit"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/storage"
	"github.com/KTCrisis/flux7-mesh/trace"
)

type meshState struct {
	cfg        *config.Config
	reg        *registry.Registry
	pol        *policy.Engine
	traces     *trace.Store
	approvals  *approval.Store
	grants     *grant.Store
	handler    *proxy.Handler
	mcpManager *mcp.Manager
	mcpHTTP    *mcp.HTTPHandler
	closers    []func()
}

func (m *meshState) Close() {
	for i := len(m.closers) - 1; i >= 0; i-- {
		m.closers[i]()
	}
}

func initMesh(configPath string, portOverride int, specURL, backendURL string) (*meshState, error) {
	m := &meshState{}

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", configPath, err)
	}
	m.cfg = cfg
	slog.Info("config loaded", "policies", len(cfg.Policies))

	if portOverride > 0 {
		cfg.Port = portOverride
	}

	// Registry
	m.reg = registry.New()
	if specURL != "" {
		slog.Info("loading OpenAPI spec", "url", specURL)
		if err := m.reg.LoadOpenAPI(specURL, backendURL, nil); err != nil {
			return nil, fmt.Errorf("load OpenAPI spec: %w", err)
		}
		slog.Info("registry loaded", "tools", len(m.reg.All()))
	}
	for _, oa := range cfg.OpenAPIs {
		if oa.URL != "" {
			slog.Info("loading OpenAPI spec from config", "url", oa.URL)
			if err := m.reg.LoadOpenAPI(oa.URL, oa.BackendURL, nil); err != nil {
				return nil, fmt.Errorf("load OpenAPI %s: %w", oa.URL, err)
			}
		} else {
			slog.Info("loading OpenAPI spec from file", "file", oa.File)
			if err := m.reg.LoadOpenAPIFile(oa.File, oa.BackendURL); err != nil {
				return nil, fmt.Errorf("load OpenAPI %s: %w", oa.File, err)
			}
		}
		slog.Info("OpenAPI tools loaded", "tools", len(m.reg.All()))
	}

	// Policy engine
	m.pol = policy.NewEngine(cfg.Policies)
	slog.Info("policy engine ready", "policies", len(cfg.Policies))

	// Trace store
	if cfg.TraceFile != "" {
		m.traces, err = trace.NewPersistentStore(10000, cfg.TraceFile)
		if err != nil {
			return nil, fmt.Errorf("open trace file %s: %w", cfg.TraceFile, err)
		}
		m.closers = append(m.closers, func() { m.traces.Close() })
		slog.Info("trace store ready", "file", cfg.TraceFile)
	} else {
		m.traces = trace.NewStore(10000)
		slog.Info("trace store ready", "mode", "in-memory")
	}

	// OTEL exporter
	if cfg.OTELEndpoint != "" {
		otelExp := trace.NewOTELExporter(cfg.OTELEndpoint)
		m.traces.OTEL = otelExp
		m.closers = append(m.closers, func() { otelExp.Close() })
		slog.Info("OTEL exporter ready", "endpoint", cfg.OTELEndpoint)
	}

	// Approval store
	approvalTimeout := time.Duration(cfg.Approval.TimeoutSeconds) * time.Second
	m.approvals = approval.NewStore(approvalTimeout)
	m.approvals.Notifier = approval.NewNotifier(cfg.Approval.NotifyURL)
	if cfg.Approval.NotifyURL != "" {
		slog.Info("approval notify webhook configured", "url", cfg.Approval.NotifyURL)
	}
	if cfg.Memory.URL != "" {
		m.approvals.MemoryWriter = approval.NewMemoryWriter(cfg.Memory.URL, cfg.Memory.Token)
		slog.Info("memory writer configured — decisions will be persisted", "url", cfg.Memory.URL)
		if cfg.Supervisor.IsAutoApproveEnabled() {
			m.approvals.MemoryReader = approval.NewMemoryReader(cfg.Memory.URL, cfg.Memory.Token, cfg.Supervisor.GetMinApprovals())
			slog.Info("memory reader configured — auto-approve from past decisions",
				"url", cfg.Memory.URL, "min_approvals", cfg.Supervisor.GetMinApprovals())
		}
	}
	slog.Info("approval store ready", "timeout", approvalTimeout)
	if cfg.Supervisor.IsEnabled() {
		slog.Info("supervisor mode enabled — approval.resolve hidden from agents")
	}

	// Grant store
	m.grants = grant.NewStore()

	// Durable storage (SQLite)
	if cfg.StoragePath != "" {
		stateDB, err := storage.Open(cfg.StoragePath)
		if err != nil {
			return nil, fmt.Errorf("open state database %s: %w", cfg.StoragePath, err)
		}
		m.closers = append(m.closers, func() { stateDB.Close() })
		m.approvals.SetDB(stateDB)
		m.grants.SetDB(stateDB)
		if n, err := m.approvals.LoadAll(); err != nil {
			slog.Error("failed to load approvals", "error", err)
		} else if n > 0 {
			slog.Info("approvals restored from disk", "count", n)
		}
		if n, err := m.grants.LoadAll(); err != nil {
			slog.Error("failed to load grants", "error", err)
		} else if n > 0 {
			slog.Info("grants restored from disk", "count", n)
		}
		slog.Info("durable state ready", "path", cfg.StoragePath)
	}

	// Rate limiter
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

	// CLI tools
	if len(cfg.CLITools) > 0 {
		m.reg.LoadCLI(cfg.CLITools)
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

	// Handler
	m.handler = proxy.NewHandler(m.reg, m.pol, m.traces)
	m.handler.Approvals = m.approvals
	m.handler.RateLimiter = limiter
	m.handler.Grants = m.grants
	m.handler.SupervisorCfg = cfg.Supervisor
	m.handler.Version = version
	m.handler.Commit = commit
	m.handler.BuildDate = date
	if len(cfg.CLITools) > 0 {
		m.handler.CLIRunner = &meshexec.Runner{MaxOutputBytes: 1 << 20}
	}

	// Upstream MCP servers (parallel)
	if len(cfg.MCPServers) > 0 {
		m.mcpManager = mcp.NewManager()
		m.handler.MCPForwarder = m.mcpManager

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
				m.mcpManager.Add(client)
				defs := convertMCPTools(client.Tools())
				m.reg.LoadMCP(sc.Name, defs)
				for _, d := range defs {
					slog.Info("  MCP tool registered", "name", sc.Name+"."+d.Name, "server", sc.Name)
				}
			}(serverCfg)
		}
		wg.Wait()
		cancel()
		slog.Info("MCP upstream servers connected", "count", len(m.mcpManager.All()))
	}

	// MCP Streamable HTTP handler
	m.mcpHTTP = mcp.NewHTTPHandler(m.reg, m.pol, m.traces, m.approvals, m.handler, m.mcpManager, cfg.Supervisor.IsEnabled(), cfg.Supervisor.SupervisorAgents)
	m.handler.MCPHTTPHandler = m.mcpHTTP

	// Policy hot-reload watcher
	policyWatcher, err := policy.NewWatcher(configPath, func(policies []config.Policy) {
		m.pol.Reload(policies)
		newLimits := make(map[string]ratelimit.Limit)
		for _, p := range policies {
			if p.RateLimit != nil {
				newLimits[p.Name] = ratelimit.Limit{
					MaxPerMinute: p.RateLimit.MaxPerMinute,
					MaxTotal:     p.RateLimit.MaxTotal,
				}
			}
		}
		limiter.ReplaceLimits(newLimits)
	})
	if err != nil {
		slog.Warn("policy hot-reload disabled", "error", err)
	} else {
		m.closers = append(m.closers, func() { policyWatcher.Close() })
		slog.Info("policy hot-reload enabled", "config", configPath)
	}

	return m, nil
}

func (m *meshState) ServeHTTP() error {
	addr := fmt.Sprintf(":%d", m.cfg.Port)
	slog.Info("mesh7 daemon listening", "addr", addr, "version", version)
	slog.Info("endpoints",
		"tool_call", fmt.Sprintf("POST http://localhost%s/tool/{name}", addr),
		"list_tools", fmt.Sprintf("GET  http://localhost%s/tools", addr),
		"mcp_http", fmt.Sprintf("POST http://localhost%s/mcp", addr),
		"approvals", fmt.Sprintf("GET  http://localhost%s/approvals", addr),
		"mcp_servers", fmt.Sprintf("GET  http://localhost%s/mcp-servers", addr),
		"traces", fmt.Sprintf("GET  http://localhost%s/traces", addr),
		"otel_traces", fmt.Sprintf("GET  http://localhost%s/otel-traces", addr),
		"health", fmt.Sprintf("GET  http://localhost%s/health", addr),
		"version", fmt.Sprintf("GET  http://localhost%s/version", addr),
	)

	srv := &http.Server{Addr: addr, Handler: m.handler}
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down...")
		if m.mcpManager != nil {
			m.mcpManager.CloseAll()
		}
		srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
