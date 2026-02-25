package mcp

import (
	"strings"
)

// Session schema injection helpers.
// We enforce a session requirement (session_id) on selected tools only.
// Role-specific fields like worker_id and role_code are injected separately.

func toolRequiresSession(role string, tool string) bool {
	role = strings.TrimSpace(role)
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return false
	}

	switch role {
	case "worker":
		// From claim task and after.
		switch tool {
		case "claimIssueTask",
			"extendIssueTaskLease",
			"lockFiles",
			"heartbeat",
			"unlock",
			"askIssueTask",
			"submitIssueTask",
			"listTaskDocs",
			"readTaskDoc",
			"writeTaskDoc",
			"getIssueTask":
			return true
		default:
			return false
		}
	case "lead":
		// From wait inbox and after.
		switch tool {
		case "waitIssueTaskEvents",
			"selectIssueInbox",
			"nextIssueSignal",
			"stepLeadInbox",
			"replyIssueTaskMessage",
			"reviewIssueTask",
			"getNextStepToken",
			"submitDelivery",
			"closeIssue":
			return true
		default:
			return false
		}
	case "acceptor":
		// From review and after.
		switch tool {
		case "reviewDelivery":
			return true
		default:
			return false
		}
	default:
		// Unknown role: keep old strict behavior out of schema; runtime may still enforce.
		return false
	}
}

func injectSessionIntoTools(role string, tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		req := toolRequiresSession(role, t.Name)
		out = append(out, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: injectSessionIntoSchema(t.InputSchema, req),
		})
	}
	return out
}

func injectSessionIntoSchema(schema any, requireSession bool) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}

	if requireSession {
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
	} else {
		// Ensure any legacy session fields are removed from required list.
		baseRequired := schemaRequiredStrings(m)
		filtered := make([]string, 0, len(baseRequired))
		for _, r := range baseRequired {
			if r == "session_id" || r == "semantic_session_id" {
				continue
			}
			filtered = append(filtered, r)
		}
		schemaSetRequiredStrings(m, filtered)
	}

	props, ok := m["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		m["properties"] = props
	}

	if requireSession {
		// Normalize session semantics: only session_id is exposed and required.
		// We intentionally override per-tool description to avoid drift.
		props["session_id"] = map[string]any{
			"type":        "string",
			"description": "Session id (cookie-like).",
		}
		delete(props, "semantic_session_id")
	} else {
		// For tools that don't require session, do NOT expose session_id.
		delete(props, "session_id")
		delete(props, "semantic_session_id")
	}
	delete(m, "anyOf")
	delete(m, "oneOf")
	return m
}
