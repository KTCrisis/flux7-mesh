package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/mcp"
	"github.com/KTCrisis/flux7-mesh/registry"
)

func runDiscover(args []string) {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	configPath := fs.String("config", "", "Path to config YAML (discovers MCP servers defined in it)")
	specURL := fs.String("openapi", "", "OpenAPI spec URL to discover")
	backendURL := fs.String("backend", "", "Backend base URL (overrides spec)")
	genPolicy := fs.Bool("generate-policy", false, "Generate a suggested policy YAML")
	fs.Parse(args)

	if *configPath == "" && *specURL == "" {
		fmt.Fprintln(os.Stderr, "Usage: agent-mesh discover [--config config.yaml] [--openapi <url>] [--generate-policy]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Discover tools from MCP servers and OpenAPI specs without starting the proxy.")
		os.Exit(1)
	}

	// Quiet logging — only errors
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	reg := registry.New()
	var mcpManager *mcp.Manager

	// Discover from OpenAPI
	if *specURL != "" {
		fmt.Fprintf(os.Stderr, "Loading OpenAPI spec: %s\n", *specURL)
		if err := reg.LoadOpenAPI(*specURL, *backendURL, nil); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
	}

	// Discover from config (MCP servers)
	if *configPath != "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %s\n", err)
			os.Exit(1)
		}

		if *specURL == "" && len(cfg.MCPServers) == 0 {
			fmt.Fprintln(os.Stderr, "No MCP servers or OpenAPI specs to discover.")
			os.Exit(1)
		}

		if len(cfg.MCPServers) > 0 {
			mcpManager = mcp.NewManager()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

			for _, serverCfg := range cfg.MCPServers {
				var client *mcp.MCPClient
				switch serverCfg.Transport {
				case "stdio":
					fmt.Fprintf(os.Stderr, "Connecting to MCP server: %s (%s %s)\n", serverCfg.Name, serverCfg.Command, strings.Join(serverCfg.Args, " "))
					client = mcp.NewStdioClient(serverCfg.Name, serverCfg.Command, serverCfg.Args, serverCfg.Env)
				case "sse":
					fmt.Fprintf(os.Stderr, "Connecting to MCP server: %s (%s)\n", serverCfg.Name, serverCfg.URL)
					client = mcp.NewSSEClient(serverCfg.Name, serverCfg.URL, serverCfg.Headers)
				default:
					fmt.Fprintf(os.Stderr, "Skipping %s (unsupported transport: %s)\n", serverCfg.Name, serverCfg.Transport)
					continue
				}

				if err := client.Connect(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "  Error: %s\n", err)
					continue
				}
				mcpManager.Add(client)

				defs := convertMCPTools(client.Tools())
				reg.LoadMCP(serverCfg.Name, defs)
			}
			cancel()
		}
	}

	tools := reg.All()
	if len(tools) == 0 {
		fmt.Fprintln(os.Stderr, "No tools discovered.")
		os.Exit(0)
	}

	// Sort tools by name
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	// Group by source
	groups := groupTools(tools)

	// Print discovered tools
	fmt.Printf("\nDiscovered %d tools:\n", len(tools))
	for _, g := range groups {
		fmt.Printf("\n  %s (%d tools):\n", g.label, len(g.tools))
		for _, t := range g.tools {
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			if desc == "" {
				desc = "-"
			}
			fmt.Printf("    %-40s %s\n", t.Name, desc)
		}
	}

	// Generate policy
	if *genPolicy {
		fmt.Printf("\n# Suggested policy (read-only by default):\n\n")
		generatePolicy(groups)
	}

	// Cleanup MCP connections
	if mcpManager != nil {
		mcpManager.CloseAll()
	}
}

type toolGroup struct {
	label string
	tools []*registry.Tool
}

func groupTools(tools []*registry.Tool) []toolGroup {
	bySource := make(map[string][]*registry.Tool)
	order := []string{}

	for _, t := range tools {
		key := t.Source
		if t.MCPServer != "" {
			key = "mcp:" + t.MCPServer
		}
		if _, exists := bySource[key]; !exists {
			order = append(order, key)
		}
		bySource[key] = append(bySource[key], t)
	}

	groups := make([]toolGroup, 0, len(order))
	for _, key := range order {
		label := key
		if strings.HasPrefix(key, "mcp:") {
			label = "MCP server \"" + strings.TrimPrefix(key, "mcp:") + "\""
		} else if key == "openapi" {
			label = "OpenAPI"
		}
		groups = append(groups, toolGroup{label: label, tools: bySource[key]})
	}
	return groups
}

func generatePolicy(groups []toolGroup) {
	fmt.Println("policies:")
	fmt.Println("  - name: safe-mode")
	fmt.Println("    agent: \"*\"")
	fmt.Println("    rules:")

	for _, g := range groups {
		readTools := []string{}
		writeTools := []string{}

		for _, t := range g.tools {
			if isReadTool(t) {
				readTools = append(readTools, t.Name)
			} else {
				writeTools = append(writeTools, t.Name)
			}
		}

		if len(readTools) > 0 {
			fmt.Printf("      # %s — read operations\n", g.label)
			fmt.Printf("      - tools: [%s]\n", formatToolList(readTools))
			fmt.Println("        action: allow")
		}
		if len(writeTools) > 0 {
			fmt.Printf("      # %s — write operations\n", g.label)
			fmt.Printf("      - tools: [%s]\n", formatToolList(writeTools))
			fmt.Println("        action: deny")
		}
	}

	fmt.Println("")
	fmt.Println("  - name: default")
	fmt.Println("    agent: \"*\"")
	fmt.Println("    rules:")
	fmt.Println("      - tools: [\"*\"]")
	fmt.Println("        action: deny")
}

// isReadTool guesses if a tool is read-only based on its name and HTTP method.
func isReadTool(t *registry.Tool) bool {
	if t.Method == "GET" {
		return true
	}
	name := strings.ToLower(t.Name)
	readPrefixes := []string{"get_", "list_", "find_", "search_", "read_", "describe_", "show_", "fetch_", "query_"}
	readContains := []string{".read_", ".list_", ".get_", ".find_", ".search_", ".describe_", ".show_", ".fetch_", ".query_", ".directory_tree", ".get_file_info", ".list_allowed", "_read_", "_list_", "_get_", "_find_", "_search_", "_fetch_", "_query_"}
	for _, p := range readPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, c := range readContains {
		if strings.Contains(name, c) {
			return true
		}
	}
	return false
}

func formatToolList(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	joined := strings.Join(quoted, ", ")
	if len(joined) > 80 {
		// Multi-line
		return "\n          " + strings.Join(quoted, ",\n          ") + "\n        "
	}
	return joined
}
