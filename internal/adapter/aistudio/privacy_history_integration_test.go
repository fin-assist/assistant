package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/generation"
)

func TestUnitPrivacyAmbiguousPreflightNeverReservesOrReadsCredentials(t *testing.T) {
	client, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	effects := 0
	client.options.Key = httpTestKey(func(context.Context) (string, error) { effects++; return "key", nil })
	client.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) { effects++; return nil, errors.New("unexpected HTTP") })
	request := httpTestRequest()
	request.Instructions = "Карта 4111 1111 1111 1111 - 1000 ₽"
	model, _ := NewGenerationModel(client)
	_, err = model.NewSession().Stream(httpTestContext(t), generation.ModelRequest{Instructions: request.Instructions, Input: request.Input, MaxOutputTokens: 64}, func(generation.ModelDelta) error { effects++; return nil }, func([]byte) error { effects++; return nil })
	if !errors.Is(err, generation.ErrInputAmbiguous) || effects != 0 {
		t.Fatal("ambiguous preflight had effects or lost typed signal", effects, err)
	}
}

func TestIntegrationPrivacyAmbiguousHistoryTrimsThroughSameSessionWithoutBypass(t *testing.T) {
	const bad = "Карта 4111 1111 1111 1111 - 1000 ₽"
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "current-with-good-history"}[current], func(t *testing.T) {
			var keys, posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), "4111 1111") || strings.Contains(string(raw), "old-marker") {
					t.Error("unsafe history/input reached HTTP")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, httpTestSSE("done"))
			}))
			defer srv.Close()
			client := httpTestClient(t, srv, func(options *Options) {
				options.Key = httpTestKey(func(context.Context) (string, error) { keys.Add(1); return "key", nil })
			})
			adapter, _ := NewGenerationModel(client)
			factory := &privacyObservedFactory{base: adapter}
			history, question := bad+" old-marker", "Новый финансовый вопрос"
			if current {
				history, question = "good old-marker", bad
			}
			store := &sizeEngineStore{history: []generation.HistoryPair{{Question: "old", Answer: history}}}
			engine, err := generation.New(store, store, store, factory, &privacyData{}, sizeLimits(), time.Now)
			if err != nil {
				t.Fatal(err)
			}
			task := sizeTask()
			task.Text = question
			if err = engine.Run(httpTestContext(t), task); err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if current {
				want = 0
			}
			if factory.sessions != 1 || factory.attempts != 2 || posts.Load() != want || keys.Load() != want || store.history[0].Answer != history {
				t.Fatal("unsafe ambiguity retry", factory.sessions, factory.attempts, posts.Load(), keys.Load())
			}
			if current {
				if store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged {
					t.Fatal(store.outcome)
				}
			} else if store.outcome.ErrorCode != "" || !store.outcome.Charged {
				t.Fatal(store.outcome)
			}
		})
	}
}
