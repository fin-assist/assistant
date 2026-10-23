package generation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fin-assist/assistant/internal/tooldata"
)

type offTopicCommitFailure struct{ store *engineStore }

func (tx offTopicCommitFailure) Run(ctx context.Context, fn func(context.Context) error) error {
	if err := tx.store.Run(ctx, fn); err != nil {
		return err
	}
	return errors.New("commit failed")
}

func TestOffTopicDailyObservationRequiresNewCommittedTransition(t *testing.T) {
	for _, scenario := range []string{"commit failure", "already finished"} {
		t.Run(scenario, func(t *testing.T) {
			store := &engineStore{finished: scenario == "already finished"}
			observer := &engineObserver{}
			limits := engineLimits()
			limits.Observer = observer
			model := &engineModel{script: func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error) {
				return ModelResult{Output: []ModelItem{engineTool("off", "off_topic", `{}`)}}, nil
			}}
			engine := engineForTest(t, store, model, engineData{}, limits)
			if scenario == "commit failure" {
				engine.transactions = offTopicCommitFailure{store}
			}
			err := engine.Run(engineContext(t), engineTask())
			if (scenario == "commit failure") != (err != nil) {
				t.Fatalf("unexpected error: %v", err)
			}
			if observer.finished != 0 || len(observer.offTopicCounts) != 0 {
				t.Fatalf("uncommitted/duplicate metric: %+v", observer)
			}
		})
	}
}

func offTopicSearch(id string) ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": id, "type": "web_search_call"})
	return ModelItem{ID: id, Type: "web_search_call", Raw: raw}
}

func offTopicText(text string) ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{map[string]any{"type": "url_citation", "url": "javascript:PRIVATE"}}}}})
	return ModelItem{ID: "message", Type: "message", Raw: raw}
}

func TestEngineOffTopicTakesPriority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		text     string
		extra    []ModelItem
		searches int
	}{
		{name: "alone"},
		{name: "streamed text", text: "PRIVATE unrelated draft"},
		{name: "completed search and invalid citation", text: "PRIVATE search draft", extra: []ModelItem{offTopicSearch("search")}, searches: 1},
		{name: "read", extra: []ModelItem{engineTool("read", "get_data_overview", `{}`)}},
		{name: "invalid read arguments", extra: []ModelItem{engineTool("read", "find_operations", `{"user_id":"PRIVATE"}`)}},
		{name: "presentation", extra: []ModelItem{engineTool("show", "present_result", `{"result_id":"foreign","view":"rows"}`)}},
		{name: "invalid presentation arguments", extra: []ModelItem{engineTool("show", "present_result", `{"amount":42}`)}},
		{name: "cannot answer", extra: []ModelItem{engineTool("cannot", "cannot_answer", `{}`)}},
		{name: "invalid cannot answer arguments", extra: []ModelItem{engineTool("cannot", "cannot_answer", `{"reason":"PRIVATE"}`)}},
		{name: "unknown function and malformed arguments", extra: []ModelItem{engineTool("unknown", "PRIVATE_unknown", `not-json`)}},
		{name: "multiple terminal signals", extra: []ModelItem{engineTool("second-off", "off_topic", " { } ")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &engineStore{}
			observer := &engineObserver{}
			limits := engineLimits()
			limits.Observer = observer
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				t.Fatal("off_topic executed a financial RPC")
				return tooldata.Result{}, nil
			}}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
				if step != 1 {
					t.Fatal("off_topic continued the model cycle")
				}
				output := append([]ModelItem{}, tc.extra...)
				if tc.text != "" {
					if err := emit(ModelDelta{Text: tc.text}); err != nil {
						return ModelResult{}, err
					}
					if store.appendText != tc.text {
						t.Fatal("streamed text was not durable before callback return")
					}
					output = append(output, offTopicText(tc.text))
				}
				output = append(output, engineTool("off", "off_topic", `{}`))
				return ModelResult{Output: output}, nil
			}}
			if err := engineForTest(t, store, model, data, limits).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if store.outcome.ErrorCode != "OFF_TOPIC" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 || store.outcome.Source != nil || store.finishCount != 1 || model.network != 1 {
				t.Fatalf("invalid terminal outcome: %+v, finish=%d network=%d", store.outcome, store.finishCount, model.network)
			}
			if observer.searches != tc.searches || strings.Join(observer.functions, ",") != "off_topic" || observer.finished != 1 || observer.code != "OFF_TOPIC" || len(observer.offTopicCounts) != 1 || observer.offTopicCounts[0] != 1 {
				t.Fatalf("invalid terminal accounting: %+v", observer)
			}
		})
	}
}

func TestEngineInvalidOffTopicFailsClosed(t *testing.T) {
	for _, args := range []string{"", `null`, `[]`, `true`, `"{}"`, `{"reason":"PRIVATE"}`, `{"x":1,"x":2}`, `{} {}`, `{`} {
		t.Run(args, func(t *testing.T) {
			store := &engineStore{}
			model := &engineModel{script: func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error) {
				return ModelResult{Output: []ModelItem{engineTool("read", "get_data_overview", `{}`), engineTool("off", "off_topic", args), engineTool("valid-off", "off_topic", `{}`)}}, nil
			}}
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				t.Fatal("invalid off_topic allowed another tool to execute")
				return tooldata.Result{}, nil
			}}
			if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || store.finishCount != 1 {
				t.Fatalf("invalid off_topic was accepted: %+v", store.outcome)
			}
		})
	}
}

func TestEngineOffTopicClearsEarlierPresentationAndSource(t *testing.T) {
	store := &engineStore{}
	reads := 0
	data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		reads++
		return engineAggregateForPresentation(), nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		switch step {
		case 1:
			return ModelResult{Output: []ModelItem{engineTool("read", "aggregate_operations", `{"all_time":true}`)}}, nil
		case 2:
			if err := emit(ModelDelta{Text: "PRIVATE earlier draft"}); err != nil {
				return ModelResult{}, err
			}
			return ModelResult{Output: []ModelItem{engineText("PRIVATE earlier draft"), engineTool("show", "present_result", `{"result_id":"read","view":"rows"}`)}}, nil
		default:
			if len(store.savedBlocks) < 2 || store.savedBlocks[1].Type != "rows" {
				t.Fatal("prior presentation was not durable")
			}
			return ModelResult{Output: []ModelItem{engineTool("off", "off_topic", `{}`), engineTool("ignored", "get_data_overview", `{}`)}}, nil
		}
	}}
	if err := engineForTest(t, store, model, data, engineLimits()).Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || model.network != 3 || store.outcome.ErrorCode != "OFF_TOPIC" || store.outcome.Charged || len(store.outcome.Blocks) != 0 || len(store.outcome.Chips) != 0 || store.outcome.Source != nil || store.finishCount != 1 {
		t.Fatalf("prior data leaked into terminal outcome: %+v reads=%d network=%d", store.outcome, reads, model.network)
	}
}

func TestEngineOffTopicDoesNotBypassEnvelopeOrBudgets(t *testing.T) {
	for _, scenario := range []string{"duplicate output ID", "duplicate call ID", "reused call ID", "raw mismatch", "search cap", "function cap", "step cap", "ignored sibling function cap"} {
		t.Run(scenario, func(t *testing.T) {
			store := &engineStore{}
			observer := &engineObserver{}
			limits := engineLimits()
			limits.Observer = observer
			if scenario == "function cap" || scenario == "ignored sibling function cap" {
				limits.Budget.MaxFunctions = 1
				limits.ParallelFunctions = 1
			}
			if scenario == "step cap" {
				limits.Budget.MaxSteps = 1
			}
			reads := 0
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				reads++
				return tooldata.Result{JSON: json.RawMessage(`{}`)}, nil
			}}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, _ func(ModelDelta) error, step int) (ModelResult, error) {
				if step == 1 && (scenario == "function cap" || scenario == "step cap" || scenario == "reused call ID") {
					return ModelResult{Output: []ModelItem{engineTool("prior", "get_data_overview", `{}`)}}, nil
				}
				off := engineTool("off", "off_topic", `{}`)
				output := []ModelItem{off}
				switch scenario {
				case "duplicate output ID":
					output = append(output, off)
				case "duplicate call ID":
					other := engineTool("other", "unknown", `PRIVATE`)
					other.CallID = off.CallID
					var raw map[string]any
					_ = json.Unmarshal(other.Raw, &raw)
					raw["call_id"] = off.CallID
					other.Raw, _ = json.Marshal(raw)
					output = append(output, other)
				case "reused call ID":
					output = []ModelItem{engineTool("prior", "off_topic", `{}`)}
				case "raw mismatch":
					output[0].Arguments = `{"PRIVATE":true}`
				case "search cap":
					for i := 0; i <= limits.MaxSearchCalls; i++ {
						output = append(output, offTopicSearch(fmt.Sprint("search", i)))
					}
				case "ignored sibling function cap":
					for i := 0; i < 20; i++ {
						output = append(output, engineTool(fmt.Sprint("ignored", i), "PRIVATE", `not-json`))
					}
				}
				return ModelResult{Output: output}, nil
			}}
			if err := engineForTest(t, store, model, data, limits).Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			want := "MODEL_ERROR"
			if scenario == "ignored sibling function cap" {
				want = "OFF_TOPIC"
			}
			wantReads := 0
			if scenario == "function cap" || scenario == "step cap" || scenario == "reused call ID" {
				wantReads = 1
			}
			if store.outcome.ErrorCode != want || store.outcome.Charged || reads != wantReads || len(store.outcome.Blocks) != 0 {
				t.Fatalf("invalid bounded outcome: %+v reads=%d", store.outcome, reads)
			}
			if scenario == "search cap" && observer.searches != limits.MaxSearchCalls+1 {
				t.Fatal("actual provider searches were not counted")
			}
		})
	}
}

func TestOffTopicToolSchemaIsStrictAndAlwaysPresent(t *testing.T) {
	for _, searches := range []int{0, 3} {
		found := 0
		for _, raw := range toolDefinitions(searches) {
			var tool struct {
				Name       string `json:"name"`
				Parameters struct {
					Type                 string                     `json:"type"`
					AdditionalProperties bool                       `json:"additionalProperties"`
					Properties           map[string]json.RawMessage `json:"properties"`
				} `json:"parameters"`
			}
			if json.Unmarshal(raw, &tool) != nil {
				t.Fatal("invalid tool schema")
			}
			if tool.Name == "off_topic" {
				found++
				if tool.Parameters.Type != "object" || tool.Parameters.AdditionalProperties || tool.Parameters.Properties == nil || len(tool.Parameters.Properties) != 0 {
					t.Fatalf("nonempty off_topic arguments: %s", raw)
				}
			}
		}
		if found != 1 || !strings.Contains(systemInstructions, "off_topic") {
			t.Fatal("off_topic is missing from the model contract")
		}
	}
}
