package generation

import (
	"encoding/json"
	"strings"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// presentationFixture reads concise test literals into typed trusted facts.
// Numeric strings are accepted only by this fixture builder, never by the presenter.
func presentationFixture(name string, raw json.RawMessage) tooldata.Result {
	result := tooldata.Result{JSON: raw}
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil {
		return result
	}
	var normalize func(any)
	normalize = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, v := range value {
				switch key {
				case "amount_minor", "total_amount_minor", "operation_count", "total_operation_count", "total_count", "average_minor", "amount_from_minor", "amount_to_minor":
					if text, ok := v.(string); ok {
						value[key] = json.Number(text)
					}
				default:
					normalize(v)
				}
			}
		case []any:
			for _, v := range value {
				normalize(v)
			}
		}
	}
	normalize(body)
	// Top-level catalog actions in these literals are wrapped like nested actions;
	// trusted facts carry the selected navigation filter directly.
	if blocks, ok := body["blocks"].([]any); ok {
		for _, b := range blocks {
			if block, ok := b.(map[string]any); ok {
				action, ok := block["action"].(map[string]any)
				if !ok {
					continue
				}
				if action["type"] == "open_operations" {
					block["action"] = action["filter"]
				} else {
					delete(block, "action")
				}
			}
		}
	}
	normalized, err := json.Marshal(body)
	if err != nil {
		return result
	}
	switch name {
	case "aggregate_operations":
		var facts tooldata.Aggregate
		if json.Unmarshal(normalized, &facts) == nil {
			result.Aggregate = &facts
		}
	case "find_operations":
		var facts tooldata.Operations
		if json.Unmarshal(normalized, &facts) == nil {
			result.Operations = &facts
		}
	case "get_data_overview":
		var facts tooldata.Overview
		if json.Unmarshal(normalized, &facts) == nil {
			result.Overview = &facts
		}
	case "get_analytics":
		var facts tooldata.Analytics
		if json.Unmarshal(normalized, &facts) == nil {
			result.Analytics = &facts
		}
	}
	return result
}
