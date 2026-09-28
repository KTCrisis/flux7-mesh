package registry

import (
	"encoding/json"
	"sync"
	"time"
)

// Tool represents a callable operation discovered from an OpenAPI spec, MCP server, or CLI binary.
type Tool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Method      string            `json:"method"`              // HTTP method (empty for MCP/CLI tools)
	Path        string            `json:"path"`                // HTTP path (empty for MCP/CLI tools)
	BaseURL     string            `json:"base_url"`            // backend URL (empty for MCP/CLI tools)
	Params      []Param           `json:"params,omitempty"`
	Headers     map[string]string `json:"-"`
	Source      string            `json:"source"`              // "openapi", "mcp", or "cli"
	MCPServer   string            `json:"mcp_server,omitempty"`
	CLIMeta     *CLIToolMeta      `json:"cli_meta,omitempty"`  // CLI-specific metadata
	Annotations *Annotations      `json:"annotations,omitempty"` // upstream MCP hints, nil if none
}

// Annotations are the behaviour hints an upstream MCP server declares for a
// tool. Pointers keep "absent" apart from "false". They are claims from the
// server, used to suggest a policy, never to relax one.
type Annotations struct {
	ReadOnly    *bool `json:"read_only,omitempty"`
	Destructive *bool `json:"destructive,omitempty"`
	Idempotent  *bool `json:"idempotent,omitempty"`
	OpenWorld   *bool `json:"open_world,omitempty"`
}

// CLIToolMeta holds CLI-specific metadata attached to a Tool.
type CLIToolMeta struct {
	Bin           string            `json:"bin"`
	Command       string            `json:"command"`                  // subcommand (e.g. "plan", "get")
	AllowedArgs   []string          `json:"allowed_args,omitempty"`   // nil = any args
	Timeout       time.Duration     `json:"timeout"`                  // 0 = default 30s
	WorkingDir    string            `json:"working_dir,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Strict        bool              `json:"strict"`
	DefaultAction string            `json:"default_action"`
	IsCatchAll    bool              `json:"is_catch_all"`             // true for dynamic dispatch
}

// DispatchFloor returns the least-permissive action this tool tolerates
// regardless of policy, or "" when no floor applies.
//
// It applies only to the dynamic dispatcher (`<name>.__dispatch`) of a
// non-strict CLI tool. Declared commands are governed by policy alone —
// declaring a command is itself the operator's grant. The dispatcher accepts
// any subcommand the binary knows, so a glob rule such as
// `terraform.*: allow` would otherwise hand over `destroy` along with `plan`.
//
// Nil-safe: an unknown tool has no floor.
func (t *Tool) DispatchFloor() string {
	if t == nil || t.CLIMeta == nil || !t.CLIMeta.IsCatchAll {
		return ""
	}
	return t.CLIMeta.DefaultAction
}

// Param describes a single parameter for a tool.
type Param struct {
	Name     string `json:"name"`
	In       string `json:"in"` // path, query, body
	Type     string `json:"type"`
	Required bool   `json:"required"`

	// RawSchema preserves the raw JSON Schema of this parameter when the tool
	// was imported from an upstream MCP server. When present, the MCP server's
	// tools/list handler emits this verbatim instead of rebuilding a shallow
	// {type, description} object — this keeps schema constructs like "anyOf",
	// "items", "enum" and nested objects intact for agents downstream.
	// Empty for locally-defined virtual tools and for OpenAPI/CLI imports.
	RawSchema json.RawMessage `json:"-"`
}

// Registry holds all discovered tools. Safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

func New() *Registry {
	return &Registry{tools: make(map[string]*Tool)}
}

func (r *Registry) Get(name string) *Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

func (r *Registry) All() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Remove deletes a tool by name.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// LoadManual registers a tool manually (for non-OpenAPI backends).
func (r *Registry) LoadManual(tool *Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
}

// set is the internal unlocked setter, for use by methods that already hold the lock.
func (r *Registry) set(name string, tool *Tool) {
	r.tools[name] = tool
}
