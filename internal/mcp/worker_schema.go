package mcp

import "strings"

func injectWorkerIDIntoTools(role string, tools []ToolDefinition) []ToolDefinition {
	if strings.TrimSpace(role) != "worker" {
		return tools
	}

	// Only enforce worker_id on worker-context tools.
	// This includes locks and task-related tools to prevent cross-worker operations.
	// Note: role_code is injected separately for all tools when configured.
	workerRequired := map[string]bool{
		"claimIssueTask":       true,
		"submitIssueTask":      true,
		"askIssueTask":         true,
		"postIssueTaskMessage": true,
		"lockFiles":            true,
		"heartbeat":            true,
		"unlock":               true,
		"listLocks":            true,
	}

	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if workerRequired[t.Name] {
			out = append(out, ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: injectWorkerIDIntoSchema(t.InputSchema),
			})
			continue
		}
		out = append(out, t)
	}
	return out
}

func injectWorkerIDIntoSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}
	if _, exists := props["worker_id"]; !exists {
		props["worker_id"] = map[string]any{"type": "string", "description": "Worker employee ID used for identity binding."}
	}

	// Ensure required includes worker_id
	schemaAddRequired(m, "worker_id")
	return m
}
