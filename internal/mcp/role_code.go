package mcp

import (
	"os"
	"strings"
)

// Role code injection helpers.
// We enforce a role-specific code on all tools exposed to a role when configured.

func expectedRoleCode(role string) string {
	role = strings.TrimSpace(strings.ToUpper(role))
	if role != "" {
		if v := strings.TrimSpace(os.Getenv("SWARM_MCP_ROLE_CODE_" + role)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("SWARM_MCP_ROLE_CODE"))
}

func injectRoleCodeIntoTools(role string, tools []ToolDefinition) []ToolDefinition {
	tok := expectedRoleCode(role)
	if tok == "" {
		return tools
	}
	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: injectRoleCodeIntoSchema(t.InputSchema),
		})
	}
	return out
}

func injectRoleCodeIntoSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}
	if _, exists := props["role_code"]; !exists {
		props["role_code"] = map[string]any{"type": "string", "description": "Role code required for this MCP role."}
	}
	// Ensure required includes role_code
	schemaAddRequired(m, "role_code")
	return m
}
