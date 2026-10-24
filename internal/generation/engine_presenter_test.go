package generation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/tooldata"
)

func engineAggregateForPresentation() tooldata.Result {
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	return tooldata.Result{
		JSON:      json.RawMessage(`{"total_amount_minor":"12345","total_operation_count":"3","range":{"start":"2026-09-01T00:00:00Z","end":"2026-10-01T00:00:00Z"},"bounds":{"coverage":"COVERAGE_COMPLETE"},"time_zone":"Europe/Moscow","calculated_at":"2026-10-23T00:00:00Z"}`),
		Aggregate: &tooldata.Aggregate{Amount: 12345, Count: 3, Range: &tooldata.Range{Start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, Zone: "Europe/Moscow"},
		Evidence:  tooldata.Evidence{Source: &tooldata.Source{CalculatedAt: &at, TimeZone: "Europe/Moscow", Range: &tooldata.Range{Start: at.AddDate(0, -1, 0), End: at}}},
	}
}

func TestEnginePresentationPreservesTextBlockOrderAndDurability(t *testing.T) {
	store := &engineStore{}
	observer := &engineObserver{}
	limits := engineLimits()
	limits.Observer = observer
	dataCalls := 0
	data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		dataCalls++
		return engineAggregateForPresentation(), nil
	}}
	model := &engineModel{script: func(_ context.Context, request ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		switch step {
		case 1:
			if !strings.Contains(string(request.Tools[len(request.Tools)-3]), "present_result") {
				t.Error("present_result missing from the model tools")
			}
			if err := emit(ModelDelta{Text: "Сначала данные."}); err != nil {
				return ModelResult{}, err
			}
			return ModelResult{Output: []ModelItem{engineText("Сначала данные."), engineTool("read", "aggregate_operations", `{"filter":{"q":"Самокат"},"all_time":true}`)}}, nil
		case 2:
			return ModelResult{Output: []ModelItem{engineTool("show", "present_result", `{"result_id":"read","view":"rows"}`)}}, nil
		default:
			if len(store.savedBlocks) < 2 || store.savedBlocks[0].Text != "Сначала данные." || store.savedBlocks[1].Type != "rows" || !strings.Contains(string(store.savedBlocks[1].Body), "12345") {
				t.Fatalf("model continued before the trusted rows were durable: %+v", store.savedBlocks)
			}
			if !strings.Contains(string(request.Input[len(request.Input)-1]), "presented") {
				t.Error("presentation acknowledgement missing from continuation")
			}
			return engineFinal(emit, "После таблицы.")
		}
	}}
	if err := engineForTest(t, store, model, data, limits).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	blocks := store.outcome.Blocks
	if store.outcome.ErrorCode != "" || !store.outcome.Charged || dataCalls != 1 || len(blocks) < 3 || blocks[0].Text != "Сначала данные." || blocks[1].Type != "rows" || blocks[len(blocks)-1].Text != "После таблицы." || len(store.outcome.Chips) == 0 {
		t.Fatalf("invalid presentation outcome: %+v calls=%d", store.outcome, dataCalls)
	}
	var chip struct {
		Screen  string `json:"screen"`
		Filters struct {
			Query    string `json:"q"`
			From, To time.Time
			Mode     string `json:"transfer_mode"`
		} `json:"filters"`
	}
	if json.Unmarshal(store.outcome.Chips[0], &chip) != nil || chip.Screen != "operations" || chip.Filters.Query != "Самокат" || chip.Filters.Mode != "without" || !chip.Filters.From.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || !chip.Filters.To.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("aggregate-total chip lost the saved query, actual range or mode: %s", store.outcome.Chips[0])
	}
	if strings.Join(observer.functions, ",") != "aggregate_operations,present_result" || observer.finished != 1 {
		t.Fatalf("presentation accounting: %+v", observer)
	}
}

func TestEngineRejectsUntrustedPresentationBeforeAnyReadInBatch(t *testing.T) {
	for _, args := range []string{
		`{"result_id":"foreign-question-read","view":"rows"}`,
		`{"result_id":"new-read","view":"rows"}`,
		`{"result_id":"new-read","view":"rows","value":999}`,
		`{"result_id":"new-read","view":"rows","filters":{"user_id":"other"}}`,
		`{"result_id":"new-read","result_id":"other","view":"rows"}`,
		`{"result_id":"new-read","view":"imagined"}`,
	} {
		t.Run(args, func(t *testing.T) {
			store := &engineStore{}
			calls := 0
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				calls++
				return engineAggregateForPresentation(), nil
			}}
			model := &engineModel{script: func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error) {
				return ModelResult{Output: []ModelItem{engineTool("new-read", "aggregate_operations", `{"all_time":true}`), engineTool("show", "present_result", args)}}, nil
			}}
			if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if calls != 0 || len(store.savedBlocks) != 0 || store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 {
				t.Fatalf("untrusted presentation had effects: calls=%d store=%+v", calls, store)
			}
		})
	}
}

func TestEnginePresentationConsumesFunctionBudget(t *testing.T) {
	store := &engineStore{}
	limits := engineLimits()
	limits.Budget.MaxFunctions = 1
	limits.ParallelFunctions = 1
	data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		return engineAggregateForPresentation(), nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, _ func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			return ModelResult{Output: []ModelItem{engineTool("read", "aggregate_operations", `{"all_time":true}`)}}, nil
		}
		return ModelResult{Output: []ModelItem{engineTool("show", "present_result", `{"result_id":"read","view":"rows"}`)}}, nil
	}}
	if err := engineForTest(t, store, model, data, limits).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if store.outcome.ErrorCode != "MODEL_ERROR" || len(store.savedBlocks) != 0 || model.network != 2 {
		t.Fatalf("presentation bypassed the function budget: %+v", store)
	}
}

func TestEnginePresentationCallIDCannotBeReused(t *testing.T) {
	store := &engineStore{}
	reads := 0
	data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		reads++
		return engineAggregateForPresentation(), nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, _ func(ModelDelta) error, step int) (ModelResult, error) {
		switch step {
		case 1:
			return ModelResult{Output: []ModelItem{engineTool("read", "aggregate_operations", `{"all_time":true}`)}}, nil
		case 2:
			return ModelResult{Output: []ModelItem{engineTool("show", "present_result", `{"result_id":"read","view":"rows"}`)}}, nil
		default:
			return ModelResult{Output: []ModelItem{engineTool("show", "get_data_overview", `{}`)}}, nil
		}
	}}
	if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || store.outcome.ErrorCode != "MODEL_ERROR" || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 {
		t.Fatalf("presentation call ID was reused: reads=%d outcome=%+v", reads, store.outcome)
	}
}

func TestEnginePresentationBatchPreflightAndWriteFailure(t *testing.T) {
	for _, scenario := range []string{"second reference invalid", "block write fails"} {
		t.Run(scenario, func(t *testing.T) {
			store := &engineStore{}
			reads := 0
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				reads++
				return engineAggregateForPresentation(), nil
			}}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
				if step == 1 {
					if err := emit(ModelDelta{Text: "Предварительный текст"}); err != nil {
						return ModelResult{}, err
					}
					return ModelResult{Output: []ModelItem{engineText("Предварительный текст"), engineTool("read", "aggregate_operations", `{"all_time":true}`)}}, nil
				}
				output := []ModelItem{engineTool("show", "present_result", `{"result_id":"read","view":"rows"}`)}
				if scenario == "second reference invalid" {
					output = append(output, engineTool("show-other", "present_result", `{"result_id":"foreign","view":"rows"}`))
				} else {
					store.appendErr = errors.New("private PostgreSQL body")
				}
				return ModelResult{Output: output}, nil
			}}
			if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if reads != 1 || len(store.savedBlocks) != 1 || store.savedBlocks[0].Text != "Предварительный текст" || store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 || store.outcome.Source != nil {
				t.Fatalf("invalid presentation leaked data or partially published a batch: %+v", store)
			}
		})
	}
}

func TestEngineCitationsRequireActualQuestionSearch(t *testing.T) {
	for _, search := range []bool{false, true} {
		t.Run(map[bool]string{false: "no search", true: "search in earlier step"}[search], func(t *testing.T) {
			store := &engineStore{}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
				if search && step == 1 {
					return ModelResult{Output: []ModelItem{{ID: "search", Type: "web_search_call", Raw: json.RawMessage(`{"id":"search","type":"web_search_call","status":"completed"}`)}, engineTool("read", "get_data_overview", `{}`)}}, nil
				}
				return engineFinal(emit, "Финансовые данные. https://model-invented.example")
			}}
			if err := engineForTest(t, store, model, engineNoData(), engineLimits()).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if store.outcome.ErrorCode != "" || !store.outcome.Charged {
				t.Fatal(store.outcome)
			}
			if !search {
				if len(store.outcome.Blocks) != 1 {
					t.Fatal("citation promoted without an actual search")
				}
				return
			}
			if len(store.outcome.Blocks) != 2 || !strings.Contains(store.outcome.Blocks[1].Text, "https://example.org") || strings.Contains(store.outcome.Blocks[1].Text, "model-invented") || len(store.savedBlocks) != 2 || store.savedBlocks[1].Text != store.outcome.Blocks[1].Text {
				t.Fatalf("sources were not confirmed, question-scoped and durable: %+v", store.outcome)
			}
		})
	}
}

func TestEngineMalformedCitationStillCountsObservedSearch(t *testing.T) {
	store := &engineStore{}
	observer := &engineObserver{}
	limits := engineLimits()
	limits.Observer = observer
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
		if err := emit(ModelDelta{Text: "Частичный ответ"}); err != nil {
			return ModelResult{}, err
		}
		message := engineText("Частичный ответ")
		message.Raw = json.RawMessage(strings.ReplaceAll(string(message.Raw), "https://example.org", "javascript:private-invalid"))
		return ModelResult{Output: []ModelItem{{ID: "search", Type: "web_search_call", Raw: json.RawMessage(`{"id":"search","type":"web_search_call","status":"completed"}`)}, message}}, nil
	}}
	if err := engineForTest(t, store, model, engineNoData(), limits).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if observer.searches != 1 || observer.finished != 1 || observer.code != "MODEL_ERROR" || store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 || store.outcome.Source != nil {
		t.Fatalf("malformed citation lost search accounting or leaked output: %+v %+v", observer, store.outcome)
	}
}

func TestEngineMultiplePresentationsAndReadsKeepModelOrder(t *testing.T) {
	store := &engineStore{}
	reads := 0
	data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		reads++
		return engineAggregateForPresentation(), nil
	}}
	model := &engineModel{script: func(_ context.Context, request ModelRequest, _ func(ModelDelta) error, step int) (ModelResult, error) {
		switch step {
		case 1:
			return ModelResult{Output: []ModelItem{engineTool("read", "aggregate_operations", `{"all_time":true,"filter":{"q":"Самокат"}}`)}}, nil
		case 2:
			return ModelResult{Output: []ModelItem{engineTool("show-1", "present_result", `{"result_id":"read","view":"rows"}`), engineTool("aux-read", "get_data_overview", `{}`), engineTool("show-2", "present_result", `{"result_id":"read","view":"rows"}`)}}, nil
		default:
			if len(store.savedBlocks) != 2 || store.savedBlocks[0].Type != "rows" || string(store.savedBlocks[0].Body) != string(store.savedBlocks[1].Body) {
				t.Fatalf("presentations were not durable in order: %+v", store.savedBlocks)
			}
			for i, want := range []string{"show-1", "aux-read", "show-2"} {
				var output struct {
					CallID string `json:"call_id"`
				}
				if json.Unmarshal(request.Input[len(request.Input)-3+i], &output) != nil || output.CallID != want {
					t.Fatalf("continuation output %d did not retain call order: %s", i, request.Input[len(request.Input)-3+i])
				}
			}
			// A valid structured-only answer can finish without synthetic prose.
			return ModelResult{}, nil
		}
	}}
	if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || store.outcome.ErrorCode != "" || !store.outcome.Charged || len(store.outcome.Blocks) != 2 || len(store.outcome.Chips) != 1 {
		t.Fatalf("structured answer lost blocks or duplicated chips: reads=%d outcome=%+v", reads, store.outcome)
	}
}
