package mcp

// Session schema injection helpers.
// We enforce a session requirement (session_id) on all tools.
// Role-specific fields like worker_id and role_code are injected separately.

func injectSessionIntoTools(tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: injectSessionIntoSchema(t.InputSchema),
		})
	}
	return out
}

func injectSessionIntoSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	// We enforce session_id as a single required field.
	// Keep the existing required fields (except any session fields), and add session_id.
	baseRequired := schemaRequiredStrings(m)
	filtered := make([]string, 0, len(baseRequired))
	for _, r := range baseRequired {
		if r == "session_id" || r == "semantic_session_id" {
			continue
		}
		filtered = append(filtered, r)
	}
	filtered = append(filtered, "session_id")
	schemaSetRequiredStrings(m, filtered)

	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}

	// Normalize session semantics: only session_id is exposed and required.
	// We intentionally override per-tool description to avoid drift.
	props["session_id"] = map[string]any{
		"type":        "string",
		"description": "Session id (cookie-like). Required.",
	}
	delete(props, "semantic_session_id")
	delete(m, "anyOf")
	delete(m, "oneOf")
	return m
}
