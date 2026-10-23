package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

type sizeEngineStore struct {
	history  []generation.HistoryPair
	outcome  generation.Outcome
	appended string
}

func (s *sizeEngineStore) History(context.Context, domain.GenerationTask, int) ([]generation.HistoryPair, error) {
	return s.history, nil
}
func (s *sizeEngineStore) Append(_ context.Context, _ domain.GenerationTask, _ int, text string) error {
	s.appended += text
	return nil
}
func (*sizeEngineStore) AppendBlock(context.Context, domain.GenerationTask, int, dialogue.Block) error {
	return errors.New("unexpected structured block in request-size test")
}
func (s *sizeEngineStore) Finish(_ context.Context, _ domain.GenerationTask, outcome generation.Outcome, _ time.Time) (generation.FinishResult, error) {
	s.outcome = outcome
	return generation.FinishResult{Applied: true}, nil
}
func (s *sizeEngineStore) Expired(context.Context, time.Time, int) ([]domain.GenerationTask, error) {
	return nil, nil
}
func (s *sizeEngineStore) Run(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (s *sizeEngineStore) Guard(context.Context, string) (bool, error) { return true, nil }

type sizeModel func(context.Context, generation.ModelRequest, func(generation.ModelDelta) error, func([]byte) error) (generation.ModelResult, error)

func (f sizeModel) Stream(ctx context.Context, r generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
	return f(ctx, r, emit, reserve)
}

type sizeData struct {
	tooldata.Data
	calls atomic.Int32
}

func (d *sizeData) Overview(context.Context, tooldata.Subject) (tooldata.Result, error) {
	d.calls.Add(1)
	return tooldata.Result{JSON: json.RawMessage(`{}`)}, nil
}

func sizeLimits() generation.EngineLimits {
	return generation.EngineLimits{Budget: generation.BudgetLimits{InputTokensPerStep: 24000, InputTokensTotal: 48000, HistoryTokens: 16000, RequestBytesPerStep: 65536, RequestBytesTotal: 131072, MaxSteps: 8, MaxFunctions: 16}, MaxOutputTokens: 4096, ParallelFunctions: 4, MaxSearchCalls: 3}
}
func sizeTask() domain.GenerationTask {
	return domain.GenerationTask{UserID: "user", QuestionID: "q", AnswerID: "a", Text: "current question", TransferMode: "with", TimeZone: "Europe/Moscow"}
}
func sizeEngine(t *testing.T, store *sizeEngineStore, model generation.Model, data *sizeData, limits generation.EngineLimits) *generation.Engine {
	t.Helper()
	engine, err := generation.New(store, store, store, model, data, limits, func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestGenerationTransportSizeSentinelAndNoEffects(t *testing.T) {
	for _, kind := range []string{"instructions", "raw sum", "serialized overhead", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			options := httpTestOptions()
			options.Limits.RequestBytes = 1024
			keys, httpCalls, emits, guards := 0, 0, 0, 0
			options.Key = httpTestKey(func(context.Context) (string, error) { keys++; return "key", nil })
			client, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) { httpCalls++; return nil, errors.New("unexpected") })
			model, _ := NewGenerationModel(client)
			request := httpTestRequest()
			switch kind {
			case "instructions":
				request.Instructions = strings.Repeat("a", 1025)
			case "raw sum":
				request.Input = []json.RawMessage{json.RawMessage(`{"text":"` + strings.Repeat("a", 1024) + `"}`)}
			case "serialized overhead":
				request.Instructions = strings.Repeat("<&", 100)
			case "malformed":
				request.Input = []json.RawMessage{json.RawMessage(`{"bad":`)}
			}
			_, err = model.Stream(httpTestContext(t), generation.ModelRequest{Instructions: request.Instructions, Input: request.Input, MaxOutputTokens: request.MaxOutputTokens, MaxToolCalls: request.MaxToolCalls}, func(generation.ModelDelta) error { emits++; return nil }, func([]byte) error { guards++; return nil })
			if err == nil || errors.Is(err, generation.ErrInputTooLarge) != (kind != "malformed") {
				t.Fatalf("classification %s: %v", kind, err)
			}
			if keys != 0 || httpCalls != 0 || emits != 0 || guards != 0 {
				t.Fatalf("preflight effects keys=%d HTTP=%d emits=%d guards=%d", keys, httpCalls, emits, guards)
			}
		})
	}
}

func TestGenerationRealHTTPTrimsWholeHistoryBeforeSinglePOST(t *testing.T) {
	for _, kind := range []string{"raw sum", "serialized overhead"} {
		t.Run(kind, func(t *testing.T) {
			store := &sizeEngineStore{history: []generation.HistoryPair{{Question: "OLD-QUESTION", Answer: "OLD-ANSWER"}, {Question: "RECENT-QUESTION", Answer: "RECENT-ANSWER"}}}
			data := &sizeData{}
			task := sizeTask()
			limits := sizeLimits()
			// Measure the actual engine envelope, including prompt and tool schemas,
			// to exercise each transport boundary without copying engine internals.
			preflightSize := 0
			probe := sizeModel(func(_ context.Context, r generation.ModelRequest, _ func(generation.ModelDelta) error, _ func([]byte) error) (generation.ModelResult, error) {
				preflightSize = len(r.Instructions)
				for _, raw := range append(append([]json.RawMessage{}, r.Input...), r.Tools...) {
					preflightSize += len(raw)
				}
				return generation.ModelResult{}, generation.ErrRefused
			})
			if err := sizeEngine(t, store, probe, data, limits).Run(httpTestContext(t), task); err != nil {
				t.Fatal(err)
			}
			target := 65536
			if kind == "raw sum" {
				target++
			}
			store.history[0].Question += strings.Repeat("x", target-preflightSize)
			store.outcome = generation.Outcome{}
			var keys, posts, acceptedGuards, attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if len(raw) > 65536 || strings.Contains(string(raw), "OLD-QUESTION") || strings.Contains(string(raw), "OLD-ANSWER") || !strings.Contains(string(raw), "RECENT-QUESTION") || !strings.Contains(string(raw), "RECENT-ANSWER") {
					t.Errorf("wrong history trim or byte count %d", len(raw))
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, httpTestSSE("Готовый ответ"))
			}))
			defer server.Close()
			client := httpTestClient(t, server, func(options *Options) {
				options.Limits.RequestBytes = 65536
				options.Key = httpTestKey(func(context.Context) (string, error) {
					if acceptedGuards.Load() != 1 {
						t.Error("credential read before first accepted guard")
					}
					keys.Add(1)
					return "key", nil
				})
			})
			adapter, _ := NewGenerationModel(client)
			model := sizeModel(func(ctx context.Context, r generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
				attempts.Add(1)
				return adapter.Stream(ctx, r, emit, func(raw []byte) error {
					if err := reserve(raw); err != nil {
						return err
					}
					acceptedGuards.Add(1)
					return nil
				})
			})
			if err := sizeEngine(t, store, model, data, limits).Run(httpTestContext(t), task); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() != 2 || keys.Load() != 1 || posts.Load() != 1 || store.outcome.ErrorCode != "" || !store.outcome.Charged || store.appended != "Готовый ответ" {
				t.Fatalf("attempts=%d keys=%d posts=%d outcome=%+v", attempts.Load(), keys.Load(), posts.Load(), store.outcome)
			}
		})
	}
}

func TestGenerationRealHTTPDoesNotResetBudgetAfterPreflightTrim(t *testing.T) {
	store := &sizeEngineStore{history: []generation.HistoryPair{{Question: strings.Repeat("old", 12000), Answer: "old answer"}, {Question: "recent", Answer: "recent answer"}}}
	task := sizeTask()
	task.Text = strings.Repeat("current", 5000)
	limits := sizeLimits()
	limits.Budget.RequestBytesTotal = 65536
	data := &sizeData{}
	var keys, posts, guards atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"id": "function", "type": "function_call", "call_id": "call", "name": "get_data_overview", "arguments": "{}", "status": "completed"}
		_, _ = io.WriteString(w, httpTestFrames(
			map[string]any{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}},
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "function", "type": "function_call", "call_id": "call", "name": "get_data_overview", "arguments": "", "status": "in_progress"}},
			map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "function", "delta": "{}"}, map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "function", "name": "get_data_overview", "arguments": "{}"},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}, map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
		))
	}))
	defer server.Close()
	client := httpTestClient(t, server, func(o *Options) {
		o.Limits.RequestBytes = 65536
		o.Key = httpTestKey(func(context.Context) (string, error) { keys.Add(1); return "key", nil })
	})
	adapter, _ := NewGenerationModel(client)
	model := sizeModel(func(ctx context.Context, r generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
		return adapter.Stream(ctx, r, emit, func(raw []byte) error {
			err := reserve(raw)
			if err == nil {
				guards.Add(1)
			}
			return err
		})
	})
	if err := sizeEngine(t, store, model, data, limits).Run(httpTestContext(t), task); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || keys.Load() != 1 || guards.Load() != 1 || data.calls.Load() != 1 || store.outcome.ErrorCode != "MODEL_ERROR" {
		t.Fatalf("budget reset: posts=%d keys=%d guards=%d tools=%d outcome=%+v", posts.Load(), keys.Load(), guards.Load(), data.calls.Load(), store.outcome)
	}
}
