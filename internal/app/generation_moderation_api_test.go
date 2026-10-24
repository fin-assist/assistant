package app_test

import (
	"io"
	"net/http"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
)

func TestAPIGenerationAPIModerationIsDistinctFromTokenExhaustion(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		code      assistantv1.AnswerErrorCode
		retryable bool
	}{
		{"content_filter", assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_CANNOT_ANSWER, false},
		{"max_output_tokens", assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_MODEL_ERROR, true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			model := generationAPIModel(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, generationAPIFrames(generationAPICreated(), map[string]any{
					"type": "response.incomplete", "response": map[string]any{"id": "r", "status": "incomplete", "incomplete_details": map[string]any{"reason": tc.reason}, "output": []any{}},
				}))
			})
			env := generationAPIEnv(t, &generationAPIPeers{grant: true}, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Вопрос о финансах", commonv1.TransferMode_TRANSFER_MODE_WITH)
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			if done != nil || failed == nil || failed.GetCode() != tc.code || failed.GetRetryable() != tc.retryable || failed.GetRemainingLimit() != 5 {
				t.Fatalf("done=%v failed=%v", done, failed)
			}
			page, err := assistantv1.NewAssistantServiceClient(env.Conn).ListMessages(generationAPIContext(t, generationAPIUser, ""), &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.GetMessages()) != 2 || len(page.Messages[1].Blocks) != 0 || page.Messages[1].Source != nil {
				t.Fatalf("failed history=%v err=%v", page, err)
			}
		})
	}
}
