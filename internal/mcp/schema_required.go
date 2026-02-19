package mcp

// Schema required-field helpers.
//
// We store inputSchema as map[string]any, and "required" may be either []string
// (from our schema builder) or []any (after JSON round-trips). These helpers
// normalize behavior so injections never overwrite existing required fields.

func schemaRequiredStrings(m map[string]any) []string {
	if m == nil {
		return nil
	}
	if raw, ok := m["required"].([]string); ok {
		out := make([]string, 0, len(raw))
		seen := map[string]bool{}
		for _, s := range raw {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
		return out
	}
	if raw, ok := m["required"].([]any); ok {
		out := make([]string, 0, len(raw))
		seen := map[string]bool{}
		for _, v := range raw {
			s, _ := v.(string)
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
		return out
	}
	return nil
}

func schemaSetRequiredStrings(m map[string]any, required []string) {
	if m == nil {
		return
	}
	if len(required) == 0 {
		delete(m, "required")
		return
	}
	out := make([]any, 0, len(required))
	seen := map[string]bool{}
	for _, s := range required {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	m["required"] = out
}

func schemaAddRequired(m map[string]any, name string) {
	if m == nil {
		return
	}
	req := schemaRequiredStrings(m)
	for _, r := range req {
		if r == name {
			return
		}
	}
	req = append(req, name)
	schemaSetRequiredStrings(m, req)
}

func schemaRemoveRequired(m map[string]any, name string) {
	if m == nil {
		return
	}
	req := schemaRequiredStrings(m)
	if len(req) == 0 {
		return
	}
	out := make([]string, 0, len(req))
	for _, r := range req {
		if r == name {
			continue
		}
		out = append(out, r)
	}
	schemaSetRequiredStrings(m, out)
}
