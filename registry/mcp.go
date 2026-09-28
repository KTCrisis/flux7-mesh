package registry

import "encoding/json"

// MCPToolDef describes a tool discovered from an upstream MCP server.
// This is the registry's input format — callers convert from their MCP types to this.
type MCPToolDef struct {
	Name        string
	Description string
	Params      []Param
	Annotations *Annotations
}

// NewMCPToolDef creates an MCPToolDef from raw MCP schema data.
func NewMCPToolDef(name, description string, properties map[string]MCPPropDef, required []string) MCPToolDef {
	requiredSet := make(map[string]bool, len(required))
	for _, r := range required {
		requiredSet[r] = true
	}
	var params []Param
	for pName, prop := range properties {
		params = append(params, Param{
			Name:      pName,
			In:        "body",
			Type:      prop.Type,
			Required:  requiredSet[pName],
			RawSchema: prop.RawSchema,
		})
	}
	return MCPToolDef{
		Name:        name,
		Description: description,
		Params:      params,
	}
}

// MCPPropDef is a minimal property definition for MCP tool schema conversion.
// RawSchema, when set, is the verbatim JSON Schema of the property as received
// from the upstream MCP server. Callers that have access to the raw schema
// should populate this so the MCP server's re-export layer can pass through
// constructs like "anyOf", "items", "enum" and nested objects intact.
type MCPPropDef struct {
	Type      string
	RawSchema json.RawMessage
}

// LoadMCP registers tools from an upstream MCP server into the registry.
// Tool names are namespaced as "serverName.toolName" to avoid collisions.
func (r *Registry) LoadMCP(serverName string, tools []MCPToolDef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		name := serverName + "." + t.Name
		r.set(name, &Tool{
			Name:        name,
			Description: t.Description,
			Source:      "mcp",
			MCPServer:   serverName,
			Params:      t.Params,
			Annotations: t.Annotations,
		})
	}
}

// RemoveByServer removes all tools from a given MCP server.
func (r *Registry) RemoveByServer(serverName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, t := range r.tools {
		if t.MCPServer == serverName {
			delete(r.tools, name)
		}
	}
}
