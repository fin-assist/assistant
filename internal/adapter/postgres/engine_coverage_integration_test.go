package postgres

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

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
			version := start.Add(-time.Hour)
			result := tooldata.Result{
				JSON: json.RawMessage(`{"trusted":"financial facts"}`),
				Evidence: tooldata.Evidence{Source: &tooldata.Source{
					Range: &tooldata.Range{Start: start, End: end}, CalculatedAt: &storeNow, TimeZone: "UTC",
					Version: tooldata.Version{CategoriesChangedAt: &version, UploadsChangedAt: &version},
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
