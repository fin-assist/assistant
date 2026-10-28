package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/datarevision"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

type coverageStoredModel struct {
	step   int
	output func(int) ([]generation.ModelItem, string)
}

func coverageCatalogRun(t *testing.T, block, path, period, key string, locks []string) (dialogue.Answer, int) {
	t.Helper()
	result := storedResult(t, "get_analytics", `{"has_data":true,"currency":"RUB","params":{"period":"`+period+`","period_key":"`+key+`","transfer_mode":"without"},"blocks":[`+block+`]}`)
	result.Evidence.LockedPaths = locks
	present, err := json.Marshal(map[string]string{"result_id": "read", "view": "analytics", "path": path})
	if err != nil {
		t.Fatal(err)
	}
	read, err := json.Marshal(map[string]string{"period": period, "period_key": key})
	if err != nil {
		t.Fatal(err)
	}
	model := &storedModel{steps: [][]generation.ModelItem{{storedCall("read", "get_analytics", string(read))}, {storedCall("show", "present_result", string(present))}}}
	return runStoredEngine(t, dialogueDB(t), model, storedData{result})
}

func TestIntegrationEngineCatalogChartsKeepCoverageOrUseExplicitFallback(t *testing.T) {
	point := `{"value":0.25,"coverage":"partial","data_from":"2026-09-05T00:00:00Z","data_to":"2026-09-20T00:00:00Z","range":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}}`
	base := `{"kind":"bar","value_kind":"percent","points":[` + point + `,{"coverage":"no_data","range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"}}]}`
	for _, tc := range []struct {
		name, data, want string
		failed           bool
	}{
		{"percent-with-absent-month", base, "chart", false},
		{"unsupported-caption", strings.Replace(base, `"kind":"bar"`, `"caption":"Пояснение","kind":"bar"`, 1), "text", false},
		{"unsupported-summary", strings.Replace(base, `"kind":"bar"`, `"summary":{"value":0.25},"kind":"bar"`, 1), "text", false},
		{"unsupported-notes", strings.Replace(base, `"kind":"bar"`, `"notes":["Пояснение"],"kind":"bar"`, 1), "text", false},
		{"point-note", strings.Replace(base, `"coverage":"partial"`, `"note":"Неполный период","coverage":"partial"`, 1), "text", false},
		{"future-chart-kind", strings.Replace(base, `"kind":"bar"`, `"kind":"future"`, 1), "text", false},
		{"bad-highlight", strings.Replace(base, `"kind":"bar"`, `"highlight_index":8,"kind":"bar"`, 1), "", true},
		{"reversed-range", strings.Replace(base, `"from":"2026-09-01T00:00:00Z"`, `"from":"2026-10-01T00:00:00Z"`, 1), "", true},
		{"overlapping-months", strings.Replace(base, `"from":"2026-10-01T00:00:00Z"`, `"from":"2026-09-15T00:00:00Z"`, 1), "", true},
		{"unknown-coverage", strings.Replace(base, `"coverage":"partial"`, `"coverage":"future"`, 1), "", true},
		{"no-data-with-value", strings.Replace(base, `"coverage":"partial"`, `"coverage":"no_data"`, 1), "", true},
		{"outside-bounds", strings.Replace(base, "2026-09-05T00:00:00Z", "2026-08-05T00:00:00Z", 1), "", true},
		{"reversed-bounds", strings.Replace(base, "2026-09-05T00:00:00Z", "2026-09-25T00:00:00Z", 1), "", true},
		{"percent-outside-range", strings.Replace(base, `"value":0.25`, `"value":1.25`, 1), "", true},
		{"amount-not-integer", strings.Replace(base, `"value_kind":"percent"`, `"value_kind":"amount"`, 1), "", true},
		{"amount-overflow-fallback", strings.Replace(strings.Replace(base, `"value_kind":"percent"`, `"value_kind":"amount"`, 1), `"value":0.25`, `"value":9007199254740992`, 1), "text", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := `{"type":"chart","v":1,"alt_text":"Сентябрь: четверть; октябрь: данных нет","data":` + tc.data + `}`
			saved, used := coverageCatalogRun(t, block, "blocks[0]", "month", "2026-09", nil)
			if tc.failed {
				if saved.Status != "failed" || saved.ErrorCode != "MODEL_ERROR" || saved.Charged || used != 0 || len(saved.Blocks) != 0 || len(saved.Chips) != 0 {
					t.Fatalf("invalid chart or partial output persisted: %+v used=%d", saved, used)
				}
				return
			}
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Blocks) != 2 || saved.Blocks[0].Type != tc.want {
				t.Fatalf("chart meaning or fallback lost: %+v used=%d", saved, used)
			}
			if tc.want == "text" && saved.Blocks[0].Text != "Сентябрь: четверть; октябрь: данных нет" {
				t.Fatalf("unsupported chart lost its trusted text: %+v", saved.Blocks[0])
			}
		})
	}
}

func TestIntegrationEngineSelectedLocksAndCalendarChipsStayConsistent(t *testing.T) {
	for _, tc := range []struct {
		name, block, path, lock, want string
	}{
		{"block-lock", `{"type":"rows","v":1,"status":"locked","lock":{"text":"Пояснение блокировки"}}`, "blocks[0]", "blocks[0]", "Пояснение блокировки"},
		{"block-lock-fallback", `{"type":"rows","v":1,"status":"locked","lock":{}}`, "blocks[0]", "blocks[0]", "Пока недоступно"},
		{"tile-lock", `{"type":"tile_group","v":1,"data":{"tiles":[{"status":"locked","lock":{"text":"Платный прогноз"},"value_kind":"amount","value":999}]}}`, "blocks[0].data.tiles[0]", "blocks[0].data.tiles[0]", "Платный прогноз"},
		{"ancestor-lock", `{"type":"tile_group","v":1,"status":"locked","lock":{"text":"Закрытый раздел"},"data":{"tiles":[{"value_kind":"amount","value":999}]}}`, "blocks[0].data.tiles[0]", "blocks[0]", "Закрытый раздел"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, used := coverageCatalogRun(t, tc.block, tc.path, "month", "2026-10", []string{tc.lock})
			if saved.Status != "complete" || saved.Charged || used != 0 || len(saved.Blocks) != 2 || saved.Blocks[0].Type != "text" || saved.Blocks[0].Text != tc.want || strings.Contains(saved.Blocks[0].Text, "999") {
				t.Fatalf("selected lock leaked facts or consumed a question: %+v used=%d", saved, used)
			}
		})
	}
	for _, tc := range []struct{ period, key string }{{"quarter", "2026-Q3"}, {"year", "2026"}} {
		t.Run(tc.period, func(t *testing.T) {
			block := `{"type":"rows","v":1,"title":"Поступления","action":{"q":"Перевод","transfer_mode":"without"},"data":{"rows":[{"label":"Пример","value_kind":"count","value":2,"action":{"type":"open_operations","filter":{"q":"Кофе","transfer_mode":"without","amount_from":100,"amount_to":200}}},{"label":"Дубликат","value_kind":"count","value":1,"action":{"type":"open_operations","filter":{"q":"Кофе","transfer_mode":"without","amount_from":100,"amount_to":200}}}]}}`
			saved, used := coverageCatalogRun(t, block, "blocks[0]", tc.period, tc.key, nil)
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Chips) != 3 {
				t.Fatalf("native/row/calendar navigation lost or duplicated: %+v used=%d", saved, used)
			}
			body, err := json.Marshal(saved.Chips)
			if err != nil || !strings.Contains(string(body), tc.key) || !strings.Contains(string(body), `"amount_from":100`) || !strings.Contains(string(body), `"amount_to":200`) {
				t.Fatalf("trusted navigation lost calendar or amount bounds: %s err=%v", body, err)
			}
		})
	}
}

func (m *coverageStoredModel) NewSession() generation.ModelSession {
	return generation.ModelSessionFunc(m.Stream)
}

func (m *coverageStoredModel) Stream(_ context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return generation.ModelResult{}, err
	}
	if err := reserve(raw); err != nil {
		return generation.ModelResult{}, err
	}
	m.step++
	items, text := m.output(m.step)
	if text != "" {
		if err := emit(generation.ModelDelta{Text: text}); err != nil {
			return generation.ModelResult{}, err
		}
	}
	return generation.ModelResult{Output: items}, nil
}

func coverageStoredMessage(t *testing.T, id, text string, annotations []any) generation.ModelItem {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": annotations}}})
	if err != nil {
		t.Fatal(err)
	}
	return generation.ModelItem{ID: id, Type: "message", Raw: raw}
}

func TestIntegrationEngineProviderCitationsPersistOnlyAfterObservedSearch(t *testing.T) {
	for _, variant := range []string{"valid-and-deduplicated", "without-search", "unknown-annotation", "javascript", "credentials", "encoded-control", "relative", "invalid-host", "invalid-port", "bad-offset", "missing-url", "null-title", "too-many-sources", "oversized-title"} {
		t.Run(variant, func(t *testing.T) {
			annotation := map[string]any{"type": "url_citation", "url": "HTTPS://EXAMPLE.ORG/report", "title": "  <b>**Источник**</b>\n[данные]\u202e", "start_index": 0, "end_index": 0}
			annotations := []any{annotation, map[string]any{"type": "url_citation", "url": "https://example.org/report", "title": "Duplicate"}, map[string]any{"type": "url_citation", "url": "another.example", "title": ""}}
			switch variant {
			case "unknown-annotation":
				annotations = []any{map[string]any{"type": "future_annotation", "url": "javascript:ignored"}}
			case "javascript":
				annotation["url"] = "javascript:private"
			case "credentials":
				annotation["url"] = "https://user:private@example.org"
			case "encoded-control":
				annotation["url"] = "https://example.org/%0Aprivate"
			case "relative":
				annotation["url"] = "//example.org"
			case "invalid-host":
				annotation["url"] = "https://-bad.example"
			case "invalid-port":
				annotation["url"] = "https://example.org:65536"
			case "bad-offset":
				annotation["start_index"] = -1
			case "missing-url":
				delete(annotation, "url")
			case "null-title":
				annotation["title"] = nil
			case "too-many-sources":
				annotations = nil
				for i := range 33 {
					annotations = append(annotations, map[string]any{"type": "url_citation", "url": "https://example.org/" + strings.Repeat("a", i+1)})
				}
			case "oversized-title":
				annotation["title"] = strings.Repeat("я", 129)
			}
			model := &coverageStoredModel{output: func(step int) ([]generation.ModelItem, string) {
				text := "Проверенные сведения."
				if step == 1 {
					items := []generation.ModelItem{coverageStoredMessage(t, "message-1", text, annotations), storedCall("overview", "get_data_overview", `{}`)}
					if variant != "without-search" {
						items = append(items, generation.ModelItem{ID: "search", Type: "web_search_call", Raw: json.RawMessage(`{"id":"search","type":"web_search_call","status":"completed"}`)})
					}
					return items, text
				}
				return []generation.ModelItem{coverageStoredMessage(t, "message-2", text, nil)}, text
			}}
			saved, used := runStoredEngine(t, dialogueDB(t), model, storedData{tooldata.Result{JSON: json.RawMessage(`{}`)}})
			valid := variant == "valid-and-deduplicated" || variant == "without-search" || variant == "unknown-annotation"
			if !valid {
				if saved.Status != "failed" || saved.ErrorCode != "MODEL_ERROR" || saved.Charged || used != 0 || len(saved.Blocks) != 0 || len(saved.Chips) != 0 || saved.Source != nil {
					t.Fatalf("unsafe citations or partial text persisted: %+v used=%d", saved, used)
				}
				return
			}
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Blocks) == 0 {
				t.Fatalf("valid citation outcome lost: %+v used=%d", saved, used)
			}
			if variant != "valid-and-deduplicated" {
				if len(saved.Blocks) != 1 || strings.Contains(saved.Blocks[0].Text, "Источники:") {
					t.Fatalf("unconfirmed annotation became a source: %+v", saved.Blocks)
				}
				return
			}
			if len(saved.Blocks) != 2 || strings.Count(saved.Blocks[1].Text, "https://example.org/report") != 1 || !strings.Contains(saved.Blocks[1].Text, "https://another.example") || strings.ContainsAny(saved.Blocks[1].Text, "<>[]*\u202e") {
				t.Fatalf("citation order, normalization, deduplication or title safety lost: %+v", saved.Blocks)
			}
		})
	}
}

// These cases exercise presentation together with durable blocks and accounting.
// They do not substitute for the peer protocol or public RPC tests.
func TestIntegrationEngineCatalogStatesPersistWithoutInventedValues(t *testing.T) {
	for _, tc := range []struct {
		name, block, path, want, kind string
		failed                        bool
	}{
		{"text", `{"type":"text","v":1,"data":{"text":"Подтверждённые сведения"}}`, "blocks[0]", "Подтверждённые сведения", "text", false},
		{"future-fallback", `{"type":"future","v":3,"alt_text":"Новый показатель"}`, "blocks[0]", "Новый показатель", "text", false},
		{"none", `{"type":"rows","v":1,"status":"none","empty_text":"Пока нет периода"}`, "blocks[0]", "Пока нет периода", "text", false},
		{"unavailable", `{"type":"rows","v":1,"status":"unavailable"}`, "blocks[0]", "Показатель пока недоступен", "text", false},
		{"foreign-currency", `{"type":"rows","v":1,"data":{"rows":[{"label":"Возврат","value_kind":"amount","value":750}]}}`, "blocks[0]", "7,50 USD", "rows", false},
		{"count", `{"type":"rows","v":1,"data":{"rows":[{"label":"Покупок","value_kind":"count","value":3}]}}`, "blocks[0]", "Покупок", "rows", false},
		{"future-value-kind-text", `{"type":"rows","v":1,"data":{"rows":[{"label":"Показатель","value_kind":"future","value":12,"text":"Присланное пояснение"}]}}`, "blocks[0]", "Присланное пояснение", "rows", false},
		{"percent", `{"type":"rows","v":1,"data":{"rows":[{"label":"Доля","value_kind":"percent","value":0.25}]}}`, "blocks[0]", "0.25", "rows", false},
		{"percent-outside-display-range", `{"type":"rows","v":1,"data":{"rows":[{"label":"Изменение","value_kind":"percent","value":1.25}]}}`, "blocks[0]", "1.25", "rows", false},
		{"tentative-tile", `{"type":"tile_group","v":1,"data":{"tiles":[{"title":"Прогноз","status":"tentative","value_kind":"amount","value":12345,"approx":true,"show_plus":true}]}}`, "blocks[0].data.tiles[0]", "предварительно", "rows", false},
		{"unavailable-tile", `{"type":"tile_group","v":1,"data":{"tiles":[{"status":"unavailable","value":999}]}}`, "blocks[0].data.tiles[0]", "Показатель пока недоступен", "text", false},
		{"unavailable-ancestor", `{"type":"tile_group","v":1,"status":"unavailable","empty_text":"Неполный период","data":{"tiles":[{"title":"Скрыто","value_kind":"amount","value":999}]}}`, "blocks[0].data.tiles[0]", "Неполный период", "text", false},
		{"missing-fallback", `{"type":"future","v":3}`, "blocks[0]", "", "", true},
		{"missing-text", `{"type":"text","v":1,"data":{}}`, "blocks[0]", "", "", true},
		{"missing-label", `{"type":"rows","v":1,"data":{"rows":[{"value_kind":"count","value":3}]}}`, "blocks[0]", "", "", true},
		{"negative-count", `{"type":"rows","v":1,"data":{"rows":[{"label":"Покупок","value_kind":"count","value":-1}]}}`, "blocks[0]", "", "", true},
		{"fractional-count", `{"type":"rows","v":1,"data":{"rows":[{"label":"Покупок","value_kind":"count","value":1.5}]}}`, "blocks[0]", "", "", true},
		{"missing-value", `{"type":"rows","v":1,"data":{"rows":[{"label":"Показатель"}]}}`, "blocks[0]", "", "", true},
		{"tile-out-of-bounds", `{"type":"tile_group","v":1,"data":{"tiles":[]}}`, "blocks[0].data.tiles[0]", "", "", true},
		{"invalid-path", `{"type":"rows","v":1}`, "blocks[-1]", "", "", true},
		{"unknown-tile-state", `{"type":"tile_group","v":1,"data":{"tiles":[{"status":"future"}]}}`, "blocks[0].data.tiles[0]", "", "", true},
		{"unproven-lock", `{"type":"rows","v":1,"status":"locked","lock":{"text":"Скрыто"}}`, "blocks[0]", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currency := "RUB"
			if tc.name == "foreign-currency" {
				currency = "USD"
			}
			raw := `{"has_data":true,"currency":"` + currency + `","params":{"period":"month","period_key":"2026-10","transfer_mode":"without"},"blocks":[` + tc.block + `]}`
			result := storedResult(t, "get_analytics", raw)
			present, err := json.Marshal(map[string]string{"result_id": "read", "view": "analytics", "path": tc.path})
			if err != nil {
				t.Fatal(err)
			}
			db := dialogueDB(t)
			model := &storedModel{steps: [][]generation.ModelItem{{storedCall("read", "get_analytics", `{"period":"month","period_key":"2026-10"}`)}, {storedCall("show", "present_result", string(present))}}}
			saved, used := runStoredEngine(t, db, model, storedData{result})
			if tc.failed {
				if saved.Status != "failed" || saved.ErrorCode != "MODEL_ERROR" || saved.Charged || used != 0 || len(saved.Blocks) != 0 || len(saved.Chips) != 0 || saved.Source != nil {
					t.Fatalf("invalid presentation persisted effects: %+v used=%d", saved, used)
				}
				return
			}
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Blocks) != 2 || saved.Blocks[0].Type != tc.kind || len(saved.Chips) != 1 {
				t.Fatalf("presentation or charge lost: %+v used=%d", saved, used)
			}
			body, err := json.Marshal(saved.Blocks[0])
			if err != nil || !strings.Contains(string(body), tc.want) || strings.Contains(string(body), "999") {
				t.Fatalf("trusted display lost or hidden value leaked: %s err=%v", body, err)
			}
			var chip struct {
				Screen string `json:"screen"`
				Params struct {
					Period string `json:"period"`
					Date   string `json:"date"`
					Mode   string `json:"transfer_mode"`
				} `json:"params"`
			}
			if json.Unmarshal(saved.Chips[0], &chip) != nil || chip.Screen != "analytics" || chip.Params.Period != "month" || chip.Params.Date != "2026-10" || chip.Params.Mode != "without" {
				t.Fatalf("persisted calendar navigation changed: %s", saved.Chips[0])
			}
		})
	}
}

func TestIntegrationEngineMonthlyChartPersistsCoverageAndRejectsUnprovenMonths(t *testing.T) {
	for _, variant := range []string{"partial-with-gap", "overflow", "missing-month", "duplicate-month", "unknown-coverage", "missing-data-to", "wrong-month-range", "duplicate-group", "invalid-zone", "outside-calendar"} {
		t.Run(variant, func(t *testing.T) {
			start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
			end := start.AddDate(0, 3, 0)
			result := tooldata.Result{
				JSON: json.RawMessage(`{"trusted":"financial facts"}`),
				Evidence: tooldata.Evidence{Source: &tooldata.Source{
					Range: &tooldata.Range{Start: start, End: end}, CalculatedAt: &storeNow, TimeZone: "UTC",
					Version: tooldata.Version{Revisions: datarevision.Current{"categories": 1, "uploads": 1, "time_zone": 1}},
				}},
				Aggregate: &tooldata.Aggregate{Zone: "UTC", Range: &tooldata.Range{Start: start, End: end}, Truncated: true},
				Overview:  &tooldata.Overview{Zone: "UTC"},
			}
			for i := range 3 {
				a, b := start.AddDate(0, i, 0), start.AddDate(0, i+1, 0)
				last := b.Add(-time.Second)
				month := tooldata.Month{Month: a.Format("2006-01"), Range: tooldata.Range{Start: a, End: b}, Bounds: tooldata.Bounds{Coverage: "complete", DataFrom: &a, DataTo: &last}}
				if i == 0 {
					first := a.AddDate(0, 0, 2)
					month.Bounds.Coverage, month.Bounds.DataFrom = "partial", &first
					month.Bounds.Gaps = []tooldata.Range{{Start: a.AddDate(0, 0, 10), End: a.AddDate(0, 0, 11)}}
				}
				result.Overview.Months = append(result.Overview.Months, month)
				if i != 1 {
					group := tooldata.Group{Keys: []tooldata.GroupKey{{Month: month.Month}}, Amount: 12345, Count: 2, Filters: &tooldata.NavigationFilter{From: &a, To: &b, Mode: "without"}}
					result.Aggregate.Groups = append([]tooldata.Group{group}, result.Aggregate.Groups...)
				}
			}
			switch variant {
			case "overflow":
				result.Aggregate.Groups[0].Amount = 9223372036854775807
			case "missing-month":
				result.Overview.Months = result.Overview.Months[:2]
			case "duplicate-month":
				result.Overview.Months = append(result.Overview.Months, result.Overview.Months[0])
			case "unknown-coverage":
				result.Overview.Months[0].Bounds.Coverage = "future"
			case "missing-data-to":
				result.Overview.Months[0].Bounds.DataTo = nil
			case "wrong-month-range":
				result.Overview.Months[0].Range.End = end
			case "duplicate-group":
				result.Aggregate.Groups = append(result.Aggregate.Groups, result.Aggregate.Groups[0])
			case "invalid-zone":
				result.Aggregate.Zone, result.Overview.Zone, result.Evidence.Source.TimeZone = "Unknown/Zone", "Unknown/Zone", "Unknown/Zone"
			case "outside-calendar":
				result.Aggregate.Range.Start = start.AddDate(0, 0, 1)
			}
			model := &storedModel{steps: [][]generation.ModelItem{
				{storedCall("read", "aggregate_operations", `{"all_time":true,"group_by":["month"]}`)},
				{storedCall("coverage", "get_data_overview", `{}`)},
				{storedCall("show", "present_result", `{"result_id":"read","coverage_result_id":"coverage","view":"chart"}`)},
			}}
			saved, used := runStoredEngine(t, dialogueDB(t), model, storedData{result})
			if variant != "partial-with-gap" && variant != "overflow" {
				if saved.Status != "failed" || saved.ErrorCode != "MODEL_ERROR" || saved.Charged || used != 0 || len(saved.Blocks) != 0 || len(saved.Chips) != 0 || saved.Source != nil {
					t.Fatalf("unproven chart persisted: %+v used=%d", saved, used)
				}
				return
			}
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Blocks) < 2 || len(saved.Chips) != 2 || saved.Source == nil || !saved.Source.From.Equal(start) || !saved.Source.To.Equal(end) {
				t.Fatalf("chart or provenance lost: %+v used=%d", saved, used)
			}
			if variant == "overflow" {
				if saved.Blocks[0].Type != "rows" || !strings.Contains(string(saved.Blocks[0].Body), "92233720368547758,07") {
					t.Fatalf("unsafe chart failed to use exact text rows: %+v", saved.Blocks)
				}
				return
			}
			var chart struct {
				Alt  string `json:"alt_text"`
				Data struct {
					Points []struct {
						Value    int64                        `json:"value"`
						Coverage string                       `json:"coverage"`
						Range    struct{ From, To time.Time } `json:"range"`
					} `json:"points"`
				} `json:"data"`
			}
			if json.Unmarshal(saved.Blocks[0].Body, &chart) != nil || saved.Blocks[0].Type != "chart" || len(chart.Data.Points) != 2 || chart.Data.Points[0].Coverage != "partial" || chart.Data.Points[0].Value != 12345 || !chart.Data.Points[0].Range.From.Equal(start) || !chart.Data.Points[1].Range.From.Equal(start.AddDate(0, 2, 0)) || !strings.Contains(chart.Alt, "нет данных с") || !strings.Contains(chart.Alt, "Показана только часть месяцев") {
				t.Fatalf("chart invented absent month or lost limitations: %s", saved.Blocks[0].Body)
			}
		})
	}
}
