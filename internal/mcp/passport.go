package mcp

import (
	"os"
	"strings"
)

func expectedPassportToken(role string) string {
	role = strings.TrimSpace(strings.ToUpper(role))
	if role != "" {
		if v := strings.TrimSpace(os.Getenv("SWARM_MCP_PASSPORT_TOKEN_" + role)); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("SWARM_MCP_PASSPORT_TOKEN"))
}

func injectPassportTokenIntoTools(role string, tools []ToolDefinition) []ToolDefinition {
	tok := expectedPassportToken(role)
	if tok == "" {
		return tools
	}
	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: injectPassportTokenIntoSchema(t.InputSchema),
		})
	}
	return out
}

func injectPassportTokenIntoSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}
	if _, exists := props["passport_token"]; !exists {
		props["passport_token"] = map[string]any{"type": "string", "description": "Passport token required for this MCP role."}
	}
	// Ensure required includes passport_token
	req, _ := m["required"].([]any)
	for _, r := range req {
		if s, ok := r.(string); ok && s == "passport_token" {
			return m
		}
	}
	m["required"] = append(req, "passport_token")
	return m
}
