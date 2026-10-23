package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fin-assist/assistant/internal/generation"
)

// The same complete context is paid for again on every model step. Exercise
// the real HTTP boundary so a guard over just Request.Input cannot pass here.
func TestGuardedHTTPReservesRepeatedFullContext(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "completed"
		if rejected {
			name = "provider rejected"
		}
		t.Run(name, func(t *testing.T) {
			var requests, keys atomic.Int32
			bodies := make(chan []byte, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				bodies <- body
				if rejected {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, parserBody(parserTextEvents()))
			}))
			t.Cleanup(server.Close)
			options := httpTestOptions()
			options.Endpoint = server.URL + "/v1/responses"
			options.Key = httpTestKey(func(_ context.Context) (string, error) {
				keys.Add(1)
				return "test-secret", nil
			})
			client, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			request := httpTestRequest()
			request.Input = append(request.Input, json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"Сумма 9007199254740993, <test>"}`))
			var budget *generation.Budget
			var first []byte
			guard := func(raw []byte) error {
				if budget == nil {
					first = bytes.Clone(raw)
					budget, err = generation.NewBudget(generation.BudgetLimits{
						InputTokensPerStep: len(raw), InputTokensTotal: 2 * len(raw), HistoryTokens: 1,
						RequestBytesPerStep: len(raw), RequestBytesTotal: 2 * len(raw), MaxSteps: 8, MaxFunctions: 16,
					})
					if err != nil {
						return err
					}
				}
				return budget.ReserveRequest(raw)
			}
			for step := range 3 {
				_, err := client.StreamGuarded(httpTestContext(t), request, func(Delta) error { return nil }, guard)
				if step == 2 {
					if !errors.Is(err, generation.ErrBudgetExceeded) {
						t.Fatalf("third step error = %v", err)
					}
				} else if (err != nil) != rejected {
					t.Fatalf("step %d error = %v, rejected = %v", step, err, rejected)
				}
			}
			if requests.Load() != 2 || keys.Load() != 2 {
				t.Fatalf("requests = %d, key reads = %d; want 2 each", requests.Load(), keys.Load())
			}
			for range 2 {
				if body := <-bodies; !bytes.Equal(body, first) {
					t.Fatal("guarded bytes differ from the complete HTTP body")
				}
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(first, &wire); err != nil || wire["model"] == nil || wire["store"] == nil || wire["truncation"] == nil || len(wire["input"]) == 0 {
				t.Fatal("guard did not receive full wire fields and function results")
			}
		})
	}
}
