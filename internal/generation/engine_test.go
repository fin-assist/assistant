package generation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/tooldata"
)

type engineTxKey struct{}
type engineStore struct {
	history                                           []HistoryPair
	historyErr, appendErr, finishErr, guardErr, txErr error
	deleted                                           bool
	appendText                                        string
	appendCount, finishCount                          int
	outcome                                           Outcome
	finished                                          bool
	guards                                            []string
	finishContextErr                                  error
	finishedAt                                        time.Time
	historyLimit                                      int
	mu                                                sync.Mutex
}

func (s *engineStore) History(_ context.Context, _ domain.GenerationTask, limit int) ([]HistoryPair, error) {
	s.historyLimit = limit
	return s.history, s.historyErr
}
func (s *engineStore) Append(ctx context.Context, _ domain.GenerationTask, index int, text string) error {
	if ctx.Value(engineTxKey{}) != true || index != 0 {
		return errors.New("write outside transaction")
	}
	if s.appendErr != nil {
		return s.appendErr
	}
	s.appendText += text
	s.appendCount++
	return nil
}
func (s *engineStore) Finish(ctx context.Context, _ domain.GenerationTask, outcome Outcome, at time.Time) (bool, error) {
	if ctx.Value(engineTxKey{}) != true {
		return false, errors.New("finish outside transaction")
	}
	s.finishCount++
	s.finishContextErr = ctx.Err()
	if s.finishErr != nil {
		return false, s.finishErr
	}
	if s.finished {
		return false, nil
	}
	s.finished = true
	s.outcome = outcome
	s.finishedAt = at
	return true, nil
}
func (s *engineStore) Expired(context.Context, time.Time, int) ([]domain.GenerationTask, error) {
	return nil, nil
}
func (s *engineStore) Run(ctx context.Context, fn func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.txErr != nil {
		return s.txErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(context.WithValue(ctx, engineTxKey{}, true))
}
func (s *engineStore) Guard(ctx context.Context, user string) (bool, error) {
	if ctx.Value(engineTxKey{}) != true {
		return false, errors.New("guard outside transaction")
	}
	s.guards = append(s.guards, user)
	return !s.deleted, s.guardErr
}

type engineData struct {
	invoke func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error)
}

func (d engineData) Overview(ctx context.Context, s tooldata.Subject) (tooldata.Result, error) {
	return d.invoke(ctx, "get_data_overview", s, nil)
}
func (d engineData) Aggregate(ctx context.Context, s tooldata.Subject, a tooldata.AggregateArgs) (tooldata.Result, error) {
	return d.invoke(ctx, "aggregate_operations", s, a)
}
func (d engineData) Find(ctx context.Context, s tooldata.Subject, a tooldata.FindArgs) (tooldata.Result, error) {
	return d.invoke(ctx, "find_operations", s, a)
}
func (d engineData) Analytics(ctx context.Context, s tooldata.Subject, a tooldata.AnalyticsArgs) (tooldata.Result, error) {
	return d.invoke(ctx, "get_analytics", s, a)
}
func (d engineData) Periods(ctx context.Context, s tooldata.Subject, a tooldata.PeriodsArgs) (tooldata.Result, error) {
	return d.invoke(ctx, "list_periods", s, a)
}

type engineModel struct {
	script            func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error)
	before            func(context.Context, ModelRequest, func(ModelDelta) error, func([]byte) error) (ModelResult, error)
	requests          []ModelRequest
	attempts, network int
}

func (m *engineModel) Stream(ctx context.Context, r ModelRequest, emit func(ModelDelta) error, reserve func([]byte) error) (ModelResult, error) {
	m.attempts++
	if m.before != nil {
		return m.before(ctx, r, emit, reserve)
	}
	raw, _ := json.Marshal(struct {
		Model        string            `json:"model"`
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
	}{Model: "test-model", Instructions: r.Instructions, Input: r.Input, Tools: r.Tools})
	if err := reserve(raw); err != nil {
		return ModelResult{}, err
	}
	m.requests = append(m.requests, r)
	m.network++
	return m.script(ctx, r, emit, m.network)
}

type engineObserver struct {
	mu                        sync.Mutex
	steps, searches, finished int
	functions                 []string
	code                      string
	charged                   bool
}

func (o *engineObserver) Step(Usage) { o.mu.Lock(); defer o.mu.Unlock(); o.steps++ }
func (o *engineObserver) Function(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.functions = append(o.functions, name)
}
func (o *engineObserver) Searches(n int) { o.mu.Lock(); defer o.mu.Unlock(); o.searches += n }
func (o *engineObserver) Finished(code string, charged bool, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.finished++
	o.code = code
	o.charged = charged
}

func engineLimits() EngineLimits {
	return EngineLimits{Budget: BudgetLimits{InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxSteps: 8, MaxFunctions: 16}, MaxOutputTokens: 4096, ParallelFunctions: 4, MaxSearchCalls: 3}
}
func engineTask() domain.GenerationTask {
	return domain.GenerationTask{UserID: "server-user", QuestionID: "q", AnswerID: "a", Text: "Сколько я потратил?", TransferMode: "without", TimeZone: "Europe/Moscow"}
}
func engineContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func engineForTest(t *testing.T, store *engineStore, model *engineModel, data engineData, limits EngineLimits) *Engine {
	t.Helper()
	e, err := New(store, store, store, model, data, limits, func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func engineTool(id, name, args string) ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": id, "type": "function_call", "call_id": id, "name": name, "arguments": args, "status": "completed"})
	return ModelItem{ID: id, Type: "function_call", CallID: id, Name: name, Arguments: args, Raw: raw}
}
func engineText(text string) ModelItem {
	raw, _ := json.Marshal(map[string]any{"id": "message", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.org"}}}}})
	return ModelItem{ID: "message", Type: "message", Raw: raw}
}
func engineFinal(emit func(ModelDelta) error, text string) (ModelResult, error) {
	if err := emit(ModelDelta{Text: text}); err != nil {
		return ModelResult{}, err
	}
	return ModelResult{Output: []ModelItem{engineText(text)}, Usage: Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}}, nil
}
func engineNoData() engineData {
	return engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
		return tooldata.Result{JSON: json.RawMessage(`{}`)}, nil
	}}
}

func TestEngineGeneralTextDurableBeforeCallbackAndCAS(t *testing.T) {
	store := &engineStore{history: []HistoryPair{{Question: "prior question", Answer: "prior answer"}}}
	observer := &engineObserver{}
	limits := engineLimits()
	limits.Observer = observer
	model := &engineModel{script: func(_ context.Context, r ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
		if len(r.Input) != 3 || !strings.Contains(string(r.Input[0]), "prior question") || !strings.Contains(string(r.Input[1]), "prior answer") || !strings.Contains(string(r.Input[2]), "Сколько") {
			t.Errorf("context %+v", r.Input)
		}
		if r.MaxOutputTokens != 4096 || r.MaxToolCalls != 3 || !strings.Contains(r.Instructions, "НИКОГДА") {
			t.Error("missing policy")
		}
		before := store.appendText
		if err := emit(ModelDelta{Text: "Финансы "}); err != nil {
			return ModelResult{}, err
		}
		if store.appendText != before+"Финансы " {
			t.Error("callback returned before durability")
		}
		if err := emit(ModelDelta{Text: "важны."}); err != nil {
			return ModelResult{}, err
		}
		return ModelResult{Output: []ModelItem{engineText("Финансы важны.")}}, nil
	}}
	e := engineForTest(t, store, model, engineNoData(), limits)
	if err := e.Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if !store.outcome.Charged || store.outcome.ErrorCode != "" || len(store.outcome.Blocks) != 1 || store.outcome.Blocks[0].Text != store.appendText || store.outcome.Source != nil || store.appendCount != 2 || store.finishCount != 1 || observer.finished != 1 || !observer.charged {
		t.Fatalf("outcome %+v store %+v observer %+v", store.outcome, store, observer)
	}
	for _, user := range store.guards {
		if user != "server-user" {
			t.Fatalf("foreign subject %q", user)
		}
	}
	if store.finishedAt.Year() != 2026 || store.historyLimit > 65536 {
		t.Fatal("clock or history cap missing")
	}
	// A prior terminal transition wins; no second terminal metric is emitted.
	if err := e.Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if observer.finished != 1 {
		t.Fatal("CAS loser counted as completed")
	}
}

func TestEngineToolCycleRawContinuationAndSource(t *testing.T) {
	store := &engineStore{}
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	calls := 0
	data := engineData{invoke: func(_ context.Context, name string, s tooldata.Subject, a any) (tooldata.Result, error) {
		calls++
		if name != "aggregate_operations" || s.UserID != "server-user" || s.TransferMode != "without" {
			t.Errorf("scope %s %+v", name, s)
		}
		args := a.(tooldata.AggregateArgs)
		if args.Limit != 10 || args.Filter.Query != "merchant" || args.Filter.Kind != "expense" {
			t.Error(args)
		}
		return tooldata.Result{JSON: json.RawMessage(`{"total_amount_minor":"9007199254740993"}`), Evidence: tooldata.Evidence{Source: &tooldata.Source{CalculatedAt: &at, Range: &tooldata.Range{Start: at.AddDate(0, -1, 0), End: at}}}}, nil
	}}
	function := engineTool("call-1", "aggregate_operations", `{"filter":{"q":"merchant","kind":"expense"},"limit":10}`)
	reason := ModelItem{ID: "reason", Type: "reasoning", Raw: json.RawMessage(`{"id":"reason","type":"reasoning","encrypted_content":"opaque","future":{"x":1}}`)}
	model := &engineModel{script: func(_ context.Context, r ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			return ModelResult{Output: []ModelItem{reason, function}}, nil
		}
		if len(r.Input) != 4 || !strings.Contains(string(r.Input[1]), "encrypted_content") || !strings.Contains(string(r.Input[2]), "call-1") || !strings.Contains(string(r.Input[3]), "9007199254740993") {
			t.Errorf("continuation %+v", r.Input)
		}
		return engineFinal(emit, "Ответ по данным")
	}}
	e := engineForTest(t, store, model, data, engineLimits())
	if err := e.Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || model.network != 2 || !store.outcome.Charged || store.outcome.Source == nil || !store.outcome.Source.CalculatedAt.Equal(at) || store.outcome.Source.TransferMode != "without" {
		t.Fatalf("outcome %+v calls %d", store.outcome, calls)
	}
}

func TestEngineNoDataAndUnrelatedLocksPolicy(t *testing.T) {
	for _, name := range []string{"get_data_overview", "list_periods", "aggregate_operations", "find_operations", "get_analytics"} {
		for _, noData := range []bool{false, true} {
			t.Run(fmt.Sprint(name, noData), func(t *testing.T) {
				store := &engineStore{}
				data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
					return tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{NoData: noData, LockedPaths: []string{"blocks[0].data.tiles[1]"}}}, nil
				}}
				model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
					if step == 1 {
						return ModelResult{Output: []ModelItem{engineTool("call", name, `{}`)}}, nil
					}
					return engineFinal(emit, "Ответ")
				}}
				e := engineForTest(t, store, model, data, engineLimits())
				if err := e.Run(engineContext(t), engineTask()); err != nil {
					t.Fatal(err)
				}
				wantCharged := !noData || name == "get_data_overview" || name == "list_periods"
				if store.outcome.Charged != wantCharged || store.outcome.ErrorCode != "" {
					t.Fatalf("outcome %+v want charged %v", store.outcome, wantCharged)
				}
			})
		}
	}
}

func TestEngineRejectsWholeInvalidFunctionBatchBeforeRPC(t *testing.T) {
	for _, call := range []ModelItem{
		engineTool("bad", "unknown", `{}`), engineTool("bad", "find_operations", `{"user_id":"victim"}`), engineTool("bad", "find_operations", `{"filter":{"transfer_mode":"with"}}`), engineTool("bad", "find_operations", `{"limit":1,"limit":2}`), engineTool("bad", "find_operations", `{"limit":1.5}`), engineTool("bad", "find_operations", `{"filter":{"q":null}}`), engineTool("bad", "get_analytics", `{"period":"month","period_key":"2026-Q3"}`), engineTool("bad", "aggregate_operations", `{"group_by":["month","month"]}`), engineTool("bad", "find_operations", `{"filter":{"from":"2026-10-01T00:00:00Z"},"all_time":true}`),
	} {
		t.Run(call.Name+call.Arguments, func(t *testing.T) {
			store := &engineStore{}
			var rpc atomic.Int32
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				rpc.Add(1)
				return tooldata.Result{}, nil
			}}
			model := &engineModel{script: func(context.Context, ModelRequest, func(ModelDelta) error, int) (ModelResult, error) {
				return ModelResult{Output: []ModelItem{engineTool("valid", "get_data_overview", `{}`), call}}, nil
			}}
			e := engineForTest(t, store, model, data, engineLimits())
			if err := e.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if rpc.Load() != 0 || store.outcome.ErrorCode != "MODEL_ERROR" {
				t.Fatalf("outcome %+v RPC %d", store.outcome, rpc.Load())
			}
		})
	}
}

func TestEngineParallelBoundAndDeterministicToolOrder(t *testing.T) {
	store := &engineStore{}
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active, maximum atomic.Int32
	data := engineData{invoke: func(ctx context.Context, _ string, _ tooldata.Subject, _ any) (tooldata.Result, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); n > previous && !maximum.CompareAndSwap(previous, n); previous = maximum.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return tooldata.Result{}, ctx.Err()
		}
		return tooldata.Result{JSON: json.RawMessage(`{"value":"ok"}`)}, nil
	}}
	model := &engineModel{script: func(_ context.Context, r ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			var output []ModelItem
			for i := 0; i < 4; i++ {
				output = append(output, engineTool(fmt.Sprint(i), "get_data_overview", `{}`))
			}
			return ModelResult{Output: output}, nil
		}
		for i := 0; i < 4; i++ {
			var value struct {
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(r.Input[5+i], &value) != nil || value.CallID != fmt.Sprint(i) {
				t.Errorf("tool results reordered %+v", r.Input)
			}
		}
		return engineFinal(emit, "Ответ")
	}}
	e := engineForTest(t, store, model, data, engineLimits())
	done := make(chan error, 1)
	go func() { done <- e.Run(engineContext(t), engineTask()) }()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("tools did not execute concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 4 || store.outcome.ErrorCode != "" {
		t.Fatalf("maximum %d outcome %+v", maximum.Load(), store.outcome)
	}
}

func TestEngineHistoryLocalTrimNeverRetriesAcceptedNetwork(t *testing.T) {
	for _, networkFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(networkFailure), func(t *testing.T) {
			store := &engineStore{history: []HistoryPair{{Question: strings.Repeat("old", 4000), Answer: strings.Repeat("answer", 1000)}, {Question: "new question", Answer: "new answer"}}}
			limits := engineLimits()
			limits.Budget.RequestBytesPerStep = 12_000
			model := &engineModel{script: func(_ context.Context, r ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
				if len(r.Input) != 3 || !strings.Contains(string(r.Input[0]), "new question") {
					t.Errorf("whole recent pair missing %+v", r.Input)
				}
				if networkFailure {
					return ModelResult{}, ErrBudgetExceeded
				}
				return engineFinal(emit, "Ответ")
			}}
			e := engineForTest(t, store, model, engineNoData(), limits)
			if err := e.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if model.attempts != 2 || model.network != 1 {
				t.Fatalf("unsafe network retry attempts=%d network=%d", model.attempts, model.network)
			}
			if (store.outcome.ErrorCode == "MODEL_ERROR") != networkFailure {
				t.Fatalf("outcome %+v", store.outcome)
			}
		})
	}
}

func TestEngineFailureClassificationAndPartialClearing(t *testing.T) {
	for name, failure := range map[string]error{"model": errors.New("network"), "refusal": ErrRefused, "cancel": context.Canceled} {
		t.Run(name, func(t *testing.T) {
			store := &engineStore{}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
				if err := emit(ModelDelta{Text: "partial"}); err != nil {
					return ModelResult{}, err
				}
				return ModelResult{}, failure
			}}
			e := engineForTest(t, store, model, engineNoData(), engineLimits())
			if err := e.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			want := "MODEL_ERROR"
			if name == "refusal" {
				want = "CANNOT_ANSWER"
			}
			if store.appendText != "partial" || store.outcome.ErrorCode != want || store.outcome.Charged || len(store.outcome.Blocks) > 0 {
				t.Fatalf("outcome %+v store %+v", store.outcome, store)
			}
		})
	}
	for _, kind := range []string{"empty", "cannot", "data error", "data panic", "append error", "history error", "bad model text", "missing reserve", "duplicate calls"} {
		t.Run(kind, func(t *testing.T) {
			store := &engineStore{}
			data := engineNoData()
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
				switch kind {
				case "empty":
					return ModelResult{}, nil
				case "cannot":
					return ModelResult{Output: []ModelItem{engineTool("call", "cannot_answer", `{}`)}}, nil
				case "data error", "data panic":
					return ModelResult{Output: []ModelItem{engineTool("call", "get_data_overview", `{}`)}}, nil
				case "bad model text":
					return ModelResult{Output: []ModelItem{engineText("not streamed")}}, nil
				case "duplicate calls":
					return ModelResult{Output: []ModelItem{engineTool("a", "get_data_overview", `{}`), engineTool("b", "get_data_overview", `{}`)}}, nil
				}
				return engineFinal(emit, "Ответ")
			}}
			if kind == "data error" {
				data.invoke = func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
					return tooldata.Result{}, ErrRefused
				}
			}
			if kind == "data panic" {
				data.invoke = func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) { panic("private") }
			}
			if kind == "append error" {
				store.appendErr = errors.New("SQL failed")
			}
			if kind == "history error" {
				store.historyErr = errors.New("read failed")
			}
			if kind == "missing reserve" {
				model.before = func(context.Context, ModelRequest, func(ModelDelta) error, func([]byte) error) (ModelResult, error) {
					return ModelResult{}, nil
				}
			}
			e := engineForTest(t, store, model, data, engineLimits())
			if err := e.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			want := "MODEL_ERROR"
			if kind == "empty" || kind == "cannot" {
				want = "CANNOT_ANSWER"
			}
			if store.outcome.ErrorCode != want {
				t.Fatalf("outcome %+v", store.outcome)
			}
		})
	}
}

func TestEngineSearchStepsAndFunctionCaps(t *testing.T) {
	for _, scenario := range []string{"search exhaustion", "search overrun", "steps", "function batch"} {
		t.Run(scenario, func(t *testing.T) {
			store := &engineStore{}
			limits := engineLimits()
			var calls atomic.Int32
			data := engineData{invoke: func(context.Context, string, tooldata.Subject, any) (tooldata.Result, error) {
				calls.Add(1)
				return tooldata.Result{JSON: json.RawMessage(`{}`)}, nil
			}}
			model := &engineModel{script: func(_ context.Context, r ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
				switch scenario {
				case "search exhaustion":
					if step == 1 {
						var output []ModelItem
						for i := 0; i < 3; i++ {
							id := fmt.Sprint("search", i)
							output = append(output, ModelItem{ID: id, Type: "web_search_call", Raw: json.RawMessage(fmt.Sprintf(`{"id":%q,"type":"web_search_call"}`, id))})
						}
						output = append(output, engineTool("data", "get_data_overview", `{}`))
						return ModelResult{Output: output}, nil
					}
					if r.MaxToolCalls != 0 || strings.Contains(string(mustEngineJSON(r.Tools)), "web_search") {
						t.Error("search still offered after cap")
					}
					return engineFinal(emit, "Ответ")
				case "search overrun":
					var output []ModelItem
					for i := 0; i < 4; i++ {
						id := fmt.Sprint("search", i)
						output = append(output, ModelItem{ID: id, Type: "web_search_call", Raw: json.RawMessage(fmt.Sprintf(`{"id":%q,"type":"web_search_call"}`, id))})
					}
					return ModelResult{Output: output}, nil
				case "steps":
					return ModelResult{Output: []ModelItem{engineTool(fmt.Sprint("call", step), "get_data_overview", `{}`)}}, nil
				case "function batch":
					return ModelResult{Output: []ModelItem{engineTool("a", "get_data_overview", `{}`), engineTool("b", "list_periods", `{}`)}}, nil
				}
				return ModelResult{}, nil
			}}
			if scenario == "steps" {
				limits.Budget.MaxSteps = 2
			}
			if scenario == "function batch" {
				limits.Budget.MaxFunctions = 1
				limits.ParallelFunctions = 1
			}
			e := engineForTest(t, store, model, data, limits)
			if err := e.Run(engineContext(t), engineTask()); err != nil {
				t.Fatal(err)
			}
			if scenario == "search exhaustion" {
				if store.outcome.ErrorCode != "" {
					t.Fatal(store.outcome)
				}
			} else if store.outcome.ErrorCode != "MODEL_ERROR" {
				t.Fatal(store.outcome)
			}
			if scenario == "function batch" && calls.Load() != 0 {
				t.Fatal("partial batch escaped quota")
			}
			if scenario == "steps" && model.network != 2 {
				t.Fatalf("steps %d", model.network)
			}
		})
	}
}

func TestEngineDeletedCanceledAndFinishFailures(t *testing.T) {
	for _, kind := range []string{"deleted", "guard", "transaction", "finish", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			store := &engineStore{}
			ctx, cancel := context.WithCancel(engineContext(t))
			defer cancel()
			switch kind {
			case "deleted":
				store.deleted = true
			case "guard":
				store.guardErr = errors.New("guard failed")
			case "transaction":
				store.txErr = errors.New("tx failed")
			case "finish":
				store.finishErr = errors.New("finish failed")
			case "cancelled":
				cancel()
			}
			model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, _ int) (ModelResult, error) {
				return engineFinal(emit, "Ответ")
			}}
			e := engineForTest(t, store, model, engineNoData(), engineLimits())
			err := e.Run(ctx, engineTask())
			if kind == "cancelled" {
				if err != nil || store.outcome.ErrorCode != "MODEL_ERROR" || store.finishContextErr != nil {
					t.Fatalf("detached finish %+v %v", store, err)
				}
			} else if err == nil {
				t.Fatal("write failure swallowed")
			}
			if kind == "deleted" && (store.appendCount != 0 || store.finishCount != 0 || !errors.Is(err, ErrStopped)) {
				t.Fatalf("deleted write %+v %v", store, err)
			}
		})
	}
}

func mustEngineJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestEngineConstructorAndHistoryValidation(t *testing.T) {
	store := &engineStore{}
	model := &engineModel{}
	data := engineNoData()
	now := func() time.Time { return time.Now() }
	for _, bad := range []string{"repository", "transactions", "tombstones", "model", "data", "clock", "output", "parallel", "parallel max", "search", "budget"} {
		t.Run(bad, func(t *testing.T) {
			var repo Repository = store
			var tx Transactions = store
			var tomb Tombstones = store
			var m Model = model
			var d tooldata.Data = data
			clock := now
			limits := engineLimits()
			switch bad {
			case "repository":
				repo = nil
			case "transactions":
				tx = nil
			case "tombstones":
				tomb = nil
			case "model":
				m = nil
			case "data":
				d = nil
			case "clock":
				clock = nil
			case "output":
				limits.MaxOutputTokens = 15
			case "parallel":
				limits.ParallelFunctions = 0
			case "parallel max":
				limits.ParallelFunctions = 17
			case "search":
				limits.MaxSearchCalls = 0
			case "budget":
				limits.Budget.HistoryTokens = 0
			}
			if _, err := New(repo, tx, tomb, m, d, limits, clock); !errors.Is(err, ErrInvalidBudget) {
				t.Fatal(err)
			}
		})
	}
	for _, pairs := range [][]HistoryPair{{{Question: "", Answer: "answer"}}, {{Question: "question", Answer: " "}}, {{Question: string([]byte{255}), Answer: "answer"}}} {
		if _, err := historyWindow(pairs, 16000); err == nil {
			t.Fatal("invalid saved history accepted")
		}
	}
	pairs, err := historyWindow([]HistoryPair{{Question: strings.Repeat("old", 1000), Answer: "answer"}, {Question: "recent", Answer: "answer"}}, 100)
	if err != nil || len(pairs) != 1 || pairs[0].Question != "recent" {
		t.Fatalf("whole-pair selection %+v %v", pairs, err)
	}
}

func TestEngineSourceUnionOldestCalculationAndFindFallback(t *testing.T) {
	store := &engineStore{}
	early := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	middle := early.AddDate(0, 1, 0)
	late := early.AddDate(0, 2, 0)
	calculated := late.Add(time.Hour)
	data := engineData{invoke: func(_ context.Context, name string, _ tooldata.Subject, _ any) (tooldata.Result, error) {
		source := &tooldata.Source{Range: &tooldata.Range{Start: early, End: middle}, CalculatedAt: &calculated}
		if name == "find_operations" {
			source = &tooldata.Source{Range: &tooldata.Range{Start: middle, End: late}}
		}
		return tooldata.Result{JSON: json.RawMessage(`{}`), Evidence: tooldata.Evidence{Source: source}}, nil
	}}
	model := &engineModel{script: func(_ context.Context, _ ModelRequest, emit func(ModelDelta) error, step int) (ModelResult, error) {
		if step == 1 {
			return ModelResult{Output: []ModelItem{engineTool("aggregate", "aggregate_operations", `{}`), engineTool("find", "find_operations", `{}`)}}, nil
		}
		return engineFinal(emit, "Ответ")
	}}
	e := engineForTest(t, store, model, data, engineLimits())
	if err := e.Run(engineContext(t), engineTask()); err != nil {
		t.Fatal(err)
	}
	source := store.outcome.Source
	if source == nil || !source.From.Equal(early) || !source.To.Equal(late) || !source.CalculatedAt.Equal(calculated) {
		t.Fatalf("merged source %+v", source)
	}
	// An unknown range must not disappear behind a later, narrower known one.
	unknown, err := mergeSource(source, &tooldata.Source{}, "without", late)
	if err != nil || unknown.From != nil || unknown.To != nil || !unknown.CalculatedAt.Equal(late) {
		t.Fatalf("unknown range %+v %v", unknown, err)
	}
	known, err := mergeSource(unknown, &tooldata.Source{Range: &tooldata.Range{Start: early, End: middle}}, "without", calculated)
	if err != nil || known.From != nil || known.To != nil || !known.CalculatedAt.Equal(late) {
		t.Fatalf("narrowed source %+v %v", known, err)
	}
	if _, err := mergeSource(nil, &tooldata.Source{}, "without", time.Time{}); err == nil {
		t.Fatal("missing calculation time accepted")
	}
	if _, err := mergeSource(nil, &tooldata.Source{Range: &tooldata.Range{Start: late, End: early}}, "without", late); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestEngineStrictJSONAndToolArgumentBoundaries(t *testing.T) {
	for _, raw := range []string{`[]`, `null`, `{"x":1,"x":2}`, `{"x":{"y":1,"y":2}}`, `{"x":1} {}`, `{"x":`, `{"filter":{"USER_ID":"foreign"}}`, `{"limit":null}`, `{"Limit":1}`, `{"limit":"1"}`} {
		if _, err := parseTool(engineTool("call", "find_operations", raw)); err == nil {
			t.Fatalf("invalid JSON accepted %s", raw)
		}
	}
	for _, args := range []string{`{"filter":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z"}}`, `{"filter":{"amount_from_minor":2,"amount_to_minor":1}}`, `{"filter":{"amount_from_minor":-1}}`, `{"filter":{"kind":"future"}}`, `{"order":"future"}`, `{"limit":101}`} {
		if _, err := parseTool(engineTool("call", "find_operations", args)); err == nil {
			t.Fatalf("invalid args %s", args)
		}
	}
	for name, args := range map[string]string{
		"find_operations":      `{"filter":{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z","q":"merchant","category_id":"cat","selection":"s_merchant_1","kind":"income","amount_from_minor":0,"amount_to_minor":9223372036854775807},"order":"largest","limit":100}`,
		"aggregate_operations": `{"all_time":true,"group_by":["merchant","month"],"limit":200}`,
		"get_analytics":        `{"period":"year","period_key":"2026-Q3"}`,
		"list_periods":         `{"period":"quarter"}`,
	} {
		if _, err := parseTool(engineTool("call", name, args)); err != nil {
			t.Fatalf("valid args %s %s: %v", name, args, err)
		}
	}
}

func TestHistoryWindowExactEncodedArrayBoundaries(t *testing.T) {
	pairs := []HistoryPair{{Question: "old", Answer: "first"}, {Question: "<&>\u2028\n\"\\Русский 🌍", Answer: "Ответ с \"кавычками\" и &"}, {Question: "latest", Answer: "complete answer"}}
	// Make the complete serialized array end at an exact token boundary. A
	// budget smaller by one token must then remove its oldest complete pair.
	all := mustEngineJSON(historyInput(pairs))
	pairs[0].Question += strings.Repeat("x", (4-len(all)%4)%4)
	all = mustEngineJSON(historyInput(pairs))
	if len(all)%4 != 0 {
		t.Fatal("fixture is not on an exact four-byte boundary")
	}
	for tokens := 0; tokens <= EstimateTokens(all)+1; tokens++ {
		got, err := historyWindow(pairs, tokens)
		if err != nil {
			t.Fatal(err)
		}
		// Independent policy oracle: encode each candidate complete suffix as
		// one actual JSON array, including escaping, brackets and all commas.
		wantStart := 0
		for wantStart < len(pairs) && EstimateTokens(mustEngineJSON(historyInput(pairs[wantStart:]))) > tokens {
			wantStart++
		}
		want := pairs[wantStart:]
		if len(got) != len(want) {
			t.Fatalf("tokens=%d got %d pairs want %d", tokens, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tokens=%d pair[%d] differs: %+v != %+v", tokens, i, got[i], want[i])
			}
		}
	}
	got, err := historyWindow(pairs, len(all)/4)
	if err != nil || len(got) != len(pairs) {
		t.Fatalf("exact boundary: %+v %v", got, err)
	}
	got, err = historyWindow(pairs, len(all)/4-1)
	if err != nil || len(got) != len(pairs)-1 || got[0] != pairs[1] {
		t.Fatalf("one token below: %+v %v", got, err)
	}
}

func TestHistoryWindowKeepsOnlyContiguousNewestWholePairs(t *testing.T) {
	pairs := []HistoryPair{{Question: "small old", Answer: "small answer"}, {Question: strings.Repeat("oversized newest 🌍<&>", 1000), Answer: "newest answer"}}
	got, err := historyWindow(pairs, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("skipped oversized newest pair to keep old data: %+v %v", got, err)
	}
	for _, tokens := range []int{0, -1, int(^uint(0) >> 1), int(^uint(0)>>1)/4 + 1} {
		got, err = historyWindow(pairs, tokens)
		if err != nil {
			t.Fatal(err)
		}
		want := len(pairs)
		if tokens <= 0 {
			want = 0
		}
		if len(got) != want {
			t.Fatalf("tokens=%d got %d want %d", tokens, len(got), want)
		}
	}
	if got, err := historyWindow(nil, int(^uint(0)>>1)); err != nil || len(got) != 0 {
		t.Fatalf("empty input %+v %v", got, err)
	}
	// Validate every stored pair even when no history could be selected or a
	// large recent pair would terminate the reverse size scan immediately.
	pairs[0].Question = string([]byte{255})
	for _, tokens := range []int{0, 100} {
		if _, err := historyWindow(pairs, tokens); err == nil {
			t.Fatal("invalid older pair bypassed validation")
		}
	}
}

func TestHistoryWindowManyTinyPairsMatchesActualSerializedSize(t *testing.T) {
	pairs := make([]HistoryPair, 5000)
	for i := range pairs {
		pairs[i] = HistoryPair{Question: fmt.Sprint(i), Answer: "a"}
	}
	for _, tokens := range []int{1, 37, 16000} {
		got, err := historyWindow(pairs, tokens)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= len(pairs) {
			t.Fatalf("budget %d unexpectedly retained all tiny pairs", tokens)
		}
		if len(got) > 0 && EstimateTokens(mustEngineJSON(historyInput(got))) > tokens {
			t.Fatalf("serialized suffix exceeds budget %d", tokens)
		}
		start := len(pairs) - len(got)
		for i := range got {
			if got[i] != pairs[start+i] {
				t.Fatal("tiny pairs lost order or became incomplete")
			}
		}
		if start > 0 && EstimateTokens(mustEngineJSON(historyInput(pairs[start-1:]))) <= tokens {
			t.Fatalf("budget %d left room for another complete pair", tokens)
		}
	}
}
