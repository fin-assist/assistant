package generation

import (
	"context"
	"encoding/json"

	"github.com/fin-assist/assistant/internal/tooldata"
)

const readToolDescription = "Read verified financial data; present_result renders trusted figures and navigation from a completed read, never accepts invented values; cannot_answer ends an impossible answer."

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func builtinDefinition(name string, properties map[string]any, required ...string) ToolDefinition {
	parameters := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		parameters["required"] = required
	}
	raw, _ := json.Marshal(parameters)
	return ToolDefinition{Name: name, Description: readToolDescription, Parameters: raw}
}

func filterSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"from": stringProperty("Inclusive RFC3339 start"), "to": stringProperty("Exclusive RFC3339 end"), "q": stringProperty("Description search"), "category_id": stringProperty("Category identifier from trusted data"), "selection": stringProperty("Opaque token from trusted data"), "kind": map[string]any{"type": "string", "enum": []string{"expense", "income"}}, "amount_from_minor": map[string]any{"type": "integer", "minimum": 0}, "amount_to_minor": map[string]any{"type": "integer", "minimum": 0}}}
}

// BuiltinTools assembles the existing financial readers and local policy tools.
// Data remains a convenience bundle; Engine and new read extensions do not depend on it.
// A nil bundle retains definitions for disabled-model metrics; reads fail unavailable.
func BuiltinTools(data tooldata.Data) []Tool {
	return []Tool{OverviewTool(data), AggregateTool(data), FindTool(data), AnalyticsTool(data), PeriodsTool(data), offTopicTool(), presentationTool(), cannotAnswerTool()}
}

// OverviewTool binds only the coverage reader consumed by this function.
func OverviewTool(reader interface {
	Overview(context.Context, tooldata.Subject) (tooldata.Result, error)
}) Tool {
	definition := builtinDefinition("get_data_overview", map[string]any{})
	return NewReadTool(definition, emptyArguments, func(ctx context.Context, subject tooldata.Subject, _ struct{}) (tooldata.Result, error) {
		if reader == nil {
			return tooldata.Result{}, tooldata.ErrUnavailable
		}
		return reader.Overview(ctx, subject)
	}, false)
}

// AggregateTool binds the typed aggregate contract and only its reader.
func AggregateTool(reader interface {
	Aggregate(context.Context, tooldata.Subject, tooldata.AggregateArgs) (tooldata.Result, error)
}) Tool {
	definition := builtinDefinition("aggregate_operations", map[string]any{"filter": filterSchema(), "all_time": map[string]any{"type": "boolean"}, "group_by": map[string]any{"type": "array", "maxItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"month", "category", "merchant"}}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}})
	decode := func(raw json.RawMessage) (tooldata.AggregateArgs, error) {
		var args struct {
			Filter  filterArguments `json:"filter"`
			AllTime bool            `json:"all_time"`
			GroupBy []string        `json:"group_by"`
			Limit   int             `json:"limit"`
		}
		if json.Unmarshal(raw, &args) != nil || !validFilter(args.Filter, args.AllTime) || args.Limit < 0 || args.Limit > 200 || len(args.GroupBy) > 2 {
			return tooldata.AggregateArgs{}, errModelProtocol
		}
		seen := map[string]bool{}
		for _, group := range args.GroupBy {
			if seen[group] || (group != "month" && group != "category" && group != "merchant") {
				return tooldata.AggregateArgs{}, errModelProtocol
			}
			seen[group] = true
		}
		return tooldata.AggregateArgs{Filter: args.Filter.domain(), AllTime: args.AllTime, GroupBy: args.GroupBy, Limit: args.Limit}, nil
	}
	return NewReadTool(definition, decode, func(ctx context.Context, subject tooldata.Subject, args tooldata.AggregateArgs) (tooldata.Result, error) {
		if reader == nil {
			return tooldata.Result{}, tooldata.ErrUnavailable
		}
		return reader.Aggregate(ctx, subject, args)
	}, true)
}

// FindTool binds the typed operation search contract and only its reader.
func FindTool(reader interface {
	Find(context.Context, tooldata.Subject, tooldata.FindArgs) (tooldata.Result, error)
}) Tool {
	definition := builtinDefinition("find_operations", map[string]any{"filter": filterSchema(), "all_time": map[string]any{"type": "boolean"}, "order": map[string]any{"type": "string", "enum": []string{"newest", "largest"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}})
	decode := func(raw json.RawMessage) (tooldata.FindArgs, error) {
		var args struct {
			Filter  filterArguments `json:"filter"`
			AllTime bool            `json:"all_time"`
			Order   string          `json:"order"`
			Limit   int             `json:"limit"`
		}
		if json.Unmarshal(raw, &args) != nil || !validFilter(args.Filter, args.AllTime) || args.Limit < 0 || args.Limit > 100 || (args.Order != "" && args.Order != "newest" && args.Order != "largest") {
			return tooldata.FindArgs{}, errModelProtocol
		}
		return tooldata.FindArgs{Filter: args.Filter.domain(), AllTime: args.AllTime, Order: args.Order, Limit: args.Limit}, nil
	}
	return NewReadTool(definition, decode, func(ctx context.Context, subject tooldata.Subject, args tooldata.FindArgs) (tooldata.Result, error) {
		if reader == nil {
			return tooldata.Result{}, tooldata.ErrUnavailable
		}
		return reader.Find(ctx, subject, args)
	}, true)
}

// AnalyticsTool binds the calendar analytics contract and only its reader.
func AnalyticsTool(reader interface {
	Analytics(context.Context, tooldata.Subject, tooldata.AnalyticsArgs) (tooldata.Result, error)
}) Tool {
	definition := builtinDefinition("get_analytics", map[string]any{"period": map[string]any{"type": "string", "enum": []string{"month", "quarter", "year"}}, "period_key": stringProperty("Calendar key: YYYY-MM, YYYY-Qn, YYYY")})
	decode := func(raw json.RawMessage) (tooldata.AnalyticsArgs, error) {
		var args struct {
			Period    string `json:"period"`
			PeriodKey string `json:"period_key"`
		}
		if json.Unmarshal(raw, &args) != nil || !validPeriod(args.Period, args.PeriodKey) {
			return tooldata.AnalyticsArgs{}, errModelProtocol
		}
		return tooldata.AnalyticsArgs{Period: args.Period, PeriodKey: args.PeriodKey}, nil
	}
	return NewReadTool(definition, decode, func(ctx context.Context, subject tooldata.Subject, args tooldata.AnalyticsArgs) (tooldata.Result, error) {
		if reader == nil {
			return tooldata.Result{}, tooldata.ErrUnavailable
		}
		return reader.Analytics(ctx, subject, args)
	}, true)
}

// PeriodsTool binds the available-period contract and only its reader.
func PeriodsTool(reader interface {
	Periods(context.Context, tooldata.Subject, tooldata.PeriodsArgs) (tooldata.Result, error)
}) Tool {
	definition := builtinDefinition("list_periods", map[string]any{"period": map[string]any{"type": "string", "enum": []string{"month", "quarter", "year"}}})
	decode := func(raw json.RawMessage) (tooldata.PeriodsArgs, error) {
		var args struct {
			Period string `json:"period"`
		}
		if json.Unmarshal(raw, &args) != nil || !validPeriod(args.Period, "") {
			return tooldata.PeriodsArgs{}, errModelProtocol
		}
		return tooldata.PeriodsArgs{Period: args.Period}, nil
	}
	return NewReadTool(definition, decode, func(ctx context.Context, subject tooldata.Subject, args tooldata.PeriodsArgs) (tooldata.Result, error) {
		if reader == nil {
			return tooldata.Result{}, tooldata.ErrUnavailable
		}
		return reader.Periods(ctx, subject, args)
	}, false)
}

func emptyArguments(raw json.RawMessage) (struct{}, error) {
	var args struct{}
	return args, json.Unmarshal(raw, &args)
}

func offTopicTool() Tool {
	d := builtinDefinition("off_topic", map[string]any{})
	d.Description = "End a question outside personal finance with empty arguments {}; takes priority over every other function. Financial questions with an impossible answer use cannot_answer instead."
	return Tool{definition: d, priority: true, prepare: func(item ModelItem) (toolPlan, error) {
		return toolPlan{item: item, terminal: "OFF_TOPIC"}, nil
	}}
}

func cannotAnswerTool() Tool {
	return Tool{definition: builtinDefinition("cannot_answer", map[string]any{}), prepare: func(item ModelItem) (toolPlan, error) {
		return toolPlan{item: item, terminal: "CANNOT_ANSWER"}, nil
	}}
}

func presentationTool() Tool {
	d := builtinDefinition("present_result", map[string]any{"result_id": stringProperty("call_id of a completed financial read from a previous step"), "view": map[string]any{"type": "string", "enum": []string{"rows", "chart", "analytics"}}, "path": stringProperty("For analytics: existing blocks[i] or blocks[i].data.tiles[j]"), "coverage_result_id": stringProperty("For aggregate month charts: completed get_data_overview call_id with matching version and calendar coverage")}, "result_id", "view")
	return Tool{definition: d, prepare: func(item ModelItem) (toolPlan, error) {
		var args presentationArgs
		if json.Unmarshal([]byte(item.Arguments), &args) != nil || args.ResultID == "" || (args.View != "rows" && args.View != "chart" && args.View != "analytics") {
			return toolPlan{}, errModelProtocol
		}
		return toolPlan{item: item, presentation: &args}, nil
	}}
}
