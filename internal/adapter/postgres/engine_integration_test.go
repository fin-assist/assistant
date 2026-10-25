package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/go-migrate"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

// The engine uses real transactions, tombstones, durable blocks and accounting here.
// Model and data ports are scripted; provider HTTP/gRPC protocols have their own tests.
type storedModel struct {
	steps [][]generation.ModelItem
	step  int
}

func (m *storedModel) Stream(_ context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return generation.ModelResult{}, err
	}
	if err := reserve(raw); err != nil {
		return generation.ModelResult{}, err
	}
	if m.step < len(m.steps) {
		output := m.steps[m.step]
		m.step++
		return generation.ModelResult{Output: output}, nil
	}
	text := "Проверенные финансовые данные"
	if err := emit(generation.ModelDelta{Text: text}); err != nil {
		return generation.ModelResult{}, err
	}
	raw, err = json.Marshal(map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text}}})
	return generation.ModelResult{Output: []generation.ModelItem{{ID: "message", Type: "message", Raw: raw}}}, err
}
func storedCall(id, name, args string) generation.ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": args, "status": "completed"})
	return generation.ModelItem{ID: id, Type: "function_call", CallID: id, Name: name, Arguments: args, Raw: raw}
}

type storedData struct{ result tooldata.Result }

func (d storedData) Overview(context.Context, tooldata.Subject) (tooldata.Result, error) {
	return d.result, nil
}
func (d storedData) Aggregate(context.Context, tooldata.Subject, tooldata.AggregateArgs) (tooldata.Result, error) {
	return d.result, nil
}
func (d storedData) Find(context.Context, tooldata.Subject, tooldata.FindArgs) (tooldata.Result, error) {
	return d.result, nil
}
func (d storedData) Analytics(context.Context, tooldata.Subject, tooldata.AnalyticsArgs) (tooldata.Result, error) {
	return d.result, nil
}
func (d storedData) Periods(context.Context, tooldata.Subject, tooldata.PeriodsArgs) (tooldata.Result, error) {
	return d.result, nil
}

func storedResult(t *testing.T, name, raw string) tooldata.Result {
	t.Helper()
	result := tooldata.Result{JSON: json.RawMessage(raw)}
	var facts any
	switch name {
	case "aggregate_operations":
		result.Aggregate = &tooldata.Aggregate{}
		facts = result.Aggregate
	case "find_operations":
		result.Operations = &tooldata.Operations{}
		facts = result.Operations
	case "get_analytics":
		result.Analytics = &tooldata.Analytics{}
		facts = result.Analytics
	}
	if err := json.Unmarshal([]byte(raw), facts); err != nil {
		t.Fatal(err)
	}
	return result
}

func runStoredEngine(t *testing.T, db *DB, model generation.Model, data tooldata.Data) (dialogue.Answer, int) {
	t.Helper()
	cfg := migrate.DefaultConfig(db.Primary().Config().ConnString())
	if err := migrate.Up(t.Context(), cfg, []migrate.Source{{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations}}); err != nil {
		t.Fatal(err)
	}
	task := acceptedQuestion(t, db, dialogueUser, "Покажи мои расходы", storeNow)
	guards, err := tombstone.New(db, db.Primary(), func() time.Time { return storeNow })
	if err != nil {
		t.Fatal(err)
	}
	engine, err := generation.New(NewGenerations(db), db, guards, model, data, generation.EngineLimits{
		Budget:          generation.BudgetLimits{InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxSteps: 8, MaxFunctions: 16},
		MaxOutputTokens: 4096, ParallelFunctions: 4, MaxSearchCalls: 3,
	}, func() time.Time { return storeNow })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := engine.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	saved, err := NewDialogue(db).Snapshot(ctx, dialogueUser, task.AnswerID)
	if err != nil {
		t.Fatal(err)
	}
	return saved, storeUsed(t, db, dialogueUser)
}

func TestIntegrationEnginePersistsTrustedPresentationAndAccounting(t *testing.T) {
	for _, tc := range []struct{ name, tool, args, view, path, raw, want string }{
		{name: "aggregate-exact-overflow", tool: "aggregate_operations", args: `{"all_time":true}`, view: "rows", raw: `{"total_amount_minor":9223372036854775807,"total_operation_count":4}`, want: "92233720368547758,07"},
		{name: "aggregate-category", tool: "aggregate_operations", args: `{"all_time":true,"group_by":["category"]}`, view: "rows", raw: `{"total_amount_minor":12345,"total_operation_count":2,"truncated":true,"groups":[{"keys":[{"category":{"id":"food","name":"Продукты"}}],"amount_minor":12345,"operation_count":2,"average_minor":6173,"filters":{"category_id":"food","transfer_mode":"without"}}]}`, want: "Продукты"},
		{name: "aggregate-merchant-month", tool: "aggregate_operations", args: `{"all_time":true,"group_by":["merchant","month"]}`, view: "rows", raw: `{"groups":[{"keys":[{"merchant":{"title":"Магазин","selection":"selection-shop"}},{"month":"2026-10"}],"filters":{"selection":"selection-shop","selection_name":"Магазин","transfer_mode":"without"}}]}`, want: "Магазин"},
		{name: "operations-currencies", tool: "find_operations", args: `{"all_time":true}`, view: "rows", raw: `{"total_count":100,"time_zone":"Europe/Moscow","items":[{"title":"Магазин","kind":"expense","amount_minor":12345,"currency":"RUB","occurred_at":"2026-10-01T22:00:00Z"},{"title":"Возврат","kind":"income","amount_minor":750,"currency":"USD","occurred_at":"2026-10-03T12:00:00Z"},{"title":"Перевод","kind":"own_transfer","amount_minor":12,"currency":"JPY","occurred_at":"2026-10-03T12:00:00Z"}]}`, want: "7,50 USD"},
		{name: "analytics-rows", tool: "get_analytics", args: `{"period":"month","period_key":"2026-10"}`, view: "analytics", path: "blocks[0]", raw: `{"has_data":true,"currency":"RUB","params":{"period":"month","period_key":"2026-10","transfer_mode":"without"},"blocks":[{"type":"rows","v":1,"alt_text":"Расходы","data":{"rows":[{"label":"Продукты","value":12345,"value_kind":"amount"}]}}]}`, want: "Продукты"},
		{name: "analytics-unknown-fallback", tool: "get_analytics", args: `{"period":"month","period_key":"2026-10"}`, view: "analytics", path: "blocks[0]", raw: `{"has_data":true,"currency":"RUB","params":{"period":"month","period_key":"2026-10","transfer_mode":"without"},"blocks":[{"type":"future","v":3,"alt_text":"Будущий показатель"}]}`, want: "Будущий показатель"},
		{name: "analytics-forecast", tool: "get_analytics", args: `{"period":"month","period_key":"2026-10"}`, view: "analytics", path: "blocks[0].data.tiles[0]", raw: `{"has_data":true,"currency":"RUB","params":{"period":"month","period_key":"2026-10","transfer_mode":"without"},"blocks":[{"type":"tile_group","v":1,"data":{"tiles":[{"title":"Прогноз","status":"tentative","value":12345,"value_kind":"amount","approx":true,"show_plus":true}]}}]}`, want: "предварительно"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := storedResult(t, tc.tool, tc.raw)
			present, _ := json.Marshal(map[string]string{"result_id": "read", "view": tc.view, "path": tc.path})
			model := &storedModel{steps: [][]generation.ModelItem{{storedCall("read", tc.tool, tc.args)}, {storedCall("show", "present_result", string(present))}}}
			saved, used := runStoredEngine(t, dialogueDB(t), model, storedData{result})
			if saved.Status != "complete" || !saved.Charged || used != 1 || len(saved.Blocks) < 2 {
				t.Fatalf("saved=%+v used=%d", saved, used)
			}
			body, _ := json.Marshal(saved.Blocks)
			if !strings.Contains(string(body), tc.want) {
				t.Fatalf("trusted presentation missing %q: %s", tc.want, body)
			}
		})
	}
}

func TestIntegrationEngineFailureAndFreeOutcomesAreAtomic(t *testing.T) {
	for _, tc := range []struct{ name, tool, args, code string }{
		{"off-topic", "off_topic", `{}`, "OFF_TOPIC"},
		{"cannot-answer", "cannot_answer", `{}`, "CANNOT_ANSWER"},
		{"unknown-function", "foreign_function", `{}`, "MODEL_ERROR"},
		{"forged-user", "aggregate_operations", `{"all_time":true,"user_id":"foreign"}`, "MODEL_ERROR"},
		{"unknown-result", "present_result", `{"result_id":"foreign","view":"rows"}`, "MODEL_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := dialogueDB(t)
			saved, used := runStoredEngine(t, db, &storedModel{steps: [][]generation.ModelItem{{storedCall("bad", tc.tool, tc.args)}}}, storedData{tooldata.Result{JSON: json.RawMessage(`{}`)}})
			if saved.Status != "failed" || saved.ErrorCode != tc.code || saved.Charged || used != 0 || len(saved.Blocks) != 0 || len(saved.Chips) != 0 || saved.Source != nil {
				t.Fatalf("saved=%+v used=%d", saved, used)
			}
			var events int
			if err := db.primary.QueryRow(t.Context(), `SELECT count(*) FROM off_topic_event WHERE user_id=$1`, dialogueUser).Scan(&events); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.code == "OFF_TOPIC" {
				want = 1
			}
			if events != want {
				t.Fatalf("ledger=%d want%d", events, want)
			}
		})
	}
	t.Run("no-data-complete", func(t *testing.T) {
		result := tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{NoData: true}}
		saved, used := runStoredEngine(t, dialogueDB(t), &storedModel{steps: [][]generation.ModelItem{{storedCall("read", "aggregate_operations", `{"all_time":true}`)}, {storedCall("show", "present_result", `{"result_id":"read","view":"rows"}`)}}}, storedData{result})
		if saved.Status != "complete" || saved.Charged || used != 0 || len(saved.Blocks) != 2 || saved.Blocks[0].Text != "Нет данных за выбранный период" {
			t.Fatalf("free outcome=%+v used%d", saved, used)
		}
	})
}
