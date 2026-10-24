package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestAPICatalogPresentationSurvivesPersistenceAndPublicMapping(t *testing.T) {
	for _, tc := range []struct {
		name, kind, status, empty, alt, path, raw, want string
		version                                         int32
		failed                                          bool
	}{
		{name: "text", kind: "text", version: 1, raw: `{"text":"Серверное пояснение"}`, want: "Серверное пояснение"},
		{name: "unknown-type", kind: "new_widget", version: 1, alt: "Новый показатель: 42", raw: `{}`, want: "Новый показатель: 42"},
		{name: "unknown-version", kind: "rows", version: 2, alt: "Серверная сводка", raw: `{}`, want: "Серверная сводка"},
		{name: "unavailable", kind: "rows", version: 1, status: "unavailable", empty: "Пока нечего сравнить", raw: `{}`, want: "Пока нечего сравнить"},
		{name: "rows-mixed-units", kind: "rows", version: 1, raw: `{"rows":[{"label":"Итог","value":12345,"value_kind":"amount"},{"label":"Операций","value":3,"value_kind":"count"},{"label":"Доля","value":0.25,"value_kind":"percent"},{"label":"Изменение","value":-0.5,"value_kind":"percent"},{"label":"Комментарий","text":"Данных достаточно"}]}`, want: "Данных достаточно"},
		{name: "rows-negative-count", kind: "rows", version: 1, raw: `{"rows":[{"label":"Изменение","value":-3,"value_kind":"count"}]}`, failed: true},
		{name: "forecast-approx-positive", kind: "tile_group", version: 1, path: "blocks[0].data.tiles[0]", raw: `{"tiles":[{"id":"metric","title":"Прогноз","status":"tentative","value":12345,"value_kind":"amount","approx":true,"show_plus":true}]}`, want: "≈+123,45"},
		{name: "forecast-approx-negative", kind: "tile_group", version: 1, path: "blocks[0].data.tiles[0]", raw: `{"tiles":[{"id":"metric","title":"Прогноз","status":"tentative","value":-12345,"value_kind":"amount","approx":true,"show_plus":true}]}`, want: "≈-123,45"},
		{name: "forecast-percent", kind: "tile_group", version: 1, path: "blocks[0].data.tiles[0]", raw: `{"tiles":[{"id":"metric","title":"Прогноз","status":"ready","value":0.25,"value_kind":"percent","approx":true,"show_plus":true}]}`, want: "≈+0.25"},
		{name: "tile-none", kind: "tile_group", version: 1, path: "blocks[0].data.tiles[0]", raw: `{"tiles":[{"id":"metric","title":"Прогноз","status":"none"}]}`, want: "Показатель пока недоступен"},
		{name: "chart", kind: "chart", version: 1, alt: "Расходы за октябрь", raw: `{"kind":"bar","value_kind":"amount","points":[{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"},"value":12345,"coverage":"complete"}]}`, want: "Расходы за октябрь"},
		{name: "chart-future-kind", kind: "chart", version: 1, alt: "График нового типа", raw: `{"kind":"line","value_kind":"amount","points":[]}`, want: "График нового типа"},
		{name: "chart-note-fallback", kind: "chart", version: 1, alt: "123,45 ₽ с пояснением", raw: `{"kind":"bar","value_kind":"amount","caption":"Пояснение","points":[{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"},"value":12345,"coverage":"complete"}]}`, want: "123,45 ₽ с пояснением"},
		{name: "invalid-chart-coverage", kind: "chart", version: 1, alt: "Недостоверный график", raw: `{"kind":"bar","value_kind":"amount","points":[{"range":{"from":"2026-10-01T00:00:00Z","to":"2026-11-01T00:00:00Z"},"value":12345,"coverage":"future"}]}`, failed: true},
		{name: "invalid-tile-value", kind: "tile_group", version: 1, path: "blocks[0].data.tiles[0]", raw: `{"tiles":[{"id":"metric","title":"Операции","status":"ready","value":1.5,"value_kind":"count"}]}`, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var values map[string]any
			if err := json.Unmarshal([]byte(tc.raw), &values); err != nil {
				t.Fatal(err)
			}
			data, err := structpb.NewStruct(values)
			if err != nil {
				t.Fatal(err)
			}
			response := presenterAnalytics(t)
			block := &commonv1.Block{Id: "selected", Type: tc.kind, V: tc.version, Status: tc.status, Data: data}
			if tc.alt != "" {
				block.AltText = &tc.alt
			}
			if tc.empty != "" {
				block.EmptyText = &tc.empty
			}
			response.Blocks = []*commonv1.Block{block}
			peers := &generationAPIPeers{grant: true, analyticsResponse: response}
			path := tc.path
			if path == "" {
				path = "blocks[0]"
			}
			args, _ := json.Marshal(map[string]string{"result_id": "analytics", "view": "analytics", "path": path})
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				switch step {
				case 1:
					_, _ = io.WriteString(w, generationAPIFunctionID("analytics", "get_analytics", `{"period":"month","period_key":"2026-09"}`))
				case 2:
					_, _ = io.WriteString(w, generationAPIFunctionID("show", "present_result", string(args)))
				default:
					_, _ = io.WriteString(w, generationAPIText("Данные показаны"))
				}
			})
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Покажи показатель", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if tc.failed {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
				t.Fatalf("done=%v failure=%v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			snapshot := presenterSnapshot(t, env, generationAPIUser, accepted.AnswerId)
			if len(saved.Blocks) != 2 || len(snapshot.Blocks) != 2 || len(saved.Chips) != 1 || saved.Chips[0].GetParams().GetTransferMode() != commonv1.TransferMode_TRANSFER_MODE_WITHOUT {
				t.Fatalf("public persistence=%v snapshot=%v", saved, snapshot)
			}
			shown := saved.Blocks[0].GetAltText() + saved.Blocks[0].GetText().GetText()
			if !strings.Contains(shown, tc.want) {
				t.Fatalf("display=%q want %q", shown, tc.want)
			}
		})
	}
}
