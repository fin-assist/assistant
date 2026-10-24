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
func TestIntegrationGuardedHTTPReservesRepeatedFullContext(t *testing.T) {
	for _, outcome := range []string{"completed", "provider rejected", "disconnect", "partial SSE"} {
		t.Run(outcome, func(t *testing.T) {
			allowed := 2
			if outcome == "disconnect" || outcome == "partial SSE" {
				allowed = 1
			}
			var requests, keys atomic.Int32
			bodies := make(chan []byte, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				select {
				case bodies <- body:
				default:
					t.Error("unexpected extra HTTP request")
				}
				switch outcome {
				case "provider rejected":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				case "partial SSE":
					w.Header().Set("Content-Type", "text/event-stream")
					// Deliver two text fragments, then sever the connection before
					// output_text.done or the terminal response can arrive.
					if _, err := io.WriteString(w, parserBody(parserTextEvents()[:6])); err != nil {
						t.Error(err)
					}
					w.(http.Flusher).Flush()
					fallthrough
				case "disconnect":
					// The full request body has been read, so whether the provider
					// performed paid work cannot be inferred from this failure.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
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
						InputTokensPerStep: len(raw), InputTokensTotal: allowed * len(raw), HistoryTokens: 1,
						RequestBytesPerStep: len(raw), RequestBytesTotal: allowed * len(raw), MaxSteps: 8, MaxFunctions: 16,
					})
					if err != nil {
						return err
					}
				}
				return budget.ReserveRequest(raw)
			}
			var deltas int
			for step := range allowed + 1 {
				_, err := client.StreamGuarded(httpTestContext(t), request, func(Delta) error { deltas++; return nil }, guard)
				if step == allowed {
					if !errors.Is(err, generation.ErrBudgetExceeded) {
						t.Fatalf("exhausted step error = %v", err)
					}
				} else if (err != nil) != (outcome != "completed") {
					t.Fatalf("step %d error = %v, outcome = %s", step, err, outcome)
				}
			}
			if outcome == "partial SSE" && deltas != 2 {
				t.Fatalf("received text fragments = %d, want 2 before stream interruption", deltas)
			}
			if requests.Load() != int32(allowed) || keys.Load() != int32(allowed) {
				t.Fatalf("requests = %d, key reads = %d; want %d each", requests.Load(), keys.Load(), allowed)
			}
			for range allowed {
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
