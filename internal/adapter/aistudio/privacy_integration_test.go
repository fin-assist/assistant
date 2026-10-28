package aistudio

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/generation"
	"github.com/fin-assist/assistant/internal/tooldata"
)

type privacyData struct {
	tooldata.Data
	query string
}

func (d *privacyData) Find(_ context.Context, _ tooldata.Subject, args tooldata.FindArgs) (tooldata.Result, error) {
	d.query = args.Filter.Query
	return tooldata.Result{JSON: json.RawMessage(`{"description":"Т-Банк аптека +7 (999) 123-45-67 card: 4111111111111111 account: 40817810099910004312","amount_minor":"9007199254740993","date":"2026-10-28"}`)}, nil
}
func TestIntegrationPrivacyWholeEngineContextAndEchoedSearch(t *testing.T) {
	for _, mode := range []string{"visible", "opaque-only", "opaque-with-summary"} {
		t.Run(mode, func(t *testing.T) { privacyWholeEngine(t, mode) })
	}
}
func privacyWholeEngine(t *testing.T, mode string) {
	store := &sizeEngineStore{history: []generation.HistoryPair{{Question: "Старая почта old@example.test", Answer: "Старая карта 4111 1111 1111 1111"}}}
	data := &privacyData{}
	var requests [][]byte
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		requests = append(requests, raw)
		for _, secret := range []string{"old@example.test", "4111111111111111", "4111 1111", "40817810099910004312", "123-45-67", "123-45-68"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("original requisite reached HTTP: %s", secret)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			tokens := regexp.MustCompile(`\[phone_[0-9a-f]+\]`).FindAllString(string(raw), -1)
			if len(tokens) != 2 || tokens[0] == tokens[1] {
				t.Error("different phone numbers merged")
				return
			}
			token = tokens[0]
			args, _ := json.Marshal(map[string]any{"filter": map[string]any{"q": token}})
			events := httpProtocolFunctionEvents()
			for _, event := range events {
				if delta, ok := event["delta"]; ok && delta != "" {
					event["delta"] = string(args)
				}
				if _, ok := event["arguments"]; ok {
					event["arguments"] = string(args)
				}
				if item, ok := event["item"].(map[string]any); ok {
					item["name"] = "find_operations"
					if item["arguments"] != "" {
						item["arguments"] = string(args)
					}
				}
				if _, ok := event["name"]; ok {
					event["name"] = "find_operations"
				}
				if response, ok := event["response"].(map[string]any); ok {
					if output, ok := response["output"].([]any); ok && len(output) > 0 {
						item := output[0].(map[string]any)
						item["name"] = "find_operations"
						item["arguments"] = string(args)
					}
				}
			}
			reason := map[string]any{"id": "reasoning-item", "type": "reasoning", "status": "completed"}
			if mode != "opaque-only" {
				reason["summary"] = []any{map[string]any{"type": "summary_text", "text": "thought@example.test"}}
			}
			if mode != "visible" {
				reason["encrypted_content"] = "opaque-provider-payload"
			}
			for _, event := range events {
				if _, ok := event["output_index"]; ok {
					event["output_index"] = 1
				}
				if response, ok := event["response"].(map[string]any); ok {
					if output, ok := response["output"].([]any); ok && len(output) > 0 {
						response["output"] = append([]any{reason}, output...)
					}
				}
			}
			events = append([]map[string]any{events[0],
				parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "reasoning-item", "type": "reasoning"}}),
				parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": reason})}, events[1:]...)
			_, _ = io.WriteString(w, parserBody(events))
			return
		}
		if strings.Contains(string(raw), "thought@example.test") || !strings.Contains(string(raw), "reasoning-item") {
			t.Error("visible reasoning was lost or not filtered")
		}
		if !strings.Contains(string(raw), token) || !strings.Contains(string(raw), "9007199254740993") || !strings.Contains(string(raw), "2026-10-28") || !strings.Contains(string(raw), "аптека") {
			t.Error("financial values, link or allowed text lost")
		}
		_, _ = io.WriteString(w, httpTestSSE("Финансовый ответ"))
	}))
	defer srv.Close()
	client := httpTestClient(t, srv, nil)
	model, _ := NewGenerationModel(client)
	engine, err := generation.New(store, store, store, model, data, sizeLimits(), func() time.Time { return time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	task := sizeTask()
	task.Text = "Расходы по +7 (999) 123-45-67 и +7 (999) 123-45-68"
	if err = engine.Run(httpTestContext(t), task); err != nil {
		t.Fatal(err)
	}
	if mode != "visible" {
		if len(requests) != 1 || store.outcome.ErrorCode != "MODEL_ERROR" || store.outcome.Charged {
			t.Fatalf("opaque context bypass: posts=%d outcome=%+v", len(requests), store.outcome)
		}
		return
	}
	if len(requests) != 2 || data.query != "+7 (999) 123-45-67" || store.outcome.ErrorCode != "" || !store.outcome.Charged {
		t.Fatalf("cycle failed: posts=%d query=%q outcome=%+v", len(requests), data.query, store.outcome)
	}
	if store.history[0].Question != "Старая почта old@example.test" || task.Text != "Расходы по +7 (999) 123-45-67 и +7 (999) 123-45-68" {
		t.Fatal("original history/question changed")
	}
}
