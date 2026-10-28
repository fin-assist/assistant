package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

func TestUnitPrivacySessionKeepsKnowledgeAcrossUnsentFailuresAndIsolatesOtherSessions(t *testing.T) {
	const value = "4111111111111111"
	client, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	keys, posts := 0, 0
	client.options.Key = httpTestKey(func(context.Context) (string, error) { keys++; return "key", nil })
	var queries []string
	client.http.Transport = httpTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		posts++
		var body Request
		_ = json.NewDecoder(request.Body).Decode(&body)
		var item map[string]any
		_ = json.Unmarshal(body.Input[0], &item)
		queries = append(queries, item["q"].(string))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(httpTestSSE("done")))}, nil
	})
	session := client.newSession()
	source := httpTestRequest()
	source.Input = []json.RawMessage{json.RawMessage(`{"content":"карта ` + value + `"}`)}
	denied := errors.New("synthetic budget refusal")
	if _, err = session.streamGuarded(httpTestContext(t), source, func(Delta) error { return nil }, func([]byte) error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	oversized := httpTestRequest()
	oversized.Instructions = strings.Repeat("x", int(client.options.Limits.RequestBytes)+1)
	if _, err = session.streamGuarded(httpTestContext(t), oversized, func(Delta) error { return nil }, func([]byte) error { return nil }); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatal(err)
	}
	echo := httpTestRequest()
	echo.Input = []json.RawMessage{json.RawMessage(`{"q":"` + value + `"}`)}
	if _, err = session.streamGuarded(httpTestContext(t), echo, func(Delta) error { return nil }, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if keys != 1 || posts != 1 || queries[0] == value {
		t.Fatal("unsent failure erased private knowledge or caused credentials/HTTP", keys, posts, queries)
	}
	if _, err = client.StreamGuarded(httpTestContext(t), echo, func(Delta) error { return nil }, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if queries[1] != value {
		t.Fatal("one-shot call inherited another session dictionary")
	}
}

func TestUnitPrivacyConcurrentModelSessionsOwnTheirDictionaries(t *testing.T) {
	client, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var mu sync.Mutex
	tokens := map[string]string{}
	client.http.Transport = httpTestRoundTrip(func(request *http.Request) (*http.Response, error) {
		var body Request
		_ = json.NewDecoder(request.Body).Decode(&body)
		var item map[string]any
		_ = json.Unmarshal(body.Input[0], &item)
		text, _ := item["content"].(string)
		if query, ok := item["q"].(string); ok {
			text = query
		}
		matches := aliasPattern.FindAllString(text, -1)
		mu.Lock()
		defer mu.Unlock()
		if len(matches) != 1 {
			t.Error("session lost its requisite")
		} else if prior, ok := tokens[body.Instructions]; ok && prior != matches[0] {
			t.Error("alias changed inside session")
		} else {
			tokens[body.Instructions] = matches[0]
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(httpTestSSE("done")))}, nil
	})
	factory, _ := NewGenerationModel(client)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func() {
			session := factory.NewSession()
			<-start
			for _, input := range []string{`{"content":"карта 4111111111111111"}`, `{"q":"4111111111111111"}`} {
				_, err := session.Stream(httpTestContext(t), generation.ModelRequest{Instructions: name, Input: []json.RawMessage{json.RawMessage(input)}, MaxOutputTokens: 64}, func(generation.ModelDelta) error { return nil }, func([]byte) error { return nil })
				if err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if tokens["one"] == "" || tokens["one"] == tokens["two"] {
		t.Fatal("concurrent generations shared aliases")
	}
}

type privacyTrimData struct {
	tooldata.Data
	value, query string
}

func (d *privacyTrimData) Aggregate(_ context.Context, _ tooldata.Subject, args tooldata.AggregateArgs) (tooldata.Result, error) {
	d.query = args.Filter.Query
	raw, _ := json.Marshal(map[string]any{"groups": []any{map[string]any{"filters": map[string]any{"q": d.value}, "amount_minor": d.value, "title": strings.Repeat("x", 20000)}}, "total_amount_minor": d.value})
	return tooldata.Result{JSON: raw}, nil
}

type privacyObservedFactory struct {
	base                        generation.Model
	sessions, attempts, denials int
}

func (m *privacyObservedFactory) NewSession() generation.ModelSession {
	m.sessions++
	session := m.base.NewSession()
	return generation.ModelSessionFunc(func(ctx context.Context, request generation.ModelRequest, emit func(generation.ModelDelta) error, reserve func([]byte) error) (generation.ModelResult, error) {
		m.attempts++
		return session.Stream(ctx, request, emit, func(raw []byte) error {
			err := reserve(raw)
			if errors.Is(err, generation.ErrBudgetExceeded) {
				m.denials++
			}
			return err
		})
	})
}
func TestIntegrationPrivacyHistoryTrimKeepsKnowledgeForSuccessfulSecondPOST(t *testing.T) {
	for _, tc := range []struct{ label, value string }{{"карта ", "4111111111111111"}, {"телефон ", "79991234567"}, {"счёт ", "40817810099910004312"}} {
		for _, mode := range []string{"budget", "transport"} {
			t.Run(tc.label+mode, func(t *testing.T) {
				history := tc.label + tc.value + " history-only-marker " + strings.Repeat("h", 20000)
				store := &sizeEngineStore{history: []generation.HistoryPair{{Question: "old question", Answer: history}}}
				data := &privacyTrimData{value: tc.value}
				var keys, posts atomic.Int32
				var token string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					var body Request
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					raw, _ := json.Marshal(body.Input)
					step := posts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if step == 1 {
						aliases := aliasPattern.FindAllString(string(raw), -1)
						if len(aliases) != 1 {
							t.Error("history source not masked")
							return
						}
						token = aliases[0]
						args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token}})
						events := httpProtocolFunctionEvents()
						for _, event := range events {
							if _, ok := event["delta"]; ok {
								event["delta"] = string(args)
							}
							if _, ok := event["arguments"]; ok {
								event["arguments"] = string(args)
							}
							if _, ok := event["name"]; ok {
								event["name"] = "aggregate_operations"
							}
							if item, ok := event["item"].(map[string]any); ok {
								item["name"] = "aggregate_operations"
								if item["arguments"] != "" {
									item["arguments"] = string(args)
								}
							}
							if response, ok := event["response"].(map[string]any); ok {
								if output, ok := response["output"].([]any); ok && len(output) > 0 {
									item := output[0].(map[string]any)
									item["name"] = "aggregate_operations"
									item["arguments"] = string(args)
								}
							}
						}
						_, _ = io.WriteString(w, parserBody(events))
						return
					}
					if strings.Contains(string(raw), "history-only-marker") {
						t.Error("old source pair was not trimmed")
					}
					var items []map[string]any
					_ = json.Unmarshal(raw, &items)
					for _, item := range items {
						if item["type"] == "function_call" {
							var args map[string]any
							_ = json.Unmarshal([]byte(item["arguments"].(string)), &args)
							if args["filter"].(map[string]any)["q"] != token {
								t.Error("bare q leaked after trim")
							}
						}
						if item["type"] == "function_call_output" {
							var result map[string]any
							_ = json.Unmarshal([]byte(item["output"].(string)), &result)
							group := result["groups"].([]any)[0].(map[string]any)
							if group["filters"].(map[string]any)["q"] != token || group["amount_minor"] != tc.value || result["total_amount_minor"] != tc.value {
								t.Error("result q leaked or quoted money changed after trim")
							}
						}
					}
					_, _ = io.WriteString(w, httpTestSSE("done"))
				}))
				defer server.Close()
				client := httpTestClient(t, server, func(options *Options) {
					options.Limits.RequestBytes = 65536
					if mode == "transport" {
						options.Limits.RequestBytes = 40000
					}
					options.Key = httpTestKey(func(context.Context) (string, error) { keys.Add(1); return "key", nil })
				})
				adapter, _ := NewGenerationModel(client)
				factory := &privacyObservedFactory{base: adapter}
				limits := sizeLimits()
				expectedDenials := 0
				if mode == "budget" {
					limits.Budget.InputTokensPerStep = 9000
					limits.Budget.HistoryTokens = 9000
					expectedDenials = 1
				} else {
					limits.Budget.RequestBytesPerStep = 40000
				}
				engine, err := generation.New(store, store, store, factory, data, limits, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				task := sizeTask()
				task.Text = "Сколько всего?"
				if err = engine.Run(httpTestContext(t), task); err != nil {
					t.Fatal(err)
				}
				if factory.sessions != 1 || factory.denials != expectedDenials || factory.attempts != 3 || posts.Load() != 2 || keys.Load() != 2 || data.query != tc.value || store.outcome.ErrorCode != "" || !store.outcome.Charged {
					t.Fatalf("trim cycle: session=%d attempts=%d denied=%d posts=%d keys=%d q=%q outcome=%+v", factory.sessions, factory.attempts, factory.denials, posts.Load(), keys.Load(), data.query, store.outcome)
				}
				if store.history[0].Answer != history {
					t.Fatal("original history mutated")
				}
			})
		}
	}
}
