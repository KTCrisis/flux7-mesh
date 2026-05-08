package registry

import (
	"fmt"
	"sync"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
)

func TestNewRegistry(t *testing.T) {
	r := New()
	if r == nil {
		t.Fatal("New() returned nil")
	}
	if len(r.All()) != 0 {
		t.Errorf("new registry should be empty, got %d", len(r.All()))
	}
}

func TestLoadManual(t *testing.T) {
	r := New()
	r.LoadManual(&Tool{Name: "test_tool", Description: "a test", Source: "openapi"})

	if tool := r.Get("test_tool"); tool == nil {
		t.Fatal("Get returned nil for registered tool")
	} else if tool.Description != "a test" {
		t.Errorf("description = %q", tool.Description)
	}

	if r.Get("nonexistent") != nil {
		t.Error("Get should return nil for unknown tool")
	}
}

func TestRemove(t *testing.T) {
	r := New()
	r.LoadManual(&Tool{Name: "a"})
	r.LoadManual(&Tool{Name: "b"})
	r.Remove("a")

	if r.Get("a") != nil {
		t.Error("tool 'a' should be removed")
	}
	if r.Get("b") == nil {
		t.Error("tool 'b' should still exist")
	}
}

func TestLoadMCP(t *testing.T) {
	r := New()
	defs := []MCPToolDef{
		{Name: "read_file", Description: "Read a file", Params: []Param{{Name: "path", In: "body", Type: "string", Required: true}}},
		{Name: "write_file", Description: "Write a file", Params: []Param{{Name: "path", In: "body", Type: "string", Required: true}}},
	}
	r.LoadMCP("filesystem", defs)

	if len(r.All()) != 2 {
		t.Fatalf("tools = %d, want 2", len(r.All()))
	}

	tool := r.Get("filesystem.read_file")
	if tool == nil {
		t.Fatal("tool filesystem.read_file not found")
	}
	if tool.Source != "mcp" {
		t.Errorf("source = %q, want mcp", tool.Source)
	}
	if tool.MCPServer != "filesystem" {
		t.Errorf("mcp_server = %q, want filesystem", tool.MCPServer)
	}
	if tool.Description != "Read a file" {
		t.Errorf("description = %q", tool.Description)
	}
	if len(tool.Params) != 1 || tool.Params[0].Name != "path" {
		t.Errorf("params = %+v", tool.Params)
	}
}

func TestLoadMCPNamespacing(t *testing.T) {
	r := New()
	r.LoadMCP("server-a", []MCPToolDef{{Name: "do_thing"}})
	r.LoadMCP("server-b", []MCPToolDef{{Name: "do_thing"}})

	if len(r.All()) != 2 {
		t.Fatalf("tools = %d, want 2 (no collision)", len(r.All()))
	}
	if r.Get("server-a.do_thing") == nil {
		t.Error("server-a.do_thing not found")
	}
	if r.Get("server-b.do_thing") == nil {
		t.Error("server-b.do_thing not found")
	}
}

func TestRemoveByServer(t *testing.T) {
	r := New()
	r.LoadMCP("fs", []MCPToolDef{{Name: "read"}, {Name: "write"}})
	r.LoadMCP("db", []MCPToolDef{{Name: "query"}})
	r.LoadManual(&Tool{Name: "rest_tool", Source: "openapi"})

	r.RemoveByServer("fs")

	if r.Get("fs.read") != nil || r.Get("fs.write") != nil {
		t.Error("fs tools should be removed")
	}
	if r.Get("db.query") == nil {
		t.Error("db.query should still exist")
	}
	if r.Get("rest_tool") == nil {
		t.Error("rest_tool should still exist")
	}
}

func TestNewMCPToolDef(t *testing.T) {
	def := NewMCPToolDef("my_tool", "does stuff", map[string]MCPPropDef{
		"path":    {Type: "string"},
		"content": {Type: "string"},
		"force":   {Type: "boolean"},
	}, []string{"path"})

	if def.Name != "my_tool" {
		t.Errorf("name = %q", def.Name)
	}
	if def.Description != "does stuff" {
		t.Errorf("description = %q", def.Description)
	}
	if len(def.Params) != 3 {
		t.Fatalf("params = %d, want 3", len(def.Params))
	}

	// Check required flag
	paramMap := make(map[string]Param)
	for _, p := range def.Params {
		paramMap[p.Name] = p
	}
	if !paramMap["path"].Required {
		t.Error("path should be required")
	}
	if paramMap["content"].Required {
		t.Error("content should not be required")
	}
	if paramMap["force"].Required {
		t.Error("force should not be required")
	}
}

func TestMixedSources(t *testing.T) {
	r := New()
	r.LoadManual(&Tool{Name: "get_order", Source: "openapi", Method: "GET", Path: "/orders/{id}"})
	r.LoadMCP("fs", []MCPToolDef{{Name: "read_file"}})

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("tools = %d, want 2", len(all))
	}

	rest := r.Get("get_order")
	if rest.Source != "openapi" {
		t.Errorf("rest source = %q", rest.Source)
	}

	mcp := r.Get("fs.read_file")
	if mcp.Source != "mcp" {
		t.Errorf("mcp source = %q", mcp.Source)
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := New()
	var wg sync.WaitGroup

	// Concurrent writes
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.LoadManual(&Tool{Name: fmt.Sprintf("tool_%d", n), Source: "openapi"})
		}(i)
	}

	// Concurrent reads while writing
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.All()
			r.Get("tool_0")
		}()
	}

	wg.Wait()

	if len(r.All()) != 50 {
		t.Errorf("tools = %d, want 50", len(r.All()))
	}
}

func TestConcurrentMCPLoadRemove(t *testing.T) {
	r := New()
	var wg sync.WaitGroup

	// Load from multiple servers concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.LoadMCP(fmt.Sprintf("server-%d", n), []MCPToolDef{
				{Name: "read"}, {Name: "write"},
			})
		}(i)
	}
	wg.Wait()

	if len(r.All()) != 20 {
		t.Errorf("tools = %d, want 20", len(r.All()))
	}

	// Remove concurrently
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.RemoveByServer(fmt.Sprintf("server-%d", n))
		}(i)
	}
	wg.Wait()

	if len(r.All()) != 0 {
		t.Errorf("tools = %d, want 0 after removing all servers", len(r.All()))
	}
}

func TestLoadCLI_SimpleMode(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{Name: "gh", Bin: "gh", DefaultAction: "allow"},
	})

	// Should register catch-all dispatcher
	dispatch := r.Get("gh.__dispatch")
	if dispatch == nil {
		t.Fatal("expected gh.__dispatch tool")
	}
	if dispatch.Source != "cli" {
		t.Errorf("source = %q, want cli", dispatch.Source)
	}
	if dispatch.CLIMeta == nil || !dispatch.CLIMeta.IsCatchAll {
		t.Error("expected catch-all meta")
	}
}

func TestLoadCLI_FineTunedMode(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name:          "terraform",
			Bin:           "terraform",
			DefaultAction: "human_approval",
			Commands: map[string]config.CLICommandConfig{
				"plan":  {Timeout: "120s"},
				"apply": {AllowedArgs: []string{"-target"}, Timeout: "300s"},
			},
		},
	})

	// Declared commands should be registered
	plan := r.Get("terraform.plan")
	if plan == nil {
		t.Fatal("expected terraform.plan")
	}
	if plan.CLIMeta.Command != "plan" {
		t.Errorf("command = %q, want plan", plan.CLIMeta.Command)
	}

	apply := r.Get("terraform.apply")
	if apply == nil {
		t.Fatal("expected terraform.apply")
	}
	if len(apply.CLIMeta.AllowedArgs) != 1 || apply.CLIMeta.AllowedArgs[0] != "-target" {
		t.Errorf("allowed_args = %v", apply.CLIMeta.AllowedArgs)
	}

	// Catch-all should also exist (not strict)
	if r.Get("terraform.__dispatch") == nil {
		t.Error("expected terraform.__dispatch for non-strict mode")
	}
}

func TestLoadCLI_StrictMode(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name:   "kubectl",
			Bin:    "kubectl",
			Strict: true,
			Commands: map[string]config.CLICommandConfig{
				"get": {AllowedArgs: []string{"-n", "--namespace"}},
			},
		},
	})

	// Declared command should exist
	if r.Get("kubectl.get") == nil {
		t.Fatal("expected kubectl.get")
	}

	// No catch-all in strict mode
	if r.Get("kubectl.__dispatch") != nil {
		t.Error("strict mode should NOT have __dispatch")
	}
}

func TestResolveCLI_ExactMatch(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name:          "terraform",
			Bin:           "terraform",
			DefaultAction: "allow",
			Commands: map[string]config.CLICommandConfig{
				"plan": {},
			},
		},
	})

	// Exact match
	tool := r.ResolveCLI("terraform.plan")
	if tool == nil {
		t.Fatal("expected exact match for terraform.plan")
	}
	if tool.CLIMeta.Command != "plan" {
		t.Errorf("command = %q", tool.CLIMeta.Command)
	}
}

func TestResolveCLI_FallbackDispatch(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{Name: "terraform", Bin: "terraform", DefaultAction: "allow"},
	})

	// "terraform.init" doesn't exist, should fallback to terraform.__dispatch
	tool := r.ResolveCLI("terraform.init")
	if tool == nil {
		t.Fatal("expected fallback to __dispatch")
	}
	if !tool.CLIMeta.IsCatchAll {
		t.Error("expected catch-all tool")
	}
}

func TestResolveCLI_StrictNoFallback(t *testing.T) {
	r := New()
	r.LoadCLI([]config.CLIToolConfig{
		{
			Name: "kubectl", Bin: "kubectl", Strict: true,
			Commands: map[string]config.CLICommandConfig{"get": {}},
		},
	})

	// "kubectl.exec" should return nil in strict mode (no __dispatch)
	if r.ResolveCLI("kubectl.exec") != nil {
		t.Error("strict mode should not resolve undeclared commands")
	}
}
