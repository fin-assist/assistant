package fakellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/adapter/aistudio"
	"github.com/fin-assist/assistant/internal/generation"
)

type fakeKey struct{}

func (fakeKey) Key(context.Context) (string, error) { return "synthetic-key", nil }
func modelClient(t *testing.T, h *Handler) *aistudio.GenerationModel {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, e := aistudio.New(aistudio.Options{Environment: "dev", Endpoint: srv.URL + "/v1/responses", FolderID: "folder", Model: "gpt://folder/aliceai-llm", Key: fakeKey{}, Limits: aistudio.Limits{RequestBytes: 131072, ResponseBytes: 1048576, EventBytes: 65536, ArgumentBytes: 16384, OutputItems: 64}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(c.Close)
	model, e := aistudio.NewGenerationModel(c)
	if e != nil {
		t.Fatal(e)
	}
	return model
}
func modelRequest(q string) generation.ModelRequest {
	raw, _ := json.Marshal(map[string]any{"role": "user", "content": q})
	tools := []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}
	for _, n := range []string{"get_data_overview", "aggregate_operations", "present_result", "get_analytics", "off_topic"} {
		b, _ := json.Marshal(map[string]any{"type": "function", "name": n, "parameters": map[string]any{"type": "object"}})
		tools = append(tools, b)
	}
	return generation.ModelRequest{Input: []json.RawMessage{raw}, Tools: tools, MaxOutputTokens: 256, MaxToolCalls: 3}
}
func continueRequest(r *generation.ModelRequest, result generation.ModelResult) {
	for _, item := range result.Output {
		r.Input = append(r.Input, item.Raw)
		if item.Type == "function_call" {
			raw, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": item.CallID, "output": `{"amount":9007199254740993}`})
			r.Input = append(r.Input, raw)
		}
	}
}
func TestIntegrationScenarioThroughRealResponsesClient(t *testing.T) {
	c := modelClient(t, defaultHandler(t))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := modelRequest("rows")
	var text strings.Builder
	for step := 0; step < 3; step++ {
		result, e := c.Stream(ctx, r, func(d generation.ModelDelta) error { text.WriteString(d.Text); return nil }, func([]byte) error { return nil })
		if e != nil {
			t.Fatal(step, e)
		}
		if result.Usage.TotalTokens != 30 || (step == 0 && (len(result.Output) != 2 || result.Output[0].Name != "get_data_overview" || result.Output[1].Name != "aggregate_operations")) {
			t.Fatalf("step %d: %+v", step, result)
		}
		continueRequest(&r, result)
	}
	if !strings.Contains(text.String(), "9007199254740993") {
		t.Fatal("actual function result lost", text.String())
	}
}
func TestIntegrationScenarioSearchBudgetAndCitations(t *testing.T) {
	c := modelClient(t, defaultHandler(t))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r := modelRequest("search")
	for step, n := range []int{3, 1, 0} {
		r.MaxToolCalls = n
		r.Instructions = "Remaining built-in web searches for this answer: " + []string{"3.", "1.", "0."}[step]
		if n == 0 {
			r.Instructions += " Web search is no longer available; if data is insufficient, say exactly what is missing."
			r.Tools = r.Tools[1:]
		}
		result, e := c.Stream(ctx, r, func(generation.ModelDelta) error { return nil }, func([]byte) error { return nil })
		if e != nil {
			t.Fatal(step, e)
		}
		searches := 0
		for _, item := range result.Output {
			if item.Type == "web_search_call" {
				searches++
			}
		}
		if searches != []int{2, 1, 0}[step] {
			t.Fatal("incorrect search count", searches)
		}
		if step < 2 && result.Usage.ToolTokens != 7 {
			t.Fatal("tool usage missing")
		}
		if step == 2 && !strings.Contains(string(result.Output[0].Raw), "url_citation") {
			t.Fatal("citation lost")
		}
		continueRequest(&r, result)
	}
}
func TestIntegrationScenarioFailuresHaveNoAutomaticRetry(t *testing.T) {
	c := modelClient(t, defaultHandler(t))
	for _, q := range []string{"model-error", "refusal", "disconnect", "timeout"} {
		t.Run(q, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			result, e := c.Stream(ctx, modelRequest(q), func(generation.ModelDelta) error { return nil }, func([]byte) error { return nil })
			if e == nil || len(result.Output) != 0 {
				t.Fatal("failure accepted", result, e)
			}
			if q == "refusal" && !errors.Is(e, generation.ErrRefused) {
				t.Fatal(e)
			}
			if q == "timeout" && !errors.Is(e, context.DeadlineExceeded) {
				t.Fatal(e)
			}
		})
	}
}
func TestIntegrationScenarioRequestsAreIndependent(t *testing.T) {
	c := modelClient(t, defaultHandler(t))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			r := modelRequest("rows")
			for range 3 {
				out, e := c.Stream(ctx, r, func(generation.ModelDelta) error { return nil }, func([]byte) error { return nil })
				if e != nil {
					t.Error(e)
					return
				}
				continueRequest(&r, out)
			}
		})
	}
	wg.Wait()
}

func TestIntegrationFakeRejectsUnsafeWireRequests(t *testing.T) {
	server := httptest.NewServer(defaultHandler(t))
	defer server.Close()
	for _, tc := range []string{"missing privacy header", "logging enabled", "structured output", "stored response", "missing tool result", "completed tool results"} {
		t.Run(tc, func(t *testing.T) {
			body := requestBody("unknown")
			body["tools"] = modelRequest("rows").Tools
			header := "false"
			switch tc {
			case "missing privacy header":
				header = ""
			case "logging enabled":
				header = "true"
			case "structured output":
				body["tools"] = []any{map[string]any{"type": "web_search"}}
				body["text"] = map[string]any{"format": map[string]any{"type": "json_schema"}}
			case "stored response":
				body["store"] = true
			case "missing tool result", "completed tool results":
				body["input"] = []any{map[string]any{"role": "user", "content": "rows"}, map[string]any{"id": "fake_rows_0_0", "type": "function_call"}}
				if tc == "completed tool results" {
					body["input"] = append(body["input"].([]any), map[string]any{"type": "function_call_output", "call_id": "fake_rows_0_overview", "output": "{}"}, map[string]any{"type": "function_call_output", "call_id": "fake_rows_0_totals", "output": "{}"})
				}
			}
			raw, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			request, e := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/responses", bytes.NewReader(raw))
			if e != nil {
				t.Fatal(e)
			}
			request.Header.Set("x-data-logging-enabled", header)
			response, e := http.DefaultClient.Do(request)
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = response.Body.Close() }()
			want := http.StatusBadRequest
			if tc == "completed tool results" {
				want = http.StatusOK
			}
			if response.StatusCode != want {
				t.Fatal("unsafe request accepted", response.StatusCode)
			}
		})
	}
}
