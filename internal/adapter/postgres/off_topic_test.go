package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

func offTopicCount(t *testing.T, db *DB, user string) int {
	t.Helper()
	var n int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM off_topic_event WHERE user_id=$1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func offTopicRetry(t *testing.T, db *DB, user string, now time.Time, max int, window time.Duration) time.Time {
	t.Helper()
	q := NewQuestions(db, max, window)
	var at time.Time
	err := db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := q.Lock(ctx, user); err != nil {
			return err
		}
		var err error
		at, err = q.OffTopicRetryAt(ctx, user, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestOffTopicRollingWindowOverflowExactExpiryAndIsolation(t *testing.T) {
	db := dialogueDB(t)
	const window = 7 * 24 * time.Hour
	var tasks []domain.GenerationTask
	for range 7 {
		tasks = append(tasks, acceptedQuestion(t, db, dialogueUser, "off topic", storeNow))
	}
	for i, task := range tasks {
		at := storeNow.Add(time.Duration(i+1) * time.Second)
		if !finishStored(t, db, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, at) {
			t.Fatal("missing transition")
		}
		if finishStored(t, db, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, at) {
			t.Fatal("duplicate counted")
		}
	}
	if offTopicCount(t, db, dialogueUser) != 7 || storeUsed(t, db, dialogueUser) != 0 {
		t.Fatal("ledger/refund mismatch")
	}
	want := storeNow.Add(3*time.Second + window)
	if got := offTopicRetry(t, db, dialogueUser, storeNow.Add(time.Hour), 5, window); !got.Equal(want) {
		t.Fatalf("retry %v want %v", got, want)
	}
	if got := offTopicRetry(t, db, dialogueUser, want.Add(-time.Microsecond), 5, window); !got.Equal(want) {
		t.Fatalf("early release: %v", got)
	}
	if got := offTopicRetry(t, db, dialogueUser, want, 5, window); !got.IsZero() {
		t.Fatalf("exact expiry blocked: %v", got)
	}
	if got := offTopicRetry(t, db, dialogueForeign, storeNow.Add(time.Hour), 5, window); !got.IsZero() {
		t.Fatal("foreign events counted")
	}
	// The configured window, rather than a calendar-week boundary or user zone,
	// determines every expiry, and changing the threshold uses existing events.
	if got := offTopicRetry(t, db, dialogueUser, storeNow.Add(time.Hour), 2, 2*time.Hour); !got.Equal(storeNow.Add(6*time.Second + 2*time.Hour)) {
		t.Fatalf("configured policy: %v", got)
	}
}

func TestOffTopicResetOnlyChargedCompleteAndDailyHistorySurvives(t *testing.T) {
	db := dialogueDB(t)
	for range 2 {
		task := acceptedQuestion(t, db, dialogueUser, "off topic", storeNow)
		finishStored(t, db, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow)
	}
	for _, out := range []generation.Outcome{{ErrorCode: "MODEL_ERROR"}, {ErrorCode: "CANNOT_ANSWER"}, {Charged: false}} {
		task := acceptedQuestion(t, db, dialogueUser, "not a reset", storeNow)
		finishStored(t, db, task, out, storeNow)
		if offTopicCount(t, db, dialogueUser) != 2 {
			t.Fatalf("reset by %+v", out)
		}
	}
	task := acceptedQuestion(t, db, dialogueUser, "financial success", storeNow)
	finishStored(t, db, task, generation.Outcome{Charged: true}, storeNow)
	if offTopicCount(t, db, dialogueUser) != 0 {
		t.Fatal("success did not reset")
	}
	task = acceptedQuestion(t, db, dialogueUser, "after reset at same instant", storeNow)
	var result generation.FinishResult
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		result, err = NewGenerations(db).Finish(ctx, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.OffTopicDailyCount != 3 || offTopicCount(t, db, dialogueUser) != 1 {
		t.Fatalf("reset lost order or daily history: %+v", result)
	}
	if got := offTopicRetry(t, db, dialogueUser, storeNow, 2, 7*24*time.Hour); !got.IsZero() {
		t.Fatal("reset events still block")
	}
}

func TestOffTopicFinishRollbackAndDuplicateConcurrency(t *testing.T) {
	db := dialogueDB(t)
	task := acceptedQuestion(t, db, dialogueUser, "one outcome", storeNow)
	boom := errors.New("rollback after finish")
	err := db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := NewGenerations(db).Finish(ctx, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) || offTopicCount(t, db, dialogueUser) != 0 || storeUsed(t, db, dialogueUser) != 1 {
		t.Fatal("uncommitted ledger or refund escaped")
	}
	var wg sync.WaitGroup
	results := make(chan generation.FinishResult, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			var result generation.FinishResult
			err := db.Run(t.Context(), func(ctx context.Context) error {
				var err error
				result, err = NewGenerations(db).Finish(ctx, task, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow)
				return err
			})
			results <- result
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	applied := 0
	for result := range results {
		if result.Applied {
			applied++
			if result.OffTopicDailyCount != 1 {
				t.Fatal("duplicate daily count")
			}
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if applied != 1 || offTopicCount(t, db, dialogueUser) != 1 || storeUsed(t, db, dialogueUser) != 0 {
		t.Fatal("duplicate terminal accounting")
	}
}

func TestOffTopicResetSerializesConcurrentFinalizations(t *testing.T) {
	for _, resetFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "off_topic_then_reset", true: "reset_then_off_topic"}[resetFirst], func(t *testing.T) {
			db := dialogueDB(t)
			first := acceptedQuestion(t, db, dialogueUser, "first", storeNow)
			second := acceptedQuestion(t, db, dialogueUser, "second", storeNow)
			a, b := generation.Outcome{ErrorCode: "OFF_TOPIC"}, generation.Outcome{Charged: true}
			if resetFirst {
				a, b = b, a
			}
			locked := make(chan struct{})
			release := make(chan struct{})
			done := make(chan error, 2)
			go func() {
				done <- db.Run(t.Context(), func(ctx context.Context) error {
					if _, err := NewGenerations(db).Finish(ctx, first, a, storeNow); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
						return nil
					case <-t.Context().Done():
						return t.Context().Err()
					}
				})
			}()
			select {
			case <-locked:
			case err := <-done:
				t.Fatalf("first finalization failed: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("first finalization did not acquire lock")
			}
			go func() {
				done <- db.Run(t.Context(), func(ctx context.Context) error {
					_, err := NewGenerations(db).Finish(ctx, second, b, storeNow)
					return err
				})
			}()
			close(release)
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			want := 0
			if resetFirst {
				want = 1
			}
			if got := offTopicCount(t, db, dialogueUser); got != want {
				t.Fatalf("ledger=%d want%d", got, want)
			}
		})
	}
}

func TestOffTopicLedgerKeysCascadeAndDeletedUserCannotReappear(t *testing.T) {
	db := dialogueDB(t)
	first := acceptedQuestion(t, db, dialogueUser, "mine", storeNow)
	other := acceptedQuestion(t, db, dialogueForeign, "other", storeNow)
	finishStored(t, db, first, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow)
	finishStored(t, db, other, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow)
	for _, args := range [][]any{{dialogueUser, first.AnswerID, storeNow}, {dialogueUser, other.AnswerID, storeNow}} {
		if _, err := db.Primary().Exec(t.Context(), `INSERT INTO off_topic_event(user_id,answer_id,completed_at) VALUES ($1,$2,$3)`, args...); err == nil {
			t.Fatal("duplicate or foreign answer accepted")
		}
	}
	dialogueExec(t, db, `DELETE FROM user_state WHERE user_id=$1`, dialogueUser)
	if finishStored(t, db, first, generation.Outcome{ErrorCode: "OFF_TOPIC"}, storeNow) {
		t.Fatal("deleted answer finalized")
	}
	if offTopicCount(t, db, dialogueUser) != 0 || offTopicCount(t, db, dialogueForeign) != 1 {
		t.Fatal("cascade crossed users or resurrected data")
	}
}

func TestOffTopicAdmissionRequiresTransactionAndValidPolicy(t *testing.T) {
	db := dialogueDB(t)
	if _, err := NewQuestions(db, 5, 7*24*time.Hour).OffTopicRetryAt(t.Context(), dialogueUser, storeNow); err == nil {
		t.Fatal("accepted without transaction")
	}
	for _, q := range []*Questions{NewQuestions(db, 0, time.Hour), NewQuestions(db, 32768, time.Hour), NewQuestions(db, 5, 0)} {
		if err := db.Run(t.Context(), func(ctx context.Context) error { _, err := q.OffTopicRetryAt(ctx, dialogueUser, storeNow); return err }); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
