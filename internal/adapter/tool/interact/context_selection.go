package interact

func contextSelectionSchema() map[string]any {
	ids := func() map[string]any {
		return map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": float64(1)}}
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"description": "Explicit focus. Empty object clears focus. Replacement requires a user correction; existing sources remain auditable.",
		"properties": map[string]any{
			"group_ids": ids(), "item_ids": ids(),
			"replacements": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object", "additionalProperties": false,
					"properties": map[string]any{"old_group_id": map[string]any{"type": "string"}, "new_group_id": map[string]any{"type": "string"}},
					"required":   []string{"old_group_id", "new_group_id"},
				},
			},
		},
	}
}

func planInputAlternatives(name string) []any {
	choices := []any{map[string]any{"required": []string{"steps"}}}
	if name == "update_plan" {
		choices = append(choices, map[string]any{"required": []string{"context_selection"}})
	}
	return choices
}
