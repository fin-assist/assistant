package generation

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	grpcdialogue "github.com/fin-assist/assistant/internal/adapter/grpc/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func presentationJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func presentationRef(t *testing.T, name string, args any, body any) ToolResultReference {
	t.Helper()
	return ToolResultReference{CallID: "read", Name: name, Args: args, Result: presentationFixture(name, presentationJSON(t, body))}
}

func presentationRun(t *testing.T, ref ToolResultReference, args presentationArgs) presentationResult {
	t.Helper()
	if args.ResultID == "" {
		args.ResultID = ref.CallID
	}
	out, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, args, map[string]ToolResultReference{ref.CallID: ref})
	if err != nil {
		t.Fatal(err)
	}
	presentationTransport(t, out)
	return out
}

func presentationTransport(t *testing.T, out presentationResult) {
	t.Helper()
	if len(out.Blocks) == 0 {
		t.Fatal("presentation has no blocks")
	}
	if _, err := grpcdialogue.Blocks(out.Blocks); err != nil {
		t.Fatalf("blocks rejected by public mapper: %v; %s", err, out.Blocks[0].Body)
	}
	if _, err := grpcdialogue.Chips(out.Chips); err != nil {
		t.Fatalf("chips rejected by public mapper: %v; %s", err, out.Chips)
	}
}

func presentationSavedRows(t *testing.T, out presentationResult) []presentationRow {
	t.Helper()
	var body struct {
		Data struct {
			Rows []presentationRow `json:"rows"`
		} `json:"data"`
	}
	if len(out.Blocks) != 1 || out.Blocks[0].Type != "rows" {
		t.Fatalf("expected one rows block: %+v", out)
	}
	decoder := json.NewDecoder(strings.NewReader(string(out.Blocks[0].Body)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Data.Rows
}

func TestUnitPresentationAggregateUsesReturnedTotalsAndFilters(t *testing.T) {
	ref := presentationRef(t, "aggregate_operations", tooldata.AggregateArgs{GroupBy: []string{"category"}}, map[string]any{
		"total_amount_minor": "9007199254740991", "total_operation_count": "4000", "truncated": true,
		"groups": []any{map[string]any{
			"keys":         []any{map[string]any{"category": map[string]any{"id": "food-id", "name": "Продукты"}}},
			"amount_minor": "12345", "operation_count": "2", "average_minor": "6173",
			"filters": map[string]any{"category_id": "food-id", "amount_from_minor": "0", "transfer_mode": "with"},
		}},
	})
	out := presentationRun(t, ref, presentationArgs{View: "rows"})
	rows := presentationSavedRows(t, out)
	if len(rows) != 6 || rows[0].Value != json.Number("9007199254740991") || rows[1].Value != json.Number("4000") || rows[4].Value != json.Number("6173") || rows[5].Text != "Показана только часть групп" {
		t.Fatalf("totals/average recomputed or truncated warning lost: %+v", rows)
	}
	if out.Incomplete || len(out.Chips) != 1 || !strings.Contains(string(out.Chips[0]), `"category_id":"food-id"`) || !strings.Contains(string(out.Chips[0]), `"amount_from":0`) || strings.Contains(string(out.Chips[0]), "900719925") {
		t.Fatalf("unexpected chip/charge policy: %+v", out)
	}
}

func TestUnitPresentationTypedIntegerDefaultsAndExactFallback(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"zero omitted", "", ""},
		{"positive overflow", "9007199254740992", "90071992547409,92 ₽"},
		{"negative overflow", "-9007199254740992", "-90071992547409,92 ₽"},
		{"minimum int64", "-9223372036854775808", "-92233720368547758,08 ₽"},
		{"maximum int64", "9223372036854775807", "92233720368547758,07 ₽"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			if tc.value != "" {
				body["total_amount_minor"] = tc.value
			}
			ref := presentationRef(t, "aggregate_operations", tooldata.AggregateArgs{}, body)
			out := presentationRun(t, ref, presentationArgs{View: "rows"})
			rows := presentationSavedRows(t, out)
			if tc.want != "" {
				if rows[0].Text != tc.want || rows[0].Value != nil || rows[0].Kind != "" {
					t.Fatalf("rounded value: %+v", rows[0])
				}
			} else if rows[0].Value != json.Number("0") || rows[1].Value != json.Number("0") {
				t.Fatalf("missing protobuf scalar is not zero: %+v", rows)
			}
		})
	}
	for _, n := range []json.Number{"1.5", "9223372036854775808", "1e2", "NaN"} {
		if _, err := presentationInt(n); !errors.Is(err, errModelProtocol) {
			t.Fatalf("accepted invalid native integer %q: %v", n, err)
		}
	}
}

func TestUnitPresentationOperationRowsDoNotSumLimitedPage(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ref := presentationRef(t, "find_operations", tooldata.FindArgs{Filter: tooldata.Filter{Query: "магазин", Kind: "expense"}}, map[string]any{
		"total_count": "100", "time_zone": "Europe/Moscow", "range": map[string]any{"start": start, "end": end},
		"items": []any{
			map[string]any{"title": "Магазин", "kind": "expense", "amount_minor": "12345", "currency": "RUB", "occurred_at": "2026-05-01T22:00:00Z"},
			map[string]any{"title": "Возврат", "kind": "income", "amount_minor": "750", "currency": "USD", "occurred_at": "2026-05-03T12:00:00Z"},
		},
	})
	out := presentationRun(t, ref, presentationArgs{View: "rows"})
	rows := presentationSavedRows(t, out)
	if len(rows) != 4 || rows[0].Value != json.Number("100") || rows[1].Label != "Расход: Магазин" || rows[1].Sublabel != "02.05.2026" || rows[1].Value != json.Number("12345") || rows[2].Text != "7,50 USD" || rows[2].Value != nil || rows[3].Text != "Показана только часть операций" {
		t.Fatalf("invalid operations rows: %+v", rows)
	}
	if len(out.Chips) != 1 || !strings.Contains(string(out.Chips[0]), `"q":"магазин"`) || !strings.Contains(string(out.Chips[0]), `"from":"2026-05-01T00:00:00Z"`) || !strings.Contains(string(out.Chips[0]), `"transfer_mode":"with"`) {
		t.Fatalf("filter does not preserve validated request and actual range: %s", out.Chips)
	}
}

func TestUnitPresentationNoDataDoesNotInventZeros(t *testing.T) {
	for _, name := range []string{"aggregate_operations", "find_operations"} {
		ref := ToolResultReference{CallID: "read", Name: name, Result: tooldata.Result{Evidence: tooldata.Evidence{NoData: true}}}
		out := presentationRun(t, ref, presentationArgs{View: "rows"})
		if !out.Incomplete || out.Blocks[0].Type != "text" || len(out.Chips) != 0 {
			t.Fatalf("missing data became financial result: %+v", out)
		}
	}
}

func presentationMonthFixture(t *testing.T, zoneName string, start time.Time) (ToolResultReference, ToolResultReference, map[string]any, map[string]any) {
	t.Helper()
	version := start.Add(-time.Hour)
	source := func() *tooldata.Source {
		return &tooldata.Source{TimeZone: zoneName, Version: tooldata.Version{CategoriesChangedAt: &version, UploadsChangedAt: &version}}
	}
	var months []any
	var groups []any
	for i := range 3 {
		a, b := start.AddDate(0, i, 0), start.AddDate(0, i+1, 0)
		key := a.Format("2006-01")
		months = append(months, map[string]any{"month": key, "range": map[string]any{"start": a, "end": b}, "bounds": map[string]any{"coverage": "complete", "data_from": a, "data_to": b.Add(-time.Second)}})
		if i != 1 {
			groups = append([]any{map[string]any{"keys": []any{map[string]any{"month": key}}, "amount_minor": "12345", "operation_count": "2", "average_minor": "6173", "filters": map[string]any{"from": a, "to": b, "transfer_mode": "with"}}}, groups...)
		}
	}
	aggregate := map[string]any{"time_zone": zoneName, "range": map[string]any{"start": start, "end": start.AddDate(0, 3, 0)}, "groups": groups, "total_amount_minor": "24690", "total_operation_count": "4"}
	overview := map[string]any{"time_zone": zoneName, "months": months}
	a := presentationRef(t, "aggregate_operations", tooldata.AggregateArgs{GroupBy: []string{"month"}}, aggregate)
	a.Result.Evidence.Source = source()
	b := presentationRef(t, "get_data_overview", nil, overview)
	b.CallID = "coverage"
	b.Result.Evidence.Source = source()
	return a, b, aggregate, overview
}

func presentationChartRun(t *testing.T, a, b ToolResultReference) (presentationResult, error) {
	t.Helper()
	return preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", CoverageResultID: "coverage", View: "chart"}, map[string]ToolResultReference{"read": a, "coverage": b})
}

func TestUnitPresentationMonthlyChartUsesExactCoverageAndOmitsAbsentGroups(t *testing.T) {
	for _, zoneName := range []string{"Europe/Moscow", "Europe/Berlin"} {
		t.Run(zoneName, func(t *testing.T) {
			zone, err := time.LoadLocation(zoneName)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 3, 1, 0, 0, 0, 0, zone)
			a, b, body, overview := presentationMonthFixture(t, zoneName, start)
			body["truncated"] = true
			a.Result.Aggregate = presentationFixture(a.Name, presentationJSON(t, body)).Aggregate
			bounds := overview["months"].([]any)[0].(map[string]any)["bounds"].(map[string]any)
			bounds["coverage"] = "partial"
			bounds["data_from"] = start.AddDate(0, 0, 2)
			bounds["gaps"] = []any{map[string]any{"start": start.AddDate(0, 0, 10), "end": start.AddDate(0, 0, 11)}}
			b.Result.Overview = presentationFixture(b.Name, presentationJSON(t, overview)).Overview
			out, err := presentationChartRun(t, a, b)
			if err != nil {
				t.Fatal(err)
			}
			presentationTransport(t, out)
			var envelope struct {
				Alt  string `json:"alt_text"`
				Data struct {
					Points []presentationPoint `json:"points"`
				} `json:"data"`
			}
			if err := json.Unmarshal(out.Blocks[0].Body, &envelope); err != nil {
				t.Fatal(err)
			}
			points := envelope.Data.Points
			if len(points) != 2 || !points[0].Range.From.Equal(start) || !points[0].Range.To.Equal(start.AddDate(0, 1, 0)) || !points[1].Range.From.Equal(start.AddDate(0, 2, 0)) || *points[0].Value != 12345 || points[0].Coverage != "partial" || !points[0].DataFrom.Equal(start.AddDate(0, 0, 2)) || len(out.Chips) != 2 || out.Incomplete {
				t.Fatalf("invented missing month or lost source bounds: %+v", out)
			}
			if !strings.Contains(envelope.Alt, "данные частично") || !strings.Contains(envelope.Alt, "нет данных с") || !strings.Contains(envelope.Alt, "Показана только часть месяцев") {
				t.Fatalf("coverage/truncation detail lost: %s", envelope.Alt)
			}
			if len(out.Blocks) != 2 || out.Blocks[1].Type != "text" || !strings.Contains(out.Blocks[1].Text, "Показана только часть месяцев") || !strings.Contains(out.Blocks[1].Text, "нет данных с") {
				t.Fatalf("limitations only available to accessibility fallback: %+v", out.Blocks)
			}
		})
	}
}

func TestUnitPresentationMonthlyChartRejectsUnprovenCoverage(t *testing.T) {
	for _, mutation := range []string{"missing ref", "wrong ref name", "different version", "missing version", "different zone", "unknown zone", "partial calendar", "missing month", "different month range", "no coverage", "bad coverage bounds", "duplicate month", "wrong group", "duplicate group", "category grouping", "no range"} {
		t.Run(mutation, func(t *testing.T) {
			a, b, aggregate, overview := presentationMonthFixture(t, "UTC", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
			switch mutation {
			case "missing ref":
				b.CallID = "absent"
			case "wrong ref name":
				b.Name = "get_analytics"
			case "different version":
				at := b.Result.Evidence.Source.Version.UploadsChangedAt.Add(time.Second)
				b.Result.Evidence.Source.Version.UploadsChangedAt = &at
			case "missing version":
				b.Result.Evidence.Source.Version.UploadsChangedAt = nil
			case "different zone":
				b.Result.Evidence.Source.TimeZone = "Europe/Moscow"
			case "unknown zone":
				a.Result.Evidence.Source.TimeZone = "Nowhere/Unknown"
				b.Result.Evidence.Source.TimeZone = "Nowhere/Unknown"
				aggregate["time_zone"] = "Nowhere/Unknown"
				overview["time_zone"] = "Nowhere/Unknown"
			case "partial calendar":
				aggregate["range"].(map[string]any)["start"] = "2026-03-02T00:00:00Z"
			case "missing month":
				overview["months"] = overview["months"].([]any)[:2]
			case "different month range":
				overview["months"].([]any)[0].(map[string]any)["range"].(map[string]any)["end"] = "2026-05-01T00:00:00Z"
			case "no coverage":
				overview["months"].([]any)[0].(map[string]any)["bounds"].(map[string]any)["coverage"] = "no_data"
			case "bad coverage bounds":
				delete(overview["months"].([]any)[0].(map[string]any)["bounds"].(map[string]any), "data_to")
			case "duplicate month":
				overview["months"] = append(overview["months"].([]any), overview["months"].([]any)[0])
			case "wrong group":
				aggregate["groups"].([]any)[0].(map[string]any)["keys"] = []any{map[string]any{"month": "2026-13"}}
			case "duplicate group":
				aggregate["groups"] = append(aggregate["groups"].([]any), aggregate["groups"].([]any)[0])
			case "category grouping":
				a.Args = tooldata.AggregateArgs{GroupBy: []string{"category"}}
			case "no range":
				delete(aggregate, "range")
			}
			a.Result.Aggregate, b.Result.Overview = presentationFixture(a.Name, presentationJSON(t, aggregate)).Aggregate, presentationFixture(b.Name, presentationJSON(t, overview)).Overview
			if _, err := presentationChartRun(t, a, b); !errors.Is(err, errModelProtocol) {
				t.Fatalf("accepted unproven chart: %v", err)
			}
		})
	}
}

func TestUnitPresentationMonthlyChartOverflowFallsBackToExactRows(t *testing.T) {
	a, b, aggregate, _ := presentationMonthFixture(t, "UTC", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	aggregate["groups"].([]any)[0].(map[string]any)["amount_minor"] = "9007199254740992"
	a.Result.Aggregate = presentationFixture(a.Name, presentationJSON(t, aggregate)).Aggregate
	out, err := presentationChartRun(t, a, b)
	if err != nil {
		t.Fatal(err)
	}
	presentationTransport(t, out)
	rows := presentationSavedRows(t, out)
	if rows[2].Text != "90071992547409,92 ₽" {
		t.Fatalf("chart overflow rounded: %+v", rows)
	}
}

func presentationAnalyticsFixture(t *testing.T, blocks []any) ToolResultReference {
	t.Helper()
	return presentationRef(t, "get_analytics", tooldata.AnalyticsArgs{Period: "month", PeriodKey: "2026-10"}, map[string]any{
		"has_data": true, "currency": "RUB", "params": map[string]any{"period": "month", "period_key": "2026-10", "transfer_mode": "with"}, "blocks": blocks,
	})
}

func TestUnitPresentationSelectedAnalyticsTileControlsIncomplete(t *testing.T) {
	ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "tile_group", "v": 1, "status": "ready", "alt_text": "Расходы и прогноз", "data": map[string]any{"tiles": []any{
		map[string]any{"title": "Расходы", "status": "ready", "value": 12345, "value_kind": "amount", "caption": "За октябрь"},
		map[string]any{"title": "Прогноз", "status": "locked", "lock": map[string]any{"text": "Нужно 3 полных месяца"}},
	}}}})
	ref.Result.Evidence.LockedPaths = []string{"blocks[0].data.tiles[1]"}
	out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[0]"})
	rows := presentationSavedRows(t, out)
	if out.Incomplete || len(rows) != 1 || rows[0].Value != json.Number("12345") || rows[0].Sublabel != "За октябрь" || len(out.Chips) != 1 {
		t.Fatalf("unselected lock affected result: %+v", out)
	}
	locked := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[1]"})
	if !locked.Incomplete || locked.Blocks[0].Type != "text" || locked.Blocks[0].Text != "Нужно 3 полных месяца" {
		t.Fatalf("locked result invented numbers or lost server reason: %+v", locked)
	}
	ref.Result.Evidence.LockedPaths = nil
	if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: "analytics", Path: "blocks[0].data.tiles[1]"}, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
		t.Fatalf("untrusted lock path accepted: %v", err)
	}
}

func TestUnitPresentationAnalyticsRowsAndFutureFallback(t *testing.T) {
	for _, tc := range []struct {
		name               string
		block              map[string]any
		wantType, wantText string
	}{
		{"text", map[string]any{"type": "text", "v": 1, "data": map[string]any{"text": "Серверное пояснение"}}, "text", "Серверное пояснение"},
		{"future type", map[string]any{"type": "new_widget", "v": 1, "alt_text": "Новый показатель: 42"}, "text", "Новый показатель: 42"},
		{"future version", map[string]any{"type": "rows", "v": 2, "alt_text": "Серверная сводка"}, "text", "Серверная сводка"},
		{"unavailable", map[string]any{"type": "rows", "v": 1, "status": "unavailable", "empty_text": "Пока нечего сравнить"}, "text", "Пока нечего сравнить"},
		{"rows", map[string]any{"type": "rows", "v": 1, "data": map[string]any{"rows": []any{
			map[string]any{"label": "Итог", "value": 12345, "value_kind": "amount"},
			map[string]any{"label": "Операций", "value": 3, "value_kind": "count"},
			map[string]any{"label": "Доля", "value": 0.25, "value_kind": "percent"},
			map[string]any{"label": "Изменение", "value": -0.5, "value_kind": "percent"},
			map[string]any{"label": "Комментарий", "text": "Данных достаточно"},
		}}}, "rows", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := presentationAnalyticsFixture(t, []any{tc.block})
			out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0]"})
			if out.Incomplete || out.Blocks[0].Type != tc.wantType || out.Blocks[0].Text != tc.wantText {
				t.Fatalf("unexpected result: %+v", out)
			}
			if tc.name == "rows" {
				rows := presentationSavedRows(t, out)
				if len(rows) != 5 || rows[2].Value != json.Number("0.25") || rows[3].Text != "-0.5" || rows[4].Text != "Данных достаточно" {
					t.Fatalf("changed catalog values: %+v", rows)
				}
			}
		})
	}
}

func TestUnitPresentationAnalyticsChartAndUnsupportedFeatures(t *testing.T) {
	for _, feature := range []string{"", "note", "summary", "caption", "notes", "show_values", "future kind", "future unit", "overflow"} {
		t.Run(feature, func(t *testing.T) {
			point := map[string]any{"range": map[string]any{"from": "2026-10-01T00:00:00Z", "to": "2026-11-01T00:00:00Z"}, "value": 12345, "coverage": "partial", "data_from": "2026-10-05T00:00:00Z", "data_to": "2026-10-20T12:00:00Z"}
			data := map[string]any{"kind": "bar", "value_kind": "amount", "points": []any{point}}
			switch feature {
			case "note":
				point["note"] = "Возврат"
			case "summary":
				data["summary"] = map[string]any{"value": 12345}
			case "caption":
				data["caption"] = "Часть месяца"
			case "notes":
				data["notes"] = []any{"Возврат"}
			case "show_values":
				data["show_values"] = false
			case "future kind":
				data["kind"] = "line"
			case "future unit":
				data["value_kind"] = "count"
			case "overflow":
				point["value"] = json.Number("9007199254740992")
			}
			ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "chart", "v": 1, "title": "Расходы", "alt_text": "123,45 ₽ за часть октября", "data": data}})
			out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0]"})
			if feature == "" {
				if out.Blocks[0].Type != "chart" || !strings.Contains(string(out.Blocks[0].Body), `"data_to":"2026-10-20T12:00:00Z"`) {
					t.Fatalf("catalog chart changed: %+v", out)
				}
			} else if out.Blocks[0].Type != "text" || out.Blocks[0].Text != "123,45 ₽ за часть октября" {
				t.Fatalf("unsupported detail silently lost: %+v", out)
			}
		})
	}
}

func TestUnitPresentationRejectsUnknownViewsRefsAndPaths(t *testing.T) {
	ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "text", "v": 1, "data": map[string]any{"text": "OK"}}})
	for _, args := range []presentationArgs{
		{ResultID: "missing", View: "analytics", Path: "blocks[0]"},
		{ResultID: "read", View: "model_written"},
		{ResultID: "read", View: "chart"},
		{ResultID: "read", View: "rows"},
		{ResultID: "read", View: "analytics"},
		{ResultID: "read", View: "analytics", Path: "blocks[-1]"},
		{ResultID: "read", View: "analytics", Path: "blocks[1]"},
		{ResultID: "read", View: "analytics", Path: "blocks[0].data.value"},
		{ResultID: "read", View: "analytics", Path: "blocks[0].data.tiles[0]"},
		{ResultID: "read", View: "analytics", Path: "blocks[0]", CoverageResultID: "other"},
	} {
		if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, args, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
			t.Fatalf("accepted %+v: %v", args, err)
		}
	}
}

func TestUnitPresentationChipsRequireTrustedMatchingModeAndNormalizedPeriods(t *testing.T) {
	task := domain.GenerationTask{TransferMode: "without"}
	for _, tc := range []struct{ period, key string }{{"month", "2026-10"}, {"quarter", "2026-Q4"}, {"year", "2026"}} {
		chip, err := presentationAnalyticsChip(task, tc.period, tc.key, "without")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := grpcdialogue.Chips([]json.RawMessage{chip}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ period, key, mode string }{{"month", "2026-13", "without"}, {"quarter", "2026-10", "without"}, {"year", "0000", "without"}, {"month", "2026-10", "with"}, {"future", "2026", "without"}} {
		if _, err := presentationAnalyticsChip(task, tc.period, tc.key, tc.mode); !errors.Is(err, errModelProtocol) {
			t.Fatalf("invalid params accepted: %+v: %v", tc, err)
		}
	}
	if _, err := presentationNativeChip(task, "Операции", &presentationFilter{Q: "x", Mode: "with"}); !errors.Is(err, errModelProtocol) {
		t.Fatal("chip changed task mode")
	}
	chip, err := presentationNativeChip(task, "Операции", &presentationFilter{Mode: "without"})
	if err != nil || chip != nil {
		t.Fatalf("chip without effective filter: %s %v", chip, err)
	}
	min := int64(1)
	max := int64(0)
	if _, err := presentationNativeChip(task, "Операции", &presentationFilter{Mode: "without", AmountFrom: &min, AmountTo: &max}); !errors.Is(err, errModelProtocol) {
		t.Fatal("accepted inverted filter")
	}
}

func TestUnitPresentationCatalogRowIntegerAndForeignCurrency(t *testing.T) {
	for _, tc := range []struct{ value, kind, currency, text string }{
		{"9007199254740992", "count", "RUB", "9007199254740992"},
		{"12345", "amount", "USD", "123,45 USD"},
	} {
		v := json.Number(tc.value)
		row, err := presentationCatalogRow("Показатель", "", &v, tc.kind, "", tc.currency)
		if err != nil || row.Value != nil || row.Text != tc.text {
			t.Fatalf("exact fallback lost: %+v %v", row, err)
		}
	}
	if presentationDisplay(math.MinInt64, "count") != "-9223372036854775808" {
		t.Fatal("minimum integer overflow")
	}
}

func TestUnitPresentationAggregateMultipleDimensionsAndInvalidFacts(t *testing.T) {
	ref := presentationRef(t, "aggregate_operations", tooldata.AggregateArgs{GroupBy: []string{"merchant", "month"}}, map[string]any{
		"groups": []any{map[string]any{"keys": []any{map[string]any{"merchant": map[string]any{"title": "Магазин", "selection": "s_merchant_shop"}}, map[string]any{"month": "2026-10"}}, "filters": map[string]any{"selection": "s_merchant_shop", "selection_name": "Магазин", "transfer_mode": "with"}}},
	})
	out := presentationRun(t, ref, presentationArgs{View: "rows"})
	rows := presentationSavedRows(t, out)
	if rows[2].Label != "Магазин · 2026-10" || rows[2].Value != json.Number("0") || !strings.Contains(string(out.Chips[0]), `"selection":"s_merchant_shop"`) {
		t.Fatalf("multi-dimensional group lost trusted selection: %+v", out)
	}
	monthKey := tooldata.GroupKey{Month: "2026-10"}
	for _, facts := range []*tooldata.Aggregate{
		nil,
		{Count: -1},
		{Groups: []tooldata.Group{{Keys: nil}}},
		{Groups: []tooldata.Group{{Keys: []tooldata.GroupKey{monthKey}, Count: -1}}},
		{Groups: []tooldata.Group{{Keys: []tooldata.GroupKey{monthKey}}}},
		{Groups: []tooldata.Group{{Keys: []tooldata.GroupKey{monthKey}, Filters: &tooldata.NavigationFilter{Mode: "without"}}}},
		{Groups: []tooldata.Group{{Keys: []tooldata.GroupKey{{Month: "2026-10", Category: &tooldata.CategoryKey{ID: "x", Name: "x"}}}}}},
	} {
		ref.Args = tooldata.AggregateArgs{GroupBy: []string{"month"}}
		ref.Result.Aggregate = facts
		if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: "rows"}, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
			t.Fatalf("inconsistent facts accepted: %+v: %v", facts, err)
		}
	}
}

func TestUnitPresentationRejectsMalformedOperationRows(t *testing.T) {
	for _, mutation := range []string{"invalid count", "zone", "title", "kind", "amount", "currency", "time", "range"} {
		t.Run(mutation, func(t *testing.T) {
			item := map[string]any{"title": "Операция", "kind": "own_transfer", "amount_minor": "12345", "currency": "RUB", "occurred_at": "2026-10-01T12:00:00Z"}
			body := map[string]any{"total_count": "1", "time_zone": "UTC", "items": []any{item}}
			switch mutation {
			case "invalid count":
				body["total_count"] = "0"
			case "zone":
				body["time_zone"] = ""
			case "title":
				item["title"] = ""
			case "kind":
				item["kind"] = "future"
			case "amount":
				item["amount_minor"] = "-1"
			case "currency":
				item["currency"] = ""
			case "time":
				delete(item, "occurred_at")
			case "range":
				body["range"] = map[string]any{"start": "2026-10-02T00:00:00Z", "end": "2026-10-01T00:00:00Z"}
			}
			ref := presentationRef(t, "find_operations", tooldata.FindArgs{}, body)
			if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: "rows"}, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
				t.Fatalf("accepted invalid list: %v", err)
			}
		})
	}
}

func TestUnitPresentationSelectedTilePreservesApproximationAndSign(t *testing.T) {
	for _, tc := range []struct {
		value      any
		kind, want string
	}{
		{12345, "amount", "≈+123,45 ₽"},
		{-12345, "amount", "≈-123,45 ₽"},
		{0, "amount", "≈0,00 ₽"},
		{0.25, "percent", "≈+0.25 (доля)"},
	} {
		ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "tile_group", "v": 1, "data": map[string]any{"tiles": []any{map[string]any{"title": "Прогноз", "status": "tentative", "value": tc.value, "value_kind": tc.kind, "approx": true, "show_plus": true}}}}})
		out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[0]"})
		rows := presentationSavedRows(t, out)
		if rows[0].Value != nil || rows[0].Text != tc.want || rows[0].Label != "Прогноз (предварительно)" {
			t.Fatalf("forecast became exact: %+v", rows)
		}
	}
}

func TestUnitPresentationWholeBlockLocksAndUnavailableTiles(t *testing.T) {
	ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "chart", "v": 1, "status": "locked"}})
	ref.Result.Evidence.LockedPaths = []string{"blocks[0]"}
	out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0]"})
	if !out.Incomplete || out.Blocks[0].Text != "Пока недоступно" {
		t.Fatalf("missing lock fallback: %+v", out)
	}
	for _, status := range []string{"none", "unavailable"} {
		ref = presentationAnalyticsFixture(t, []any{map[string]any{"type": "tile_group", "v": 1, "data": map[string]any{"tiles": []any{map[string]any{"title": "Прогноз", "status": status}}}}})
		out = presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[0]"})
		if out.Incomplete || out.Blocks[0].Text != "Показатель пока недоступен" {
			t.Fatalf("unavailable became selected lock: %+v", out)
		}
	}
}

func TestUnitPresentationAnalyticsRejectsMalformedCharts(t *testing.T) {
	for _, mutation := range []string{"highlight", "range", "bounds", "order", "coverage", "no_data_value", "fraction_amount", "percent", "no_alt"} {
		t.Run(mutation, func(t *testing.T) {
			point := map[string]any{"range": map[string]any{"from": "2026-10-01T00:00:00Z", "to": "2026-11-01T00:00:00Z"}, "coverage": "complete", "value": 12345}
			data := map[string]any{"kind": "bar", "value_kind": "amount", "points": []any{point}}
			block := map[string]any{"type": "chart", "v": 1, "alt_text": "Расходы за октябрь", "data": data}
			switch mutation {
			case "highlight":
				data["highlight_index"] = 2
			case "range":
				point["range"] = map[string]any{"from": "2026-11-01T00:00:00Z", "to": "2026-10-01T00:00:00Z"}
			case "bounds":
				point["data_to"] = "2026-11-01T00:00:00Z"
			case "order":
				data["points"] = []any{point, point}
			case "coverage":
				point["coverage"] = "future"
			case "no_data_value":
				point["coverage"] = "no_data"
			case "fraction_amount":
				point["value"] = 1.5
			case "percent":
				data["value_kind"] = "percent"
				point["value"] = 1.1
			case "no_alt":
				delete(block, "alt_text")
			}
			ref := presentationAnalyticsFixture(t, []any{block})
			if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: "analytics", Path: "blocks[0]"}, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
				t.Fatalf("invalid chart accepted: %v", err)
			}
		})
	}
}

func TestUnitPresentationChildSelectionRespectsUnavailableAncestor(t *testing.T) {
	for _, status := range []string{"locked", "none", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "tile_group", "v": 1, "status": status, "lock": map[string]any{"text": "Весь показатель закрыт"}, "empty_text": "Данных пока нет", "data": map[string]any{"tiles": []any{
				map[string]any{"title": "Расходы", "value": 12345, "value_kind": "amount", "status": "ready"},
			}}}})
			if status == "locked" {
				ref.Result.Evidence.LockedPaths = []string{"blocks[0]"}
			}
			out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[0]"})
			if out.Blocks[0].Type != "text" || out.Incomplete != (status == "locked") || strings.Contains(out.Blocks[0].Text, "123") {
				t.Fatalf("child bypassed ancestor status: %+v", out)
			}
			if status == "locked" && out.Blocks[0].Text != "Весь показатель закрыт" {
				t.Fatal("ancestor reason lost")
			}
			if _, err := preparePresentation(domain.GenerationTask{TransferMode: "with"}, presentationArgs{ResultID: "read", View: "analytics", Path: "blocks[0].data.tiles[10]"}, map[string]ToolResultReference{"read": ref}); !errors.Is(err, errModelProtocol) {
				t.Fatal("nonexistent child accepted through ancestor")
			}
		})
	}
}

func TestUnitPresentationAnalyticsProjectsOnlyTrustedOperationActions(t *testing.T) {
	nativeAction := map[string]any{"type": "open_operations", "filter": map[string]any{"amount_from_minor": "9007199254740991", "transfer_mode": "with"}}
	restAction := map[string]any{"type": "open_operations", "filter": map[string]any{"amount_from": json.Number("9007199254740991"), "transfer_mode": "with"}}
	ref := presentationAnalyticsFixture(t, []any{map[string]any{"type": "rows", "v": 1, "title": "Крупные операции", "action": nativeAction, "data": map[string]any{"rows": []any{
		map[string]any{"label": "Дублированный переход", "value": 10, "value_kind": "count", "action": restAction},
		map[string]any{"label": "Продукты", "value": 12345, "value_kind": "amount", "action": map[string]any{"type": "open_operations", "filter": map[string]any{"category_id": "cat_food", "amount_to": 12345, "transfer_mode": "with"}}},
		map[string]any{"label": "В чат", "text": "Текст", "action": map[string]any{"type": "open_assistant"}},
		map[string]any{"label": "Платёж", "text": "Текст", "action": map[string]any{"type": "dismiss_regular_payment", "payment_id": "private", "label": "Не подписка"}},
	}}}})
	out := presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0]"})
	if len(out.Chips) != 3 {
		t.Fatalf("expected two distinct operations chips and analytics chip: %s", out.Chips)
	}
	chips, err := grpcdialogue.Chips(out.Chips)
	if err != nil {
		t.Fatal(err)
	}
	if chips[0].Label != "Крупные операции" || chips[0].Filters.GetAmountFromMinor() != 9007199254740991 || chips[1].Filters.GetCategoryId() != "cat_food" || chips[1].Filters.GetAmountToMinor() != 12345 || chips[2].Params.GetPeriodKey() != "2026-10" {
		t.Fatalf("filters were inferred or rounded: %+v", chips)
	}
	ref = presentationAnalyticsFixture(t, []any{map[string]any{"type": "tile_group", "v": 1, "data": map[string]any{"tiles": []any{
		map[string]any{"title": "Доходы", "value": 12345, "value_kind": "amount", "action": restAction},
		map[string]any{"title": "Другие данные", "value": 1, "value_kind": "count", "action": map[string]any{"type": "open_operations", "filter": map[string]any{"q": "unselected", "transfer_mode": "with"}}},
	}}}})
	out = presentationRun(t, ref, presentationArgs{View: "analytics", Path: "blocks[0].data.tiles[0]"})
	if len(out.Chips) != 2 || strings.Contains(string(presentationJSON(t, out.Chips)), "unselected") {
		t.Fatalf("unselected tile action leaked: %s", out.Chips)
	}
}

func TestUnitPresentationActionSkipsUnknownIncompleteAndInvalidFilters(t *testing.T) {
	task := domain.GenerationTask{TransferMode: "with"}
	for _, raw := range []string{
		`null`, `{}`, `{"type":"future","filter":{"q":"x","transfer_mode":"with"}}`,
		`{"type":"open_operations"}`, `{"type":"open_operations","filter":{}}`,
		`{"type":"open_operations","filter":{"selection_name":"Название","transfer_mode":"with"}}`,
		`{"type":"open_operations","filter":{"q":"x","transfer_mode":"without"}}`,
		`{"type":"open_operations","filter":{"amount_from":1.5,"transfer_mode":"with"}}`,
		`{"type":"open_operations","filter":{"amount_from":10,"amount_to":1,"transfer_mode":"with"}}`,
		`{"type":"open_operations","filter":{"from":"2026-11-01T00:00:00Z","to":"2026-10-01T00:00:00Z","transfer_mode":"with"}}`,
	} {
		if chip := presentationActionChip(task, "Переход", json.RawMessage(raw)); chip != nil {
			t.Fatalf("invalid action became chip: %s", chip)
		}
	}
	chip := presentationActionChip(task, "", json.RawMessage(`{"type":"open_operations","filter":{"q":"магазин","transfer_mode":"with"}}`))
	if chip == nil || !strings.Contains(string(chip), "Операции по выборке") {
		t.Fatal("missing safe server-owned label")
	}
}
