package main

import (
	"testing"

	"github.com/KTCrisis/flux7-mesh/registry"
)

func TestIsReadTool(t *testing.T) {
	tests := []struct {
		tool *registry.Tool
		want bool
	}{
		// Read tools
		{&registry.Tool{Name: "get_order", Method: "GET"}, true},
		{&registry.Tool{Name: "list_users"}, true},
		{&registry.Tool{Name: "find_pets_by_status"}, true},
		{&registry.Tool{Name: "search_files"}, true},
		{&registry.Tool{Name: "read_file"}, true},
		{&registry.Tool{Name: "filesystem.read_file"}, true},
		{&registry.Tool{Name: "filesystem.list_directory"}, true},
		{&registry.Tool{Name: "filesystem.get_file_info"}, true},
		{&registry.Tool{Name: "filesystem.directory_tree"}, true},
		{&registry.Tool{Name: "filesystem.list_allowed_directories"}, true},
		{&registry.Tool{Name: "gmail.gmail_list_emails"}, true},

		// Write tools
		{&registry.Tool{Name: "create_order", Method: "POST"}, false},
		{&registry.Tool{Name: "delete_pet"}, false},
		{&registry.Tool{Name: "update_user"}, false},
		{&registry.Tool{Name: "filesystem.write_file"}, false},
		{&registry.Tool{Name: "filesystem.edit_file"}, false},
		{&registry.Tool{Name: "filesystem.move_file"}, false},
		{&registry.Tool{Name: "gmail.gmail_send_email"}, false},
		{&registry.Tool{Name: "gmail.gmail_delete_email"}, false},
	}

	for _, tt := range tests {
		got := isReadTool(tt.tool)
		if got != tt.want {
			t.Errorf("isReadTool(%q) = %v, want %v", tt.tool.Name, got, tt.want)
		}
	}
}

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
