package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/assistant/internal/config"
)

func providerWireEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
func TestAPIProviderWireRejectsAmbiguousAndBrokenLifecycle(t *testing.T) {
	cases := []struct{ name, body string }{
		{"nested-duplicate-json", `data: {"type":"response.created","response":{"id":"r","id":"other","status":"in_progress","output":[]}}` + "\n\n"},
		{"array-duplicate-json", `data: {"type":"response.created","response":{"id":"r","status":"in_progress","output":[]},"extra":[{"key":1,"key":2}]}` + "\n\n"},
		{"multiple-json-values", "data: {} {}\n\n"},
		{"wrong-json-type", "data: []\n\n"},
		{"truncated-array-json", "data: {\"type\":\"response.created\",\"extra\":[1,]}\n\n"},
		{"truncated-object-json", "data: {\"type\":\"response.created\",\"extra\":{\"x\":}}\n\n"},
		{"invalid-utf8", "data: {\"type\":\"response.created\",\"private\":\"" + string([]byte{0xff}) + "\"}\n\n"},
		{"duplicate-event-name", "event: response.created\nevent: response.created\n" + generationAPIFrames(generationAPICreated())},
		{"event-name-disagrees", "event: response.completed\n" + generationAPIFrames(generationAPICreated())},
		{"premature-done", "data: [DONE]\n\n"},
		{"duplicate-done", generationAPIText("PRIVATE partial") + "data: [DONE]\n\ndata: [DONE]\n\n"},
	}
	for name, change := range map[string]func([]map[string]any){
		"invalid-created-status":  func(e []map[string]any) { e[0]["response"].(map[string]any)["status"] = "completed" },
		"unknown-provider-event":  func(e []map[string]any) { e[3]["type"] = "response.unrecognized_private_event" },
		"malformed-final-content": func(e []map[string]any) { e[6]["item"].(map[string]any)["content"] = []any{7} },
		"initial-message-role":    func(e []map[string]any) { e[1]["item"].(map[string]any)["role"] = "user" },
		"initial-message-content": func(e []map[string]any) {
			e[1]["item"].(map[string]any)["content"] = []any{map[string]any{"type": "output_text", "text": "PRIVATE preload"}}
		},
		"initial-part-text":            func(e []map[string]any) { e[2]["part"].(map[string]any)["text"] = "PRIVATE preload" },
		"unknown-content-type":         func(e []map[string]any) { e[2]["part"].(map[string]any)["type"] = "image" },
		"missing-delta":                func(e []map[string]any) { delete(e[3], "delta") },
		"negative-output-index":        func(e []map[string]any) { e[3]["output_index"] = -1 },
		"missing-final-text":           func(e []map[string]any) { delete(e[4], "text") },
		"nonmatching-final-part":       func(e []map[string]any) { e[5]["part"].(map[string]any)["text"] = "PRIVATE replacement" },
		"final-message-role":           func(e []map[string]any) { e[6]["item"].(map[string]any)["role"] = "user" },
		"final-message-status":         func(e []map[string]any) { e[6]["item"].(map[string]any)["status"] = "in_progress" },
		"missing-initial-output-index": func(e []map[string]any) { delete(e[1], "output_index") },
		"unsupported-output-type":      func(e []map[string]any) { e[1]["item"].(map[string]any)["type"] = "image" },
	} {
		events := providerWireEvents(t, generationAPIText("PRIVATE partial"))
		change(events)
		cases = append(cases, struct{ name, body string }{name, generationAPIFrames(events...)})
	}
	for name, change := range map[string]func([]map[string]any){
		"function-final-missing-item":        func(e []map[string]any) { e[4]["output_index"] = 7 },
		"function-initial-arguments":         func(e []map[string]any) { e[1]["item"].(map[string]any)["arguments"] = "PRIVATE preloaded" },
		"function-missing-call-id":           func(e []map[string]any) { e[1]["item"].(map[string]any)["call_id"] = "" },
		"function-missing-delta":             func(e []map[string]any) { delete(e[2], "delta") },
		"function-mismatched-done-arguments": func(e []map[string]any) { e[3]["arguments"] = `{"period":"year"}` },
		"function-mismatched-done-name":      func(e []map[string]any) { e[3]["name"] = "foreign" },
		"function-final-call-id":             func(e []map[string]any) { e[4]["item"].(map[string]any)["call_id"] = "other" },
		"function-final-name":                func(e []map[string]any) { e[4]["item"].(map[string]any)["name"] = "foreign" },
		"function-final-arguments":           func(e []map[string]any) { e[4]["item"].(map[string]any)["arguments"] = `{"period":"year"}` },
	} {
		events := providerWireEvents(t, generationAPIFunctionID("call", "list_periods", `{"period":"month"}`))
		change(events)
		cases = append(cases, struct{ name, body string }{name, generationAPIFrames(events...)})
	}

	for _, tc := range []struct{ name, tool, args string }{
		{"tool-array-arguments", "list_periods", `[]`},
		{"tool-empty-arguments", "list_periods", ``},
		{"tool-duplicate-members", "list_periods", `{"period":"month","period":"year"}`},
		{"tool-nested-duplicate", "aggregate_operations", `{"filter":{"q":"x","q":"other"}}`},
		{"tool-array-duplicate", "aggregate_operations", `{"group_by":[{"x":1,"x":2}]}`},
		{"tool-malformed-array", "aggregate_operations", `{"group_by":["category",]}`},
		{"tool-period-type", "list_periods", `{"period":7}`},
		{"tool-invalid-period", "list_periods", `{"period":"week"}`},
		{"tool-invalid-period-key", "get_analytics", `{"period":"quarter","date":"2026-Q0"}`},
		{"tool-range-and-all-time", "find_operations", `{"all_time":true,"filter":{"from":"2026-09-01T00:00:00Z"}}`},
		{"tool-zero-date", "find_operations", `{"filter":{"from":"0001-01-01T00:00:00Z"}}`},
		{"tool-reversed-range", "aggregate_operations", `{"filter":{"from":"2026-10-01T00:00:00Z","to":"2026-09-01T00:00:00Z"}}`},
		{"tool-negative-amount", "aggregate_operations", `{"filter":{"amount_from":-1}}`},
		{"tool-reversed-amounts", "find_operations", `{"filter":{"amount_from":20,"amount_to":10}}`},
	} {
		cases = append(cases, struct{ name, body string }{tc.name, generationAPIFunctionID("call", tc.tool, tc.args)})
	}

	for _, name := range []string{"duplicate-item-id", "duplicate-content-done", "duplicate-progress"} {
		events := providerWireEvents(t, generationAPIText("PRIVATE partial"))
		switch name {
		case "duplicate-item-id":
			second := map[string]any{"type": "response.output_item.added", "output_index": 1, "item": events[1]["item"]}
			events = append(append([]map[string]any{}, events[:2]...), append([]map[string]any{second}, events[2:]...)...)
		case "duplicate-content-done":
			events = append(append([]map[string]any{}, events[:6]...), append([]map[string]any{events[5]}, events[6:]...)...)
		case "duplicate-progress":
			progress := map[string]any{"type": "response.in_progress", "response": map[string]any{"id": "r", "status": "in_progress"}}
			events = append([]map[string]any{events[0], progress, progress}, events[1:]...)
		}
		cases = append(cases, struct{ name, body string }{name, generationAPIFrames(events...)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peers := &generationAPIPeers{grant: true}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) })
			env := generationAPIEnv(t, peers, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Объясни мои финансовые данные", commonv1.TransferMode_TRANSFER_MODE_WITH)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if strings.Contains(presenterHistory(t, env, generationAPIUser).String(), "PRIVATE") {
				t.Fatal("provider protocol leaked private partial data")
			}
			if model.count.Load() != 1 || peers.count("periods")+peers.count("aggregate")+peers.count("find")+peers.count("analytics") != 0 {
				t.Fatalf("invalid lifecycle caused paid retry/downstream read: model=%d periods=%d", model.count.Load(), peers.count("periods"))
			}
		})
	}
}
func TestAPIProviderNamedHeartbeatAndExactResponseBoundary(t *testing.T) {
	const answer = "Ответ по личным финансам"
	events := providerWireEvents(t, generationAPIText(answer))
	var named strings.Builder
	named.WriteString(": heartbeat\nretry: 250\nid: diagnostic-only\n\n")
	for _, event := range events {
		named.WriteString("event: " + event["type"].(string) + "\n")
		named.WriteString(generationAPIFrames(event))
	}
	named.WriteString("data: [DONE]\n\n")
	body := named.String()
	for _, overflow := range []bool{false, true} {
		name := "exact-cap"
		if overflow {
			name = "extra-after-valid-completion"
		}
		t.Run(name, func(t *testing.T) {
			wire := body
			if overflow {
				wire += ": hidden trailing bytes\n\n"
			}
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, wire) })
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model, func(c *config.Config) {
				c.Generation.ModelResponseBytes = len(body)
				c.Generation.ModelEventBytes = len(body)
				c.Generation.ModelArgumentBytes = len(body)
			})
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Расскажи о личных финансах", commonv1.TransferMode_TRANSFER_MODE_WITH)
			if overflow {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
				t.Fatalf("valid named/DONE exact-boundary response=%v %v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != answer {
				t.Fatalf("valid response changed: %v", saved)
			}
		})
	}
}

func TestAPIProviderReasoningAndProgressNeverBecomePublicAnswer(t *testing.T) {
	const answer = "Видимый финансовый ответ"
	events := providerWireEvents(t, generationAPIText(answer))
	reasoning := map[string]any{"id": "reason", "type": "reasoning", "status": "completed", "encrypted_content": "PRIVATE provider reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "PRIVATE hidden summary"}}}
	frames := []map[string]any{events[0], {"type": "response.in_progress", "response": map[string]any{"id": "r", "status": "in_progress"}}, {"type": "ping"}, {"type": "response.reasoning_summary_text.delta", "delta": "PRIVATE hidden delta"}, {"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "reason", "type": "reasoning", "status": "in_progress"}}, {"type": "response.output_item.done", "output_index": 0, "item": reasoning}}
	for _, event := range events[1:] {
		if _, ok := event["output_index"]; ok {
			event["output_index"] = 1
		}
		frames = append(frames, event)
	}
	response := events[len(events)-1]["response"].(map[string]any)
	response["output"] = append([]any{reasoning}, response["output"].([]any)...)
	body := generationAPIFrames(frames...)
	model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
	accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Мои финансовые данные", commonv1.TransferMode_TRANSFER_MODE_WITH)
	done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
	if done == nil || failed != nil || !done.Charged || done.RemainingLimit != 4 {
		t.Fatalf("reasoning/progress response=%v %v", done, failed)
	}
	saved := presenterHistory(t, env, generationAPIUser)
	if len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != answer || strings.Contains(saved.String(), "PRIVATE") {
		t.Fatalf("private provider reasoning reached history: %v", saved)
	}
}

func TestAPIProviderNestedRefusalsErasePartialAndForbidPaidRetry(t *testing.T) {
	for _, where := range []string{"added", "done"} {
		t.Run(where, func(t *testing.T) {
			events := providerWireEvents(t, generationAPIText("PRIVATE partial text"))
			index := 2
			if where == "done" {
				index = 5
			}
			events[index]["part"] = map[string]any{"type": "refusal", "refusal": "PRIVATE moderation reason"}
			body := generationAPIFrames(events...)
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Вопрос о финансах", commonv1.TransferMode_TRANSFER_MODE_WITH)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done != nil || failed.GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER || failed.GetRetryable() || failed.GetRemainingLimit() != 5 {
				t.Fatalf("nestedrefusal=%v %v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if len(saved.Blocks) != 0 || saved.Source != nil || len(saved.Chips) != 0 || strings.Contains(saved.String(), "PRIVATE") {
				t.Fatalf("refusal retained private partial answer: %v", saved)
			}
			if model.count.Load() != 1 {
				t.Fatal("provider refusal was automatically retried")
			}
		})
	}
}
