package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type toolPlan struct {
	item             ModelItem
	args             any
	presentation     *presentationArgs
	required, cannot bool
	run              func(context.Context, tooldata.Data, tooldata.Subject) (tooldata.Result, error)
}

type filterArguments struct {
	From       *time.Time `json:"from"`
	To         *time.Time `json:"to"`
	Query      string     `json:"q"`
	CategoryID string     `json:"category_id"`
	Selection  string     `json:"selection"`
	Kind       string     `json:"kind"`
	AmountFrom *int64     `json:"amount_from_minor"`
	AmountTo   *int64     `json:"amount_to_minor"`
}

func (a filterArguments) domain() tooldata.Filter {
	return tooldata.Filter{From: a.From, To: a.To, Query: a.Query, CategoryID: a.CategoryID, Selection: a.Selection, Kind: a.Kind, AmountFrom: a.AmountFrom, AmountTo: a.AmountTo}
}

var filterFields = []string{"from", "to", "q", "category_id", "selection", "kind", "amount_from_minor", "amount_to_minor"}

func parseTool(item ModelItem) (toolPlan, error) {
	plan := toolPlan{item: item}
	raw := []byte(item.Arguments)
	switch item.Name {
	case "off_topic":
		var args struct{}
		if strictArguments(raw, &args, nil) != nil {
			return plan, errModelProtocol
		}
	case "present_result":
		var args presentationArgs
		if strictArguments(raw, &args, []string{"result_id", "view", "path", "coverage_result_id"}) != nil || args.ResultID == "" || (args.View != "rows" && args.View != "chart" && args.View != "analytics") {
			return plan, errModelProtocol
		}
		plan.presentation = &args
	case "get_data_overview", "cannot_answer":
		var args struct{}
		if strictArguments(raw, &args, nil) != nil {
			return plan, errModelProtocol
		}
		plan.cannot = item.Name == "cannot_answer"
		plan.run = func(ctx context.Context, data tooldata.Data, subject tooldata.Subject) (tooldata.Result, error) {
			return data.Overview(ctx, subject)
		}
	case "aggregate_operations":
		var args struct {
			Filter  filterArguments `json:"filter"`
			AllTime bool            `json:"all_time"`
			GroupBy []string        `json:"group_by"`
			Limit   int             `json:"limit"`
		}
		if strictArguments(raw, &args, []string{"filter", "all_time", "group_by", "limit"}) != nil || !validFilter(args.Filter, args.AllTime) || args.Limit < 0 || args.Limit > 200 || len(args.GroupBy) > 2 {
			return plan, errModelProtocol
		}
		seen := map[string]bool{}
		for _, group := range args.GroupBy {
			if seen[group] || (group != "month" && group != "category" && group != "merchant") {
				return plan, errModelProtocol
			}
			seen[group] = true
		}
		plan.required = true
		plan.args = tooldata.AggregateArgs{Filter: args.Filter.domain(), AllTime: args.AllTime, GroupBy: args.GroupBy, Limit: args.Limit}
		plan.run = func(ctx context.Context, data tooldata.Data, subject tooldata.Subject) (tooldata.Result, error) {
			return data.Aggregate(ctx, subject, plan.args.(tooldata.AggregateArgs))
		}
	case "find_operations":
		var args struct {
			Filter  filterArguments `json:"filter"`
			AllTime bool            `json:"all_time"`
			Order   string          `json:"order"`
			Limit   int             `json:"limit"`
		}
		if strictArguments(raw, &args, []string{"filter", "all_time", "order", "limit"}) != nil || !validFilter(args.Filter, args.AllTime) || args.Limit < 0 || args.Limit > 100 || (args.Order != "" && args.Order != "newest" && args.Order != "largest") {
			return plan, errModelProtocol
		}
		plan.required = true
		plan.args = tooldata.FindArgs{Filter: args.Filter.domain(), AllTime: args.AllTime, Order: args.Order, Limit: args.Limit}
		plan.run = func(ctx context.Context, data tooldata.Data, subject tooldata.Subject) (tooldata.Result, error) {
			return data.Find(ctx, subject, plan.args.(tooldata.FindArgs))
		}
	case "get_analytics":
		var args struct {
			Period    string `json:"period"`
			PeriodKey string `json:"period_key"`
		}
		if strictArguments(raw, &args, []string{"period", "period_key"}) != nil || !validPeriod(args.Period, args.PeriodKey) {
			return plan, errModelProtocol
		}
		plan.required = true
		plan.args = tooldata.AnalyticsArgs{Period: args.Period, PeriodKey: args.PeriodKey}
		plan.run = func(ctx context.Context, data tooldata.Data, subject tooldata.Subject) (tooldata.Result, error) {
			return data.Analytics(ctx, subject, plan.args.(tooldata.AnalyticsArgs))
		}
	case "list_periods":
		var args struct {
			Period string `json:"period"`
		}
		if strictArguments(raw, &args, []string{"period"}) != nil || !validPeriod(args.Period, "") {
			return plan, errModelProtocol
		}
		plan.args = tooldata.PeriodsArgs{Period: args.Period}
		plan.run = func(ctx context.Context, data tooldata.Data, subject tooldata.Subject) (tooldata.Result, error) {
			return data.Periods(ctx, subject, plan.args.(tooldata.PeriodsArgs))
		}
	default:
		return plan, errModelProtocol
	}
	return plan, nil
}

func validFilter(f filterArguments, allTime bool) bool {
	if allTime && (f.From != nil || f.To != nil) {
		return false
	}
	for _, at := range []*time.Time{f.From, f.To} {
		if at != nil && (at.IsZero() || at.Year() < 1 || at.Year() > 9999) {
			return false
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return false
	}
	if f.AmountFrom != nil && *f.AmountFrom < 0 || f.AmountTo != nil && *f.AmountTo < 0 || f.AmountFrom != nil && f.AmountTo != nil && *f.AmountFrom > *f.AmountTo {
		return false
	}
	return f.Kind == "" || f.Kind == "expense" || f.Kind == "income"
}
func validPeriod(period, key string) bool {
	if period == "" {
		period = "month"
	}
	if period != "month" && period != "quarter" && period != "year" {
		return false
	}
	if key == "" {
		return true
	}
	if (period == "month" || period == "quarter" || period == "year") && len(key) == 7 && key[4] == '-' {
		if at, err := time.Parse("2006-01", key); err == nil && at.Year() > 0 {
			return true
		}
	}
	if period != "month" && len(key) == 7 && key[4:6] == "-Q" && key[6] >= '1' && key[6] <= '4' {
		if at, err := time.Parse("2006", key[:4]); err == nil && at.Year() > 0 {
			return true
		}
	}
	if period == "year" && len(key) == 4 {
		if at, err := time.Parse("2006", key); err == nil && at.Year() > 0 {
			return true
		}
	}
	return false
}

func strictArguments(raw []byte, target any, fields []string) error {
	if !validObject(raw) {
		return errModelProtocol
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return errModelProtocol
	}
	allowed := map[string]bool{}
	for _, name := range fields {
		allowed[name] = true
	}
	for name, value := range object {
		if !allowed[name] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errModelProtocol
		}
		if name == "filter" {
			var filter filterArguments
			if strictArguments(value, &filter, filterFields) != nil {
				return errModelProtocol
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return errModelProtocol
	}
	return nil
}
func validObject(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if jsonValue(d) != nil {
		return false
	}
	_, err := d.Token()
	return errors.Is(err, io.EOF)
}
func jsonValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errModelProtocol
			}
			keys[name] = true
			if err := jsonValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := jsonValue(d); err != nil {
				return err
			}
		}
	default:
		return errModelProtocol
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if delimiter == '{' && end != json.Delim('}') || delimiter == '[' && end != json.Delim(']') {
		return errModelProtocol
	}
	return nil
}

func validateOutput(items []ModelItem) ([]ModelItem, int, string, error) {
	var calls []ModelItem
	searches := 0
	var text strings.Builder
	ids := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || ids[item.ID] || !validObject(item.Raw) {
			return nil, 0, "", errModelProtocol
		}
		ids[item.ID] = true
		var wire struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Role      string `json:"role"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(item.Raw, &wire) != nil || wire.ID != item.ID || wire.Type != item.Type {
			return nil, 0, "", errModelProtocol
		}
		switch item.Type {
		case "function_call":
			if item.CallID == "" || item.Name == "" || wire.CallID != item.CallID || wire.Name != item.Name || wire.Arguments != item.Arguments {
				return nil, 0, "", errModelProtocol
			}
			calls = append(calls, item)
		case "message":
			if wire.Role != "assistant" {
				return nil, 0, "", errModelProtocol
			}
			for _, part := range wire.Content {
				if part.Type == "refusal" {
					return nil, 0, "", ErrRefused
				}
				if part.Type != "output_text" {
					return nil, 0, "", errModelProtocol
				}
				text.WriteString(part.Text)
			}
		case "reasoning":
		case "web_search_call":
			searches++
		default:
			return nil, 0, "", errModelProtocol
		}
	}
	return calls, searches, text.String(), nil
}

func toolDefinitions(searchRemaining int) []json.RawMessage {
	stringProperty := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	filter := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"from": stringProperty("Inclusive RFC3339 start"), "to": stringProperty("Exclusive RFC3339 end"), "q": stringProperty("Description search"), "category_id": stringProperty("Category identifier from trusted data"), "selection": stringProperty("Opaque token from trusted data"), "kind": map[string]any{"type": "string", "enum": []string{"expense", "income"}}, "amount_from_minor": map[string]any{"type": "integer", "minimum": 0}, "amount_to_minor": map[string]any{"type": "integer", "minimum": 0}}}
	properties := map[string]map[string]any{
		"get_data_overview": {}, "cannot_answer": {}, "off_topic": {},
		"aggregate_operations": {"filter": filter, "all_time": map[string]any{"type": "boolean"}, "group_by": map[string]any{"type": "array", "maxItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"month", "category", "merchant"}}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}},
		"find_operations":      {"filter": filter, "all_time": map[string]any{"type": "boolean"}, "order": map[string]any{"type": "string", "enum": []string{"newest", "largest"}}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}},
		"get_analytics":        {"period": map[string]any{"type": "string", "enum": []string{"month", "quarter", "year"}}, "period_key": stringProperty("Calendar key: YYYY-MM, YYYY-Qn, YYYY")},
		"list_periods":         {"period": map[string]any{"type": "string", "enum": []string{"month", "quarter", "year"}}},
		"present_result":       {"result_id": stringProperty("call_id of a completed financial read from a previous step"), "view": map[string]any{"type": "string", "enum": []string{"rows", "chart", "analytics"}}, "path": stringProperty("For analytics: existing blocks[i] or blocks[i].data.tiles[j]"), "coverage_result_id": stringProperty("For aggregate month charts: completed get_data_overview call_id with matching version and calendar coverage")},
	}
	var result []json.RawMessage
	for _, name := range []string{"get_data_overview", "aggregate_operations", "find_operations", "get_analytics", "list_periods", "off_topic", "present_result", "cannot_answer"} {
		parameters := map[string]any{"type": "object", "additionalProperties": false, "properties": properties[name]}
		if name == "present_result" {
			parameters["required"] = []string{"result_id", "view"}
		}
		description := "Read verified financial data; present_result renders trusted figures and navigation from a completed read, never accepts invented values; cannot_answer ends an impossible answer."
		if name == "off_topic" {
			description = "End a question outside personal finance with empty arguments {}; takes priority over every other function. Financial questions with an impossible answer use cannot_answer instead."
		}
		raw, _ := json.Marshal(map[string]any{"type": "function", "name": name, "description": description, "parameters": parameters})
		result = append(result, raw)
	}
	if searchRemaining > 0 {
		result = append(result, json.RawMessage(`{"type":"web_search"}`))
	}
	return result
}
