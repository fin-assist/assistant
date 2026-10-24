package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"

	"github.com/fin-assist/assistant/internal/config"
)

// A provider protocol failure must refund the accepted request and erase even
// text that was streamed before the failure. These cases exercise the HTTP
// provider boundary through AskQuestion, WatchAnswer, history and durable state.
func TestAPIProviderProtocolFailuresRefundAndErasePartialAnswers(t *testing.T) {
	for name, change := range map[string]func([]map[string]any) []map[string]any{
		"missing-created":      func(e []map[string]any) []map[string]any { return e[1:] },
		"duplicate-created":    func(e []map[string]any) []map[string]any { return append([]map[string]any{e[0]}, e...) },
		"missing-completion":   func(e []map[string]any) []map[string]any { return e[:len(e)-1] },
		"duplicate-completion": func(e []map[string]any) []map[string]any { return append(e, e[len(e)-1]) },
		"foreign-item":         func(e []map[string]any) []map[string]any { e[3]["item_id"] = "foreign"; return e },
		"content-gap":          func(e []map[string]any) []map[string]any { e[2]["content_index"] = 1; return e },
		"delta-before-content": func(e []map[string]any) []map[string]any { return append(e[:2], e[3:]...) },
		"delta-after-done":     func(e []map[string]any) []map[string]any { e[3], e[4] = e[4], e[3]; return e },
		"text-mismatch":        func(e []map[string]any) []map[string]any { e[4]["text"] = "different"; return e },
		"unfinished-part":      func(e []map[string]any) []map[string]any { return append(e[:5], e[6:]...) },
		"foreign-response": func(e []map[string]any) []map[string]any {
			e[7]["response"].(map[string]any)["id"] = "foreign"
			return e
		},
		"missing-usage": func(e []map[string]any) []map[string]any {
			delete(e[7]["response"].(map[string]any), "usage")
			return e
		},
		"fractional-usage": func(e []map[string]any) []map[string]any {
			e[7]["response"].(map[string]any)["usage"].(map[string]any)["total_tokens"] = 1.5
			return e
		},
		"sequence-regression": func(e []map[string]any) []map[string]any {
			e[0]["sequence_number"] = 3
			e[1]["sequence_number"] = 2
			return e
		},
		"final-output-changed": func(e []map[string]any) []map[string]any {
			e[7]["response"].(map[string]any)["output"] = []any{}
			return e
		},
		"provider-failure-after-text": func(e []map[string]any) []map[string]any {
			return append(e[:4], map[string]any{"type": "response.failed", "error": map[string]any{"message": "PRIVATE provider details"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var events []map[string]any
			for _, frame := range strings.Split(strings.TrimSpace(generationAPIText("PRIVATE partial text")), "\n\n") {
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				events = append(events, event)
			}
			body := generationAPIFrames(change(events)...)
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Объясни мои расходы", commonv1.TransferMode_TRANSFER_MODE_WITH)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			if strings.Contains(presenterHistory(t, env, generationAPIUser).String(), "PRIVATE") {
				t.Fatal("private partial answer reached durable history")
			}
			if model.count.Load() != 1 {
				t.Fatal("protocol failure retried the provider without a new public request")
			}
		})
	}
}

func TestAPIProviderRefusalsAreFinalAndDoNotExposePrivateText(t *testing.T) {
	for _, kind := range []string{"response.refusal.delta", "response.refusal.done"} {
		t.Run(kind, func(t *testing.T) {
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, generationAPIFrames(generationAPICreated(), map[string]any{"type": kind, "delta": "PRIVATE moderation detail", "refusal": "PRIVATE moderation detail"}))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Вопрос о финансах", commonv1.TransferMode_TRANSFER_MODE_WITH)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done != nil || failed.GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER || failed.GetRetryable() || failed.GetRemainingLimit() != 5 {
				t.Fatalf("refusal: done=%v failed=%v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if saved.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || len(saved.Blocks) != 0 || saved.Source != nil || len(saved.Chips) != 0 || strings.Contains(saved.String(), "PRIVATE") {
				t.Fatalf("refusal history: %v", saved)
			}
		})
	}
}

// Runtime limits are valid deployment policy; the API must fail closed rather
// than silently turn a bounded request into unbounded provider work.
func TestAPIProviderRuntimeBudgetsBoundWorkAndRefund(t *testing.T) {
	for _, name := range []string{"steps", "functions", "request-bytes", "input-tokens", "response-bytes", "event-bytes", "argument-bytes"} {
		t.Run(name, func(t *testing.T) {
			model := generationAPIModel(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, generationAPIFunctionID(fmt.Sprintf("call-%d", n), "list_periods", `{"period":"month"}`))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model, func(c *config.Config) {
				switch name {
				case "steps":
					c.Generation.MaxSteps = 1
				case "functions":
					c.Generation.MaxFunctions = 1
					c.Generation.ParallelFunctions = 1
				case "request-bytes":
					c.Generation.RequestBytesPerStep = 256
					c.Generation.RequestBytesTotal = 256
				case "input-tokens":
					c.Generation.InputTokensPerStep = 32
					c.Generation.InputTokensTotal = 32
					c.Generation.HistoryTokens = 32
				case "response-bytes":
					c.Generation.ModelResponseBytes = 128
					c.Generation.ModelEventBytes = 128
					c.Generation.ModelArgumentBytes = 128
				case "event-bytes":
					c.Generation.ModelEventBytes = 64
				case "argument-bytes":
					c.Generation.ModelArgumentBytes = 4
				}
			})
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Какие периоды доступны?", commonv1.TransferMode_TRANSFER_MODE_WITH)
			presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
			want := int32(1)
			switch name {
			case "steps":
				want = 1
			case "functions":
				want = 2
			case "request-bytes", "input-tokens":
				want = 0
			}
			if got := model.count.Load(); got != want {
				t.Fatalf("provider requests=%d want=%d", got, want)
			}
		})
	}
}
