package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

func TestUnitWritesRefuseMissingServiceTransaction(t *testing.T) {
	db := unitDatabase()
	accounts := NewAccounts(db)
	questions := NewQuestions(db)
	answers := NewGenerations(db)
	task := domain.GenerationTask{UserID: "user", QuestionID: "question", AnswerID: "answer"}
	for name, call := range map[string]func(context.Context) error{
		"timezone": func(ctx context.Context) error { return accounts.SetTimezone(ctx, "user", "UTC", 1) },
		"purge":    func(ctx context.Context) error { return accounts.Delete(ctx, "user") },
		"lock":     func(ctx context.Context) error { _, err := questions.Lock(ctx, "user"); return err },
		"charge":   func(ctx context.Context) error { return questions.Charge(ctx, "user", time.Now(), 1) },
		"create": func(ctx context.Context) error {
			_, err := questions.Create(ctx, "user", "question", "without", time.Now())
			return err
		},
		"retry": func(ctx context.Context) error {
			_, err := questions.Retry(ctx, "user", "answer", time.Now())
			return err
		},
		"off-topic": func(ctx context.Context) error {
			_, err := questions.RecentOffTopic(ctx, "user", time.Now(), 5)
			return err
		},
		"append-text":  func(ctx context.Context) error { return answers.Append(ctx, task, 0, "text") },
		"append-block": func(ctx context.Context) error { return answers.AppendBlock(ctx, task, 0, dialogue.Block{}) },
		"finish": func(ctx context.Context) error {
			_, err := answers.Finish(ctx, task, generation.Terminal{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(t.Context()); err == nil || !strings.Contains(err.Error(), "transaction") {
				t.Fatalf("write escaped missing-transaction guard: %v", err)
			}
		})
	}
}

func TestUnitRepositoryReadBoundsRejectBeforeDatabaseAccess(t *testing.T) {
	db := unitDatabase()
	for _, limit := range []int{-1, 0, 51} {
		if _, err := NewDialogue(db).List(t.Context(), "user", "", limit); err == nil {
			t.Fatalf("invalid dialogue limit %d", limit)
		}
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := NewGenerations(db).Expired(t.Context(), time.Time{}, limit); err == nil {
			t.Fatalf("invalid expired limit %d", limit)
		}
	}
	history, err := NewGenerations(db).History(t.Context(), domain.GenerationTask{}, 0)
	if err != nil || len(history) != 0 {
		t.Fatalf("zero history budget=%v %v", history, err)
	}
}

func TestUnitStorageFailuresPreserveCancellationAndHideDetails(t *testing.T) {
	for name, convert := range map[string]func(context.Context) error{"account": storageFailure, "question": questionFailure, "generation": generationFailure, "dialogue": dialogueFailure} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if !errors.Is(convert(ctx), context.Canceled) {
				t.Fatal("caller cancellation lost")
			}
			err := convert(t.Context())
			if err == nil || err.Error() != name+" storage unavailable" {
				t.Fatalf("storage error=%v", err)
			}
		})
	}
}
