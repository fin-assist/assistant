package generation

import (
	"context"
	"encoding/json"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// Tool descriptions are trusted server text sent with every model step; they count
// toward the per-step input budget, so each stays one or two sentences.
const (
	overviewDescription     = "Returns the user's data coverage: overall bounds, per-month coverage, data fields the sources provide (an absent field is unknown, not zero) and the data version. Call it first for any question about the user's own finances."
	aggregateDescription    = "Returns the total amount and operation count for a filter, optionally grouped by month, category or merchant, with period coverage. Use it for sums, counts and breakdowns; use find_operations only to list individual operations."
	findDescription         = "Returns a bounded list of individual operations matching a filter and the total number of matches. Use it to show or inspect specific operations, not to compute totals."
	analyticsDescription    = "Returns the ready analytics for one calendar period: metric blocks and tiles, including locked ones. Use it for questions these metrics answer; list_periods gives the available period keys."
	periodsDescription      = "Returns the calendar periods of one type that have analytics data, with their coverage. Use it to choose period_key for get_analytics."
	presentDescription      = "Shows the user a completed read from a previous step as rows, a monthly chart or an analytics block; every figure comes from that saved result. Call it only after the read has returned, never in the same batch."
	cannotAnswerDescription = "Ends a personal-finance answer with empty arguments {} when the data already read cannot answer the question. Do not use it instead of reading data; questions outside personal finance use off_topic."
	offTopicDescription     = "End a question outside personal finance with empty arguments {}; takes priority over every other function. Financial questions with an impossible answer use cannot_answer instead."
)

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func builtinDefinition(name, description string, properties map[string]any, required ...string) ToolDefinition {
	parameters := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		parameters["required"] = required
	}
	raw, _ := json.Marshal(parameters)
	return ToolDefinition{Name: name, Description: description, Parameters: raw}
}

func periodSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"month", "quarter", "year"}, "description": "Period type; default month"}
}

func allTimeSchema() map[string]any {
	return map[string]any{"type": "boolean", "description": "Whole history; not allowed together with from or to"}
}

func filterSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"from":              stringProperty("Inclusive start, RFC 3339 with offset, e.g. 2026-09-01T00:00:00+03:00; earlier than to"),
		"to":                stringProperty("Exclusive end, RFC 3339 with offset; the period is [from, to)"),
		"q":                 stringProperty("Text search in operation descriptions"),
		"category_id":       stringProperty("Category identifier copied from a tool result"),
		"selection":         stringProperty("Opaque selection token (merchant, regular payment) copied from a tool result"),
		"kind":              map[string]any{"type": "string", "enum": []string{"expense", "income"}, "description": "Only expenses or only income; omit for both"},
		"amount_from_minor": map[string]any{"type": "integer", "minimum": 0, "description": "Minimum operation amount in minor currency units (kopecks for RUB)"},
		"amount_to_minor":   map[string]any{"type": "integer", "minimum": 0, "description": "Maximum operation amount in minor currency units; not below amount_from_minor"},
	}}
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
	definition := builtinDefinition("get_data_overview", overviewDescription, map[string]any{})
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
	definition := builtinDefinition("aggregate_operations", aggregateDescription, map[string]any{"filter": filterSchema(), "all_time": allTimeSchema(), "group_by": map[string]any{"type": "array", "maxItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"month", "category", "merchant"}}, "description": "Up to two grouping dimensions; omit for one total"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Groups to return, largest by absolute amount first; default 50"}})
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
	definition := builtinDefinition("find_operations", findDescription, map[string]any{"filter": filterSchema(), "all_time": allTimeSchema(), "order": map[string]any{"type": "string", "enum": []string{"newest", "largest"}, "description": "Newest first (default) or largest amount first"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Operations to return; default 20"}})
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
	definition := builtinDefinition("get_analytics", analyticsDescription, map[string]any{"period": periodSchema(), "period_key": stringProperty("Calendar key in the user's time zone: YYYY-MM, YYYY-Qn (quarter, year) or YYYY (year); a finer key selects the enclosing period. Omit for the latest period with data")})
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
	definition := builtinDefinition("list_periods", periodsDescription, map[string]any{"period": periodSchema()})
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
	d := builtinDefinition("off_topic", offTopicDescription, map[string]any{})
	return Tool{definition: d, priority: true, prepare: func(item ModelItem) (toolPlan, error) {
		return toolPlan{item: item, terminal: "OFF_TOPIC"}, nil
	}}
}

func cannotAnswerTool() Tool {
	return Tool{definition: builtinDefinition("cannot_answer", cannotAnswerDescription, map[string]any{}), prepare: func(item ModelItem) (toolPlan, error) {
		return toolPlan{item: item, terminal: "CANNOT_ANSWER"}, nil
	}}
}

func presentationTool() Tool {
	d := builtinDefinition("present_result", presentDescription, map[string]any{"result_id": stringProperty("call_id of a completed financial read from a previous step"), "view": map[string]any{"type": "string", "enum": []string{"rows", "chart", "analytics"}, "description": "rows: aggregate_operations or find_operations; chart: aggregate_operations grouped by month only, over full calendar months; analytics: get_analytics with path"}, "path": stringProperty("For analytics: existing blocks[i] or blocks[i].data.tiles[j]"), "coverage_result_id": stringProperty("For aggregate month charts: completed get_data_overview call_id with matching version and calendar coverage")}, "result_id", "view")
	return Tool{definition: d, prepare: func(item ModelItem) (toolPlan, error) {
		var args presentationArgs
		if json.Unmarshal([]byte(item.Arguments), &args) != nil || args.ResultID == "" || (args.View != "rows" && args.View != "chart" && args.View != "analytics") {
			return toolPlan{}, errModelProtocol
		}
		return toolPlan{item: item, presentation: &args}, nil
	}}
}
