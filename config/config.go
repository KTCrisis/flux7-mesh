package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port         int               `yaml:"port"`
	StoragePath  string            `yaml:"storage_path"` // SQLite DB for durable state (approvals, grants)
	TraceFile    string            `yaml:"trace_file"`
	OTELEndpoint string            `yaml:"otel_endpoint"` // "stdout" or "http://localhost:4318" (OTLP HTTP)
	Auth         AuthConfig        `yaml:"auth"`
	TLS          TLSConfig         `yaml:"tls,omitempty"`
	Approval     ApprovalConfig    `yaml:"approval"`
	Supervisor   SupervisorConfig  `yaml:"supervisor"`
	Memory       MemoryConfig      `yaml:"memory"`
	Policies     []Policy          `yaml:"policies"`
	PolicyDir    string            `yaml:"policy_dir,omitempty"` // directory of per-agent policy files
	MCPServers   []MCPServerConfig `yaml:"mcp_servers"`
	CLITools     []CLIToolConfig   `yaml:"cli_tools"`
	OpenAPIs     []OpenAPIConfig   `yaml:"openapi,omitempty"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	JWT *JWTConfig `yaml:"jwt,omitempty"`
	// AdminToken guards the control plane (traces, grants, approvals,
	// policies, sessions, metrics). When set, those endpoints require
	// `Authorization: Bearer <token>`. When empty, the control plane is
	// restricted to loopback callers only. The data plane (tool calls,
	// /decide, /mcp, /health) is never gated by this.
	AdminToken string `yaml:"admin_token,omitempty"`
	// RequireAuthentication rejects data-plane requests with no credentials
	// (agent resolves to "anonymous") with 401 instead of letting the policy
	// engine fail closed. Off by default — tool calls are already governed by
	// policy; turning this on also stops unauthenticated registry enumeration
	// via /tools and /mcp-servers.
	RequireAuthentication bool `yaml:"require_authentication,omitempty"`
}

// TLSConfig enables in-binary TLS termination. Leave empty to serve plaintext —
// the recommended deployment terminates TLS at an ingress / reverse proxy and
// keeps mesh7 on loopback or an internal network. Set both fields to serve TLS
// directly (useful for standalone single-host deployments without an ingress).
type TLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// Enabled reports whether in-binary TLS is configured.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// JWTConfig configures JWT validation against an external IdP.
type JWTConfig struct {
	JWKSURL    string `yaml:"jwks_url"`
	Issuer     string `yaml:"issuer,omitempty"`
	Audience   string `yaml:"audience,omitempty"`
	AgentClaim string `yaml:"agent_claim,omitempty"` // default: "sub"
	// AllowLegacy re-enables the plaintext "agent:<id>" identity bypass even
	// when JWT is configured. Off by default — keeping it off means a
	// validated JWT is the only accepted identity (no spoofing past crypto).
	AllowLegacy bool `yaml:"allow_legacy,omitempty"`
}

// MemoryConfig declares an optional mem7 server for persisting decisions.
type MemoryConfig struct {
	URL   string `yaml:"url"`   // e.g. "http://localhost:9070"
	Token string `yaml:"token"` // optional bearer token
}

// OpenAPIConfig declares an OpenAPI spec to import as governed tools.
// Set either url (fetched via HTTP) or file (read from disk), not both.
type OpenAPIConfig struct {
	URL        string `yaml:"url,omitempty"`
	File       string `yaml:"file,omitempty"`
	BackendURL string `yaml:"backend_url,omitempty"`
}

// SupervisorConfig controls supervisor mode, content isolation, and auto-approval.
type SupervisorConfig struct {
	Enabled          *bool    `yaml:"enabled"` // when true, hide approval.* tools from agents
	ExposeContent    *bool    `yaml:"expose_content"`
	AutoApprove      *bool    `yaml:"auto_approve"`      // enable mem7-based auto-approval (default true)
	MinApprovals     int      `yaml:"min_approvals"`     // min past approvals for auto-approve (default 3)
	SupervisorAgents []string `yaml:"supervisor_agents"` // agent IDs (glob) allowed to see approval tools in supervisor mode
}

// IsEnabled returns whether supervisor mode is active.
// When enabled, approval.resolve and approval.pending are hidden from agents
// so that only the external supervisor can resolve approvals.
// Defaults to false when not explicitly set.
func (s SupervisorConfig) IsEnabled() bool {
	if s.Enabled == nil {
		return false
	}
	return *s.Enabled
}

// ShouldExposeContent returns whether raw param content should be exposed.
// Defaults to true when not explicitly set.
func (s SupervisorConfig) ShouldExposeContent() bool {
	if s.ExposeContent == nil {
		return true
	}
	return *s.ExposeContent
}

// IsAutoApproveEnabled returns whether mem7-based auto-approval is active.
// Defaults to true — active when memory.url is configured.
// Set to false to disable even when mem7 is available.
func (s SupervisorConfig) IsAutoApproveEnabled() bool {
	if s.AutoApprove == nil {
		return true
	}
	return *s.AutoApprove
}

// GetMinApprovals returns the threshold for auto-approval. Default 3.
func (s SupervisorConfig) GetMinApprovals() int {
	if s.MinApprovals <= 0 {
		return 3
	}
	return s.MinApprovals
}

// CLIToolConfig declares a CLI binary to wrap as governed tools.
type CLIToolConfig struct {
	Name          string                      `yaml:"name"`
	Bin           string                      `yaml:"bin"`
	DefaultAction string                      `yaml:"default_action"` // allow, deny, human_approval (default: deny)
	Strict        bool                        `yaml:"strict"`         // only declared commands allowed
	WorkingDir    string                      `yaml:"working_dir,omitempty"`
	Env           map[string]string           `yaml:"env,omitempty"`
	Commands      map[string]CLICommandConfig `yaml:"commands,omitempty"`
}

// CLICommandConfig declares a specific subcommand with constraints.
type CLICommandConfig struct {
	AllowedArgs []string `yaml:"allowed_args,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"` // e.g. "30s", "5m"
}

// ApprovalConfig controls the human approval gate behavior.
type ApprovalConfig struct {
	TimeoutSeconds int    `yaml:"timeout_seconds"` // default 300 (5 min)
	NotifyURL      string `yaml:"notify_url"`      // webhook URL for new pending approvals
	// Channel selects how human_approval requests are routed in MCP mode:
	//   queue        — always enqueue to the approval store (daemons, supervisor setups)
	//   tty          — require the interactive /dev/tty prompt; deny if unavailable (fail-closed)
	//   tty-fallback — try the TTY prompt, fall back to the queue (default, historical behavior)
	// The HTTP proxy path always uses the queue regardless of this setting.
	Channel string `yaml:"channel"`
}

// MCPServerConfig declares an upstream MCP server to connect to.
type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"` // "stdio" or "sse"
	Command   string            `yaml:"command,omitempty"`
	Args      []string          `yaml:"args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	URL       string            `yaml:"url,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty"`
}

type Policy struct {
	Name      string     `yaml:"name" json:"name"`
	Agent     string     `yaml:"agent" json:"agent"`
	RateLimit *RateLimit `yaml:"rate_limit,omitempty" json:"rate_limit,omitempty"`
	Rules     []Rule     `yaml:"rules" json:"rules"`
}

// RateLimit defines per-agent call constraints.
type RateLimit struct {
	MaxPerMinute int `yaml:"max_per_minute" json:"max_per_minute"`
	MaxTotal     int `yaml:"max_total" json:"max_total"`
}

type Rule struct {
	Tools     []string   `yaml:"tools" json:"tools"`
	Action    string     `yaml:"action" json:"action"`
	Condition *Condition `yaml:"condition,omitempty" json:"condition,omitempty"`
}

type Condition struct {
	Field    string  `yaml:"field" json:"field"`
	Operator string  `yaml:"operator" json:"operator"`
	Value    float64 `yaml:"value" json:"value"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = 9090
	}
	if cfg.Approval.TimeoutSeconds == 0 {
		cfg.Approval.TimeoutSeconds = 300
	}
	if cfg.Approval.Channel == "" {
		cfg.Approval.Channel = "tty-fallback"
	}
	switch cfg.Approval.Channel {
	case "queue", "tty", "tty-fallback":
	default:
		return nil, fmt.Errorf("approval.channel: unknown value %q (expected queue, tty or tty-fallback)", cfg.Approval.Channel)
	}
	if err := cfg.loadPolicyDir(path); err != nil {
		return nil, err
	}
	if err := cfg.validatePolicyNames(); err != nil {
		return nil, err
	}
	if err := cfg.validateCLITools(); err != nil {
		return nil, err
	}
	if err := cfg.validateOpenAPIs(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validateOpenAPIs() error {
	for i, oa := range c.OpenAPIs {
		if oa.URL == "" && oa.File == "" {
			return fmt.Errorf("openapi[%d]: url or file is required", i)
		}
		if oa.URL != "" && oa.File != "" {
			return fmt.Errorf("openapi[%d]: set url or file, not both", i)
		}
	}
	return nil
}

// loadPolicyDir loads per-agent policy files from the configured directory.
// Each *.yaml file in the directory is parsed as a single Policy and appended
// to c.Policies (after any inline policies, sorted by filename).
func (c *Config) loadPolicyDir(configPath string) error {
	if c.PolicyDir == "" {
		return nil
	}

	dir := c.PolicyDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(configPath), dir)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("policy_dir does not exist, skipping", "path", dir)
			return nil
		}
		return fmt.Errorf("read policy_dir %q: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return fmt.Errorf("read policy file %s: %w", e.Name(), err)
		}

		var p Policy
		if err := yaml.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("parse policy file %s: %w", e.Name(), err)
		}
		if p.Name == "" {
			return fmt.Errorf("policy file %s: missing required field 'name'", e.Name())
		}

		c.Policies = append(c.Policies, p)
	}

	return nil
}

// validatePolicyNames checks for duplicate policy names across all sources.
func (c *Config) validatePolicyNames() error {
	seen := make(map[string]bool, len(c.Policies))
	for _, p := range c.Policies {
		if seen[p.Name] {
			return fmt.Errorf("duplicate policy name %q", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// LoadPolicies loads and validates just the policies from a config file
// (inline policies + policy_dir). Returns the policy list and the resolved
// policy_dir path (empty if not configured).
func LoadPolicies(configPath string) ([]Policy, string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, "", err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, "", err
	}
	if err := cfg.loadPolicyDir(configPath); err != nil {
		return nil, "", err
	}
	if err := cfg.validatePolicyNames(); err != nil {
		return nil, "", err
	}
	policyDir := cfg.PolicyDir
	if policyDir != "" && !filepath.IsAbs(policyDir) {
		policyDir = filepath.Join(filepath.Dir(configPath), policyDir)
	}
	return cfg.Policies, policyDir, nil
}

func (c *Config) validateCLITools() error {
	seen := make(map[string]bool, len(c.CLITools))
	for i, ct := range c.CLITools {
		if ct.Name == "" {
			return fmt.Errorf("cli_tools[%d]: name is required", i)
		}
		if ct.Bin == "" {
			return fmt.Errorf("cli_tools[%d] (%s): bin is required", i, ct.Name)
		}
		if seen[ct.Name] {
			return fmt.Errorf("cli_tools[%d]: duplicate name %q", i, ct.Name)
		}
		seen[ct.Name] = true

		switch ct.DefaultAction {
		case "", "allow", "deny", "human_approval":
			// ok — empty defaults to "deny" at runtime
		default:
			return fmt.Errorf("cli_tools[%d] (%s): invalid default_action %q", i, ct.Name, ct.DefaultAction)
		}

		if ct.Strict && len(ct.Commands) == 0 {
			return fmt.Errorf("cli_tools[%d] (%s): strict mode requires at least one declared command", i, ct.Name)
		}

		for cmdName, cmd := range ct.Commands {
			if cmd.Timeout != "" {
				if _, err := time.ParseDuration(cmd.Timeout); err != nil {
					return fmt.Errorf("cli_tools[%d] (%s): command %q has invalid timeout %q: %w", i, ct.Name, cmdName, cmd.Timeout, err)
				}
			}
		}
	}
	return nil
}
