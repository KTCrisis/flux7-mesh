package main

import (
	"testing"

	"github.com/KTCrisis/flux7-mesh/registry"
)

func TestGroupTools(t *testing.T) {
	tools := []*registry.Tool{
		{Name: "get_order", Source: "openapi"},
		{Name: "post_order", Source: "openapi"},
		{Name: "fs.read_file", Source: "mcp", MCPServer: "fs"},
		{Name: "fs.write_file", Source: "mcp", MCPServer: "fs"},
		{Name: "gmail.list_emails", Source: "mcp", MCPServer: "gmail"},
	}

	groups := groupTools(tools)

	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3", len(groups))
	}

	// Check labels
	if groups[0].label != "OpenAPI" {
		t.Errorf("group[0].label = %q, want OpenAPI", groups[0].label)
	}
	if groups[1].label != "MCP server \"fs\"" {
		t.Errorf("group[1].label = %q", groups[1].label)
	}
	if groups[2].label != "MCP server \"gmail\"" {
		t.Errorf("group[2].label = %q", groups[2].label)
	}

	// Check counts
	if len(groups[0].tools) != 2 {
		t.Errorf("openapi tools = %d, want 2", len(groups[0].tools))
	}
	if len(groups[1].tools) != 2 {
		t.Errorf("fs tools = %d, want 2", len(groups[1].tools))
	}
	if len(groups[2].tools) != 1 {
		t.Errorf("gmail tools = %d, want 1", len(groups[2].tools))
	}
}

func TestFormatToolList(t *testing.T) {
	// Short list — single line
	short := formatToolList([]string{"read_file", "write_file"})
	if short != `"read_file", "write_file"` {
		t.Errorf("short = %q", short)
	}

	// Long list — should go multi-line
	long := formatToolList([]string{
		"filesystem.read_file", "filesystem.write_file", "filesystem.list_directory",
		"filesystem.search_files", "filesystem.get_file_info",
	})
	if long[0] != '\n' {
		t.Errorf("long list should start with newline for multi-line, got: %q", long[:20])
	}
}
