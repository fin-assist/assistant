package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/domain"
)

const unitNewAnswer = "30000000-0000-0000-0000-000000000003"

func answerRow() unitRow {
	return row(unitNewAnswer, storeNow, "saved question", "without", storeNow.Add(-time.Minute), "Europe/Moscow")
}

func TestUnitQuestionsTranslateWriteAndLookupOutcomes(t *testing.T) {
	q := NewQuestions(unitDatabase())
	t.Run("lock", func(t *testing.T) {
		tx := &unitTx{affected: 1, rows: []unitRow{row(pgtype.Date{Time: storeNow, Valid: true}, 2)}}
		state, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) (account.State, error) { return q.Lock(ctx, dialogueUser) })
		if err != nil || state.Used != 2 || !state.LimitDay.Equal(storeNow) {
			t.Fatalf("state=%+v err=%v", state, err)
		}
	})
	for _, tc := range []struct {
		name     string
		used     int
		affected int64
		fail     int
		want     error
	}{
		{name: "charge", used: 1, affected: 1},
		{name: "deleted", used: 1, want: asking.ErrDeleted},
		{name: "driver", used: 1, affected: 1, fail: 1},
		{name: "negative-counter", used: -1},
		{name: "counter-overflow", used: 32768},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &unitTx{affected: tc.affected, failExec: tc.fail}
			err := unitExecute(t, q.db, tx, func(ctx context.Context) error { return q.Charge(ctx, dialogueUser, storeNow, tc.used) })
			if tc.name == "charge" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("invalid charge result=%v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got%v want%v", err, tc.want)
			}
			if (tc.used < 1 || tc.used > 32767) && tx.execs != 0 {
				t.Fatal("invalid counter reached storage")
			}
		})
	}
	t.Run("create", func(t *testing.T) {
		tx := &unitTx{affected: 1, rows: []unitRow{row(dialogueQuestion), answerRow()}}
		task, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) (domain.GenerationTask, error) {
			return q.Create(ctx, dialogueUser, "requested", "without", storeNow)
		})
		if err != nil || task.UserID != dialogueUser || task.QuestionID != dialogueQuestion || task.AnswerID != unitNewAnswer || task.Text != "saved question" || !task.CreatedAt.Equal(storeNow) {
			t.Fatalf("task=%+v error=%v", task, err)
		}
	})
	for _, tc := range []struct {
		name     string
		rows     []unitRow
		failExec int
	}{
		{name: "insert", rows: []unitRow{{err: errUnitDriver}}},
		{name: "answer", rows: []unitRow{row(dialogueQuestion), {err: errUnitDriver}}},
		{name: "notify", rows: []unitRow{row(dialogueQuestion), answerRow()}, failExec: 1},
	} {
		t.Run("create-"+tc.name, func(t *testing.T) {
			tx := &unitTx{affected: 1, rows: tc.rows, failExec: tc.failExec}
			task, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) (domain.GenerationTask, error) {
				return q.Create(ctx, dialogueUser, "requested", "without", storeNow)
			})
			if err == nil || task != (domain.GenerationTask{}) || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("failed create leaked task/details: %+v %v", task, err)
			}
		})
	}
}

func TestUnitRetryUsesSavedPolicyAndDoesNotExposeForeignAnswer(t *testing.T) {
	q := NewQuestions(unitDatabase())
	retryable := true
	denied := false
	modelError := "MODEL_ERROR"
	otherError := "CANNOT_ANSWER"
	for _, tc := range []struct {
		name, status string
		code         *string
		retryable    *bool
		replaced     *time.Time
		lookup       error
		want         error
	}{
		{name: "retryable", status: "failed", retryable: &retryable},
		{name: "legacy-model", status: "failed", code: &modelError},
		{name: "explicit-denied", status: "failed", code: &modelError, retryable: &denied, want: asking.ErrRetryDenied},
		{name: "legacy-denied", status: "failed", code: &otherError, want: asking.ErrRetryDenied},
		{name: "already-replaced", status: "failed", retryable: &retryable, replaced: &storeNow, want: asking.ErrRetryDenied},
		{name: "complete", status: "complete", retryable: &retryable, want: asking.ErrRetryDenied},
		{name: "foreign-or-absent", lookup: pgx.ErrNoRows, want: asking.ErrNotFound},
		{name: "driver", lookup: errUnitDriver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := row(dialogueQuestion, tc.status, tc.code, tc.retryable, tc.replaced)
			lookup.err = tc.lookup
			tx := &unitTx{affected: 1, rows: []unitRow{lookup, answerRow()}}
			task, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) (domain.GenerationTask, error) {
				return q.Retry(ctx, dialogueUser, dialogueAnswer, storeNow)
			})
			if tc.want != nil {
				if !errors.Is(err, tc.want) || tx.execs != 0 {
					t.Fatalf("refusal=%v effects=%d", err, tx.execs)
				}
				return
			}
			if tc.lookup != nil {
				if err == nil || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("driver error=%v", err)
				}
				return
			}
			if err != nil || task.QuestionID != dialogueQuestion || task.Text != "saved question" || task.AnswerID != unitNewAnswer {
				t.Fatalf("retry task=%+v error=%v", task, err)
			}
		})
	}
	for fail := 1; fail <= 3; fail++ {
		tx := &unitTx{affected: 1, failExec: fail, rows: []unitRow{row(dialogueQuestion, "failed", &modelError, &retryable, (*time.Time)(nil)), answerRow()}}
		if task, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) (domain.GenerationTask, error) {
			return q.Retry(ctx, dialogueUser, dialogueAnswer, storeNow)
		}); err == nil || task != (domain.GenerationTask{}) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("failed retry returned result=%+v error=%v", task, err)
		}
	}
}
