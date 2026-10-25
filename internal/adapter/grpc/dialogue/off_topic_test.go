package dialogue

import (
	"testing"
	"time"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"

	"github.com/fin-assist/go-grpc/reqctx"

	domain "github.com/fin-assist/assistant/internal/dialogue"
)

func TestUnitConfiguredOffTopicTextIsSharedByHistoryAndWatch(t *testing.T) {
	const configured = "Помощник отвечает на финансовые вопросы. Попробуйте другую формулировку."
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy", false: "explicit"}[legacy], func(t *testing.T) {
			answer := domain.Answer{ID: user, CreatedAt: now, Status: "failed", ErrorCode: "OFF_TOPIC", TimeZone: "Europe/Moscow"}
			if !legacy {
				answer.Retryable = new(bool)
			}
			r := &reads{page: domain.Page{Messages: []domain.Message{{ID: user, CreatedAt: now, Answer: &answer}}, TimeZone: "Europe/Moscow"}, events: []domain.Event{{Kind: "snapshot", Answer: &answer}, {Kind: "error", Answer: &answer, Remaining: 5}}}
			h := New(r, Options{OffTopicText: configured})
			ctx := reqctx.WithUserID(t.Context(), user)
			history, err := h.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if err != nil || len(history.GetMessages()) != 1 || history.Messages[0].GetErrorText() != configured || history.Messages[0].GetErrorCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC || history.Messages[0].GetRetryable() {
				t.Fatalf("configured history: %v %v", history, err)
			}
			s := &stream{ctx: ctx}
			if err := h.WatchAnswer(&assistantv1.WatchAnswerRequest{AnswerId: user}, s); err != nil {
				t.Fatal(err)
			}
			if len(s.messages) != 2 || s.messages[0].GetSnapshot().GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_FAILED || s.messages[1].GetError().GetText() != configured || s.messages[1].GetError().GetCode() != assistantv1.AnswerErrorCode_ANSWER_ERROR_CODE_OFF_TOPIC || s.messages[1].GetError().GetRetryable() || s.messages[1].GetError().GetRemainingLimit() != 5 {
				t.Fatalf("configured watch: %v", s.messages)
			}
			// Existing public mapper calls retain their contract default for old callers.
			old, err := Message(domain.Message{ID: user, CreatedAt: now, Answer: &answer})
			if err != nil || old.GetErrorText() != defaultOffTopicText {
				t.Fatalf("default mapper: %v %v", old, err)
			}
		})
	}
}
func TestUnitConfiguredMapperTextDoesNotChangeOtherErrors(t *testing.T) {
	for _, code := range []string{"MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC"} {
		a := domain.Answer{Status: "failed", ErrorCode: code}
		baseline, err := Error(a, 3)
		if err != nil {
			t.Fatal(err)
		}
		custom, err := errorWithText(a, 3, "Серверная подсказка")
		if err != nil {
			t.Fatal(err)
		}
		if code == "OFF_TOPIC" {
			if custom.Text != "Серверная подсказка" {
				t.Fatal(custom)
			}
		} else if baseline.Text != custom.Text || baseline.Code != custom.Code || baseline.Retryable != custom.Retryable {
			t.Fatalf("unrelated error policy changed: %v %v", baseline, custom)
		}
	}
	for _, options := range [][]Options{nil, {{}}} {
		h := New(&reads{}, options...)
		if h.offTopicText != defaultOffTopicText {
			t.Fatal("empty option removed legacy default")
		}
	}
}
