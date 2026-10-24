package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

func finishStored(t *testing.T, db *DB, task domain.GenerationTask, out generation.Outcome, now time.Time) bool {
	t.Helper()
	var changed generation.FinishResult
	err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		changed, err = NewGenerations(db).Finish(ctx, task, generation.PrepareFinish(out, now))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return changed.Applied
}
func appendStored(t *testing.T, db *DB, task domain.GenerationTask, position int, text string) {
	t.Helper()
	if err := db.Run(t.Context(), func(ctx context.Context) error { return NewGenerations(db).Append(ctx, task, position, text) }); err != nil {
		t.Fatal(err)
	}
}
func TestGenerationsAppendAndCompletePreservesFieldsAndClock(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	appendStored(t, db, task, 0, "first")
	appendStored(t, db, task, 0, " second")
	from, to := storeNow.Add(-24*time.Hour), storeNow
	out := generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "first second final"}, {Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"chart alternative","data":{"points":[]}}`)}}, Chips: []json.RawMessage{json.RawMessage(`{"kind":"operations"}`)}, Source: &dialogue.Source{CalculatedAt: storeNow, From: &from, To: &to}}
	finished := storeNow.Add(5 * time.Second)
	if !finishStored(t, db, task, out, finished) {
		t.Fatal("no terminal transition")
	}
	a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "complete" || !a.Charged || a.Retryable != nil || len(a.Blocks) != 2 || a.Blocks[0].Text != "first second final" || len(a.Blocks[1].Body) == 0 || len(a.Chips) != 1 || a.Source == nil || !a.Source.CalculatedAt.Equal(storeNow) || !a.Source.From.Equal(from) || !a.Source.To.Equal(to) {
		t.Fatalf("outcome not saved: %+v", a)
	}
	var actual time.Time
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT finished_at FROM answer WHERE user_id=$1 AND id=$2`, dialogueUser, task.AnswerID).Scan(&actual); err != nil || !actual.Equal(finished) {
		t.Fatalf("finished at=%v %v", actual, err)
	}
	if finishStored(t, db, task, generation.Outcome{ErrorCode: "MODEL_ERROR"}, finished) {
		t.Fatal("second terminal transition applied")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "late") })
	if !errors.Is(err, generation.ErrStopped) {
		t.Fatalf("late append: %v", err)
	}
	if storeUsed(t, db, dialogueUser) != 1 {
		t.Fatal("charged completion refunded")
	}
}

func TestGenerationsFailureClearsAndRefundsOnce(t *testing.T) {
	for _, code := range []string{"MODEL_ERROR", "CANNOT_ANSWER"} {
		t.Run(code, func(t *testing.T) {
			db := dialogueDB(t)
			task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
			appendStored(t, db, task, 0, "partial")
			dialogueExec(t, db, `UPDATE answer SET calculated_at=$3,data_from=$3,data_to=$3,chips='[{"private":"chip"}]' WHERE user_id=$1 AND id=$2`, dialogueUser, task.AnswerID, storeNow)
			if !finishStored(t, db, task, generation.Outcome{ErrorCode: code, Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "must be discarded"}}, Chips: []json.RawMessage{json.RawMessage(`{"private":"discard"}`)}, Source: &dialogue.Source{CalculatedAt: storeNow}}, storeNow) {
				t.Fatal("no transition")
			}
			a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != "failed" || a.ErrorCode != code || a.Retryable == nil || *a.Retryable != (code == "MODEL_ERROR") || a.Charged || len(a.Blocks) != 0 || len(a.Chips) != 0 || a.Source != nil || storeUsed(t, db, dialogueUser) != 0 {
				t.Fatalf("failed state=%+v", a)
			}
			if finishStored(t, db, task, generation.Outcome{ErrorCode: code}, storeNow) {
				t.Fatal("second refund")
			}
		})
	}
}
func TestGenerationsRefundOnlyChargedDayAndCompleteUncharged(t *testing.T) {
	for _, past := range []bool{false, true} {
		t.Run(map[bool]string{false: "today", true: "past"}[past], func(t *testing.T) {
			db := dialogueDB(t)
			task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
			if past {
				dialogueExec(t, db, `UPDATE user_state SET limit_day='2026-10-24',limit_used=5 WHERE user_id=$1`, dialogueUser)
			}
			if !finishStored(t, db, task, generation.Outcome{Charged: false}, storeNow.Add(24*time.Hour)) {
				t.Fatal("no transition")
			}
			want := 0
			if past {
				want = 5
			}
			if got := storeUsed(t, db, dialogueUser); got != want {
				t.Fatalf("used=%d want%d", got, want)
			}
		})
	}
}
func TestGenerationsRollbackAndStablePrefix(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	boom := errors.New("rollback")
	err := db.Run(t.Context(), func(ctx context.Context) error {
		if err := r.Append(ctx, task, 0, "uncommitted"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || len(a.Blocks) != 0 {
		t.Fatalf("append rollback %+v %v", a, err)
	}
	appendStored(t, db, task, 0, "prefix")
	for _, blocks := range [][]dialogue.Block{nil, {{Type: "text", Version: 1, Text: "wrong"}}, {{Type: "text", Version: 2, Text: "prefix"}}, {{Type: "chart", Version: 1, Body: json.RawMessage(`{}`)}}} {
		err := db.Run(t.Context(), func(ctx context.Context) error {
			_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true, Blocks: blocks}, storeNow))
			return err
		})
		if err == nil {
			t.Fatal("regressed prefix accepted")
		}
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	a, err = NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || a.Status != "generating" || a.Blocks[0].Text != "prefix" || storeUsed(t, db, dialogueUser) != 1 {
		t.Fatalf("finish rollback %+v %v", a, err)
	}
	dialogueExec(t, db, `UPDATE user_state SET limit_used=0 WHERE user_id=$1`, dialogueUser)
	err = db.Run(t.Context(), func(ctx context.Context) error {
		_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow))
		return err
	})
	if err == nil {
		t.Fatal("underflow counter accepted")
	}
	a, err = NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || a.Status != "generating" {
		t.Fatalf("invalid refund was not rolled back: %+v %v", a, err)
	}
}

func TestGenerationsConcurrentFinishRefundsExactlyOnce(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			var changed generation.FinishResult
			err := db.Run(t.Context(), func(ctx context.Context) error {
				var err error
				changed, err = r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow))
				return err
			})
			results <- changed.Applied
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	changes := 0
	for changed := range results {
		if changed {
			changes++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if changes != 1 || storeUsed(t, db, dialogueUser) != 0 {
		t.Fatalf("changes=%d used=%d", changes, storeUsed(t, db, dialogueUser))
	}
}

func storeWaitBlocked(t *testing.T, db *DB, pid int32) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := db.primary.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("expected lock wait")
		case <-tick.C:
		}
	}
}
func TestGenerationsAppendAndFinishSerializeBothOrders(t *testing.T) {
	for _, appendFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "append-first", false: "finish-first"}[appendFirst], func(t *testing.T) {
			db := dialogueDB(t)
			r := NewGenerations(db)
			task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
			done := make(chan error, 1)
			err := db.Run(t.Context(), func(ctx context.Context) error {
				held, _ := db.Tx(ctx)
				var pid int32
				if err := held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				if appendFirst {
					if err := r.Append(ctx, task, 0, "prefix"); err != nil {
						return err
					}
					go func() {
						done <- db.Run(t.Context(), func(ctx context.Context) error {
							_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "prefix final"}}}, storeNow))
							return err
						})
					}()
				} else {
					if changed, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow)); err != nil || !changed.Applied {
						if err != nil {
							return err
						}
						return errors.New("finish did not apply")
					}
					go func() {
						done <- db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "late") })
					}()
				}
				return storeWaitBlocked(t, db, pid)
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if appendFirst && err != nil || !appendFirst && !errors.Is(err, generation.ErrStopped) {
					t.Fatalf("serialized error=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked write did not complete")
			}
			a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
			if err != nil {
				t.Fatal(err)
			}
			if appendFirst && (a.Status != "complete" || a.Blocks[0].Text != "prefix final") || !appendFirst && (a.Status != "failed" || len(a.Blocks) != 0) {
				t.Fatalf("serialized state %+v", a)
			}
		})
	}
}

func TestGenerationsOwnershipDeletionReplacementAndTransaction(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	if err := r.Append(t.Context(), task, 0, "x"); err == nil {
		t.Fatal("append outside transaction")
	}
	if _, err := r.Finish(t.Context(), task, generation.PrepareFinish(generation.Outcome{}, storeNow)); err == nil {
		t.Fatal("finish outside transaction")
	}
	foreign := task
	foreign.UserID = dialogueForeign
	if finishStored(t, db, foreign, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow) {
		t.Fatal("foreign finish")
	}
	err := db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, foreign, 0, "foreign") })
	if !errors.Is(err, generation.ErrStopped) {
		t.Fatalf("foreign append %v", err)
	}
	dialogueExec(t, db, `UPDATE answer SET replaced_at=$3 WHERE user_id=$1 AND id=$2`, dialogueUser, task.AnswerID, storeNow)
	if finishStored(t, db, task, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow) {
		t.Fatal("replaced finish")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "replaced") })
	if !errors.Is(err, generation.ErrStopped) {
		t.Fatalf("replaced append %v", err)
	}
	dialogueExec(t, db, `DELETE FROM user_state WHERE user_id=$1`, dialogueUser)
	if finishStored(t, db, task, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow) {
		t.Fatal("deleted finish resurrected user")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "deleted") })
	if !errors.Is(err, generation.ErrStopped) {
		t.Fatalf("deleted append %v", err)
	}
	for _, position := range []int{-1, 32768} {
		err := db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, position, "x") })
		if err == nil {
			t.Fatal("invalid position accepted")
		}
	}
}

func TestGenerationsHistoryWholeLatestCompletePairs(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	first := acceptedQuestion(t, db, dialogueUser, "q1", storeNow)
	finishStored(t, db, first, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "a1"}}}, storeNow)
	failed := acceptedQuestion(t, db, dialogueUser, "must exclude failed", storeNow)
	finishStored(t, db, failed, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow)
	second := acceptedQuestion(t, db, dialogueUser, "q2", storeNow)
	finishStored(t, db, second, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"a2","data":{"private":"not sent"}}`)}}}, storeNow)
	other := acceptedQuestion(t, db, dialogueForeign, "private foreign", storeNow)
	finishStored(t, db, other, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "foreign"}}}, storeNow)
	current := acceptedQuestion(t, db, dialogueUser, "exclude current", storeNow)
	finishStored(t, db, current, generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "current"}}}, storeNow)
	history, err := r.History(t.Context(), current, 8)
	if err != nil || len(history) != 2 || history[0].Question != "q1" || history[0].Answer != "a1" || history[1].Question != "q2" || history[1].Answer != "a2" {
		t.Fatalf("history=%+v %v", history, err)
	}
	history, err = r.History(t.Context(), current, 7)
	if err != nil || len(history) != 1 || history[0].Question != "q2" {
		t.Fatalf("bounded history=%+v %v", history, err)
	}
	for _, budget := range []int{-1, 0, 3} {
		history, err = r.History(t.Context(), current, budget)
		if err != nil || len(history) != 0 {
			t.Fatalf("budget%d history=%+v %v", budget, history, err)
		}
	}
	dialogueExec(t, db, `UPDATE answer SET replaced_at=$3 WHERE user_id=$1 AND id=$2`, dialogueUser, second.AnswerID, storeNow)
	history, err = r.History(t.Context(), current, 100)
	if err != nil || len(history) != 1 || history[0].Question != "q1" {
		t.Fatalf("replaced history=%+v %v", history, err)
	}
}

func TestGenerationsExpiredBatchAndFailures(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	first := acceptedQuestion(t, db, dialogueUser, "first", storeNow)
	second := acceptedQuestion(t, db, dialogueForeign, "second", storeNow.Add(time.Minute))
	third := acceptedQuestion(t, db, dialogueUser, "third", storeNow.Add(2*time.Minute))
	expired, err := r.Expired(t.Context(), storeNow, 1)
	if err != nil || len(expired) != 1 || expired[0] != first {
		t.Fatalf("expired=%+v %v", expired, err)
	}
	expired, err = r.Expired(t.Context(), storeNow.Add(time.Minute), 10)
	if err != nil || len(expired) != 2 || expired[1] != second {
		t.Fatalf("inclusive batch=%+v %v", expired, err)
	}
	dialogueExec(t, db, `UPDATE answer SET replaced_at=$3 WHERE user_id=$1 AND id=$2`, first.UserID, first.AnswerID, storeNow)
	finishStored(t, db, second, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow)
	expired, err = r.Expired(t.Context(), storeNow.Add(time.Hour), 1)
	if err != nil || len(expired) != 1 || expired[0] != third {
		t.Fatalf("terminal/replaced excluded=%+v %v", expired, err)
	}
	for _, limit := range []int{0, 1001} {
		if _, err := r.Expired(t.Context(), storeNow, limit); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.History(ctx, third, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("history cancellation %v", err)
	}
	if _, err := r.Expired(ctx, storeNow, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired cancellation %v", err)
	}
	dialogueExec(t, db, `DROP TABLE answer_block`)
	if _, err := r.History(t.Context(), third, 100); err == nil || strings.Contains(err.Error(), "answer_block") {
		t.Fatalf("raw history storage error %v", err)
	}
}

func TestGenerationsWriteNotificationsOnlyAfterCommit(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	listener, err := pgx.ConnectConfig(t.Context(), db.primary.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(t.Context(), `LISTEN assistant_answers`); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("rollback")
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if err := r.Append(ctx, task, 0, "rolled back"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// A later committed sentinel is a barrier: a rolled-back notification must not
	// appear before it in this already-established listener's ordered queue.
	dialogueExec(t, db, `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(ctx)
	if err != nil || n.Payload != dialogueAnswer {
		t.Fatalf("rolled-back append notified: %v %v", n, err)
	}
	appendStored(t, db, task, 0, "committed")
	n, err = listener.WaitForNotification(ctx)
	if err != nil || n.Payload != task.AnswerID {
		t.Fatalf("append notification: %v %v", n, err)
	}
	if !finishStored(t, db, task, generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow) {
		t.Fatal("no terminal transition")
	}
	n, err = listener.WaitForNotification(ctx)
	if err != nil || n.Payload != task.AnswerID {
		t.Fatalf("finish notification: %v %v", n, err)
	}
}

func TestGenerationsRejectNonTextAppendAndSanitizeWriteFailures(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	dialogueExec(t, db, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,body) VALUES ($1,$2,0,'chart',1,'{"alt_text":"keep"}')`, dialogueUser, task.AnswerID)
	err := db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "must not overwrite") })
	if err == nil {
		t.Fatal("append overwrote non-text block")
	}
	var blockType string
	if err := db.Writer(t.Context()).QueryRow(t.Context(), `SELECT type FROM answer_block WHERE user_id=$1 AND answer_id=$2 AND position=0`, dialogueUser, task.AnswerID).Scan(&blockType); err != nil || blockType != "chart" {
		t.Fatalf("blockType=%q %v", blockType, err)
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return r.Append(cancelled, task, 0, "x")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation: %v", err)
	}
	dialogueExec(t, db, `DROP TABLE answer_block`)
	err = db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "private dialogue") })
	if err == nil || err.Error() != "generation storage unavailable" {
		t.Fatalf("append raw failure: %v", err)
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true}, storeNow))
		return err
	})
	if err == nil || err.Error() != "generation storage unavailable" {
		t.Fatalf("finish raw failure: %v", err)
	}
}
