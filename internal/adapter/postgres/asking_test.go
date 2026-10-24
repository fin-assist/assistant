package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/domain"
)

var storeNow = time.Date(2026, 10, 23, 12, 34, 56, 123000000, time.UTC)

func acceptedQuestion(t *testing.T, db *DB, user, text string, now time.Time) domain.GenerationTask {
	t.Helper()
	q := NewQuestions(db)
	var task domain.GenerationTask
	err := db.Run(t.Context(), func(ctx context.Context) error {
		state, err := q.Lock(ctx, user)
		if err != nil {
			return err
		}
		if err := q.Charge(ctx, user, now, state.Used+1); err != nil {
			return err
		}
		task, err = q.Create(ctx, user, text, "without", now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
func storeUsed(t *testing.T, db *DB, user string) int {
	t.Helper()
	var used int
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT limit_used FROM user_state WHERE user_id=$1`, user).Scan(&used); err != nil {
		t.Fatal(err)
	}
	return used
}
func failedQuestion(t *testing.T, db *DB, user string) domain.GenerationTask {
	t.Helper()
	task := acceptedQuestion(t, db, user, "original", storeNow)
	dialogueExec(t, db, `UPDATE answer SET status='failed',charged=false,error_code='MODEL_ERROR',retryable=true WHERE user_id=$1 AND id=$2`, user, task.AnswerID)
	dialogueExec(t, db, `UPDATE user_state SET limit_used=limit_used-1 WHERE user_id=$1`, user)
	return task
}

func TestQuestionsCreateClockChargeAndRollback(t *testing.T) {
	db := dialogueDB(t)
	q := NewQuestions(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	if task.UserID != dialogueUser || task.QuestionID >= task.AnswerID || task.Text != "question" || task.TransferMode != "without" || task.TimeZone != "Europe/Moscow" || !task.CreatedAt.Equal(storeNow) || !task.QuestionCreatedAt.Equal(storeNow) {
		t.Fatalf("task=%+v", task)
	}
	var day string
	var charged bool
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT charge_day::text,charged FROM answer WHERE user_id=$1 AND id=$2`, dialogueUser, task.AnswerID).Scan(&day, &charged); err != nil || day != "2026-10-23" || !charged || storeUsed(t, db, dialogueUser) != 1 {
		t.Fatalf("charge=%q %v %v", day, charged, err)
	}
	dialogueExec(t, db, `UPDATE user_state SET time_zone='Asia/Tokyo' WHERE user_id=$1`, dialogueUser)
	next := acceptedQuestion(t, db, dialogueUser, "next", storeNow.Add(time.Minute))
	if next.TimeZone != "Asia/Tokyo" || next.QuestionID <= task.AnswerID {
		t.Fatalf("new task %+v", next)
	}
	boom := errors.New("rollback")
	err := db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := q.Lock(ctx, dialogueUser); err != nil {
			return err
		}
		if err := q.Charge(ctx, dialogueUser, storeNow, 3); err != nil {
			return err
		}
		if _, err := q.Create(ctx, dialogueUser, "must roll back", "with", storeNow); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || storeUsed(t, db, dialogueUser) != 2 {
		t.Fatalf("rollback %v", err)
	}
	var count int
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM question WHERE user_id=$1`, dialogueUser).Scan(&count); err != nil || count != 2 {
		t.Fatalf("questions %d %v", count, err)
	}
}

func TestQuestionsRequireTransactionAndSanitize(t *testing.T) {
	db := dialogueDB(t)
	q := NewQuestions(db)
	if _, err := q.Lock(t.Context(), dialogueUser); err == nil {
		t.Fatal("Lock without transaction")
	}
	if err := q.Charge(t.Context(), dialogueUser, storeNow, 1); err == nil {
		t.Fatal("Charge without transaction")
	}
	if _, err := q.Create(t.Context(), dialogueUser, "x", "with", storeNow); err == nil {
		t.Fatal("Create without transaction")
	}
	if _, err := q.Retry(t.Context(), dialogueUser, dialogueAnswer, storeNow); err == nil {
		t.Fatal("Retry without transaction")
	}
	for _, used := range []int{0, -1, 32768} {
		err := db.Run(t.Context(), func(ctx context.Context) error {
			if _, err := q.Lock(ctx, dialogueUser); err != nil {
				return err
			}
			return q.Charge(ctx, dialogueUser, storeNow, used)
		})
		if err == nil {
			t.Fatalf("accepted used=%d", used)
		}
	}
	err := db.Run(t.Context(), func(ctx context.Context) error { return q.Charge(ctx, dialogueForeign, storeNow, 1) })
	if !errors.Is(err, asking.ErrDeleted) {
		t.Fatalf("missing user charge: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := q.Lock(ctx, dialogueUser); err == nil {
		t.Fatal("cancelled write accepted")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		_, err := q.Create(ctx, dialogueUser, "private text", "without", storeNow)
		return err
	})
	if err == nil || err.Error() != "question storage unavailable" {
		t.Fatalf("raw SQL error leaked: %v", err)
	}
}

func TestQuestionsRetryPolicyOwnershipAndNotifications(t *testing.T) {
	db := dialogueDB(t)
	q := NewQuestions(db)
	old := failedQuestion(t, db, dialogueUser)
	listener, err := pgx.ConnectConfig(t.Context(), db.primary.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(t.Context(), `LISTEN assistant_answers`); err != nil {
		t.Fatal(err)
	}
	var task domain.GenerationTask
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := q.Lock(ctx, dialogueUser); err != nil {
			return err
		}
		if err := q.Charge(ctx, dialogueUser, storeNow, 1); err != nil {
			return err
		}
		var err error
		task, err = q.Retry(ctx, dialogueUser, old.AnswerID, storeNow.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.QuestionID != old.QuestionID || task.AnswerID <= old.AnswerID || task.Text != old.Text || !task.QuestionCreatedAt.Equal(old.QuestionCreatedAt) || !task.CreatedAt.Equal(storeNow.Add(time.Hour)) {
		t.Fatalf("retry task=%+v", task)
	}
	seen := map[string]bool{}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for range 2 {
		n, err := listener.WaitForNotification(ctx)
		if err != nil {
			t.Fatal(err)
		}
		seen[n.Payload] = true
	}
	if !seen[old.AnswerID] || !seen[task.AnswerID] {
		t.Fatalf("missing retry notifications: %v", seen)
	}
	for _, tc := range []struct {
		user, answer string
		want         error
	}{{dialogueForeign, old.AnswerID, asking.ErrNotFound}, {dialogueUser, dialogueAnswer, asking.ErrNotFound}, {dialogueUser, old.AnswerID, asking.ErrRetryDenied}, {dialogueUser, task.AnswerID, asking.ErrRetryDenied}} {
		err := db.Run(t.Context(), func(ctx context.Context) error { _, err := q.Retry(ctx, tc.user, tc.answer, storeNow); return err })
		if !errors.Is(err, tc.want) {
			t.Fatalf("retry want%v got%v", tc.want, err)
		}
	}
}

func TestQuestionsRetryLegacyAndExplicitPolicy(t *testing.T) {
	for i, tc := range []struct {
		code      string
		retryable *bool
		allowed   bool
	}{{"MODEL_ERROR", nil, true}, {"OFF_TOPIC", nil, false}, {"MODEL_ERROR", new(bool), false}} {
		t.Run(fmt.Sprintf("%s-%d", tc.code, i), func(t *testing.T) {
			db := dialogueDB(t)
			q := NewQuestions(db)
			old := failedQuestion(t, db, dialogueUser)
			dialogueExec(t, db, `UPDATE answer SET error_code=$3,retryable=$4 WHERE user_id=$1 AND id=$2`, dialogueUser, old.AnswerID, tc.code, tc.retryable)
			err := db.Run(t.Context(), func(ctx context.Context) error {
				if _, err := q.Lock(ctx, dialogueUser); err != nil {
					return err
				}
				_, err := q.Retry(ctx, dialogueUser, old.AnswerID, storeNow)
				return err
			})
			if tc.allowed && err != nil || !tc.allowed && !errors.Is(err, asking.ErrRetryDenied) {
				t.Fatalf("policy: %v", err)
			}
		})
	}
}

func TestQuestionsConcurrentRetryKeepsOneReplacementAndCharge(t *testing.T) {
	db := dialogueDB(t)
	q := NewQuestions(db)
	old := failedQuestion(t, db, dialogueUser)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			results <- db.Run(t.Context(), func(ctx context.Context) error {
				state, err := q.Lock(ctx, dialogueUser)
				if err != nil {
					return err
				}
				if err := q.Charge(ctx, dialogueUser, storeNow, state.Used+1); err != nil {
					return err
				}
				_, err = q.Retry(ctx, dialogueUser, old.AnswerID, storeNow)
				return err
			})
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, asking.ErrRetryDenied) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 || storeUsed(t, db, dialogueUser) != 1 {
		t.Fatalf("success=%d denied=%d used=%d", success, denied, storeUsed(t, db, dialogueUser))
	}
	var count int
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM answer WHERE user_id=$1 AND question_id=$2 AND replaced_at IS NULL`, dialogueUser, old.QuestionID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("active count=%d %v", count, err)
	}
}
