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

func savedChart() dialogue.Block {
	return dialogue.Block{Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"expenses","title":"Totals","data":{"kind":"bar","points":[],"value_kind":"amount","exact":9007199254740993}}`)}
}
func savedRows() dialogue.Block {
	return dialogue.Block{Type: "rows", Version: 1, Body: json.RawMessage(`{"alt_text":"categories","data":{"rows":[]}}`)}
}
func appendReadyBlock(t *testing.T, db *DB, task domain.GenerationTask, position int, b dialogue.Block) {
	t.Helper()
	if err := db.Run(t.Context(), func(ctx context.Context) error { return NewGenerations(db).AppendBlock(ctx, task, position, b) }); err != nil {
		t.Fatal(err)
	}
}
func blockWriteError(t *testing.T, db *DB, task domain.GenerationTask, position int, b dialogue.Block) error {
	t.Helper()
	return db.Run(t.Context(), func(ctx context.Context) error { return NewGenerations(db).AppendBlock(ctx, task, position, b) })
}

func TestIntegrationGenerationBlocksCommitRollbackAndNotifications(t *testing.T) {
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
		if err := r.AppendBlock(ctx, task, 0, savedChart()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	dialogueExec(t, db, `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	notification, err := listener.WaitForNotification(ctx)
	if err != nil || notification.Payload != dialogueAnswer {
		t.Fatalf("rolled-back ready block notified: %v %v", notification, err)
	}
	a, err := NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || len(a.Blocks) != 0 {
		t.Fatalf("rolled-back blocks: %+v %v", a.Blocks, err)
	}
	err = db.Run(t.Context(), func(txctx context.Context) error {
		if err := r.AppendBlock(txctx, task, 0, savedChart()); err != nil {
			return err
		}
		// A separately committed sentinel advances LISTEN while the ready block is
		// still uncommitted, proving the block's notification is deferred until commit.
		if _, err := db.Writer(t.Context()).Exec(t.Context(), `SELECT pg_notify('assistant_answers',$1)`, dialogueAnswer); err != nil {
			return err
		}
		notification, err = listener.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification.Payload != dialogueAnswer {
			return errors.New("uncommitted block notified")
		}
		a, err = NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
		if err != nil {
			return err
		}
		if len(a.Blocks) != 0 {
			return errors.New("uncommitted block visible")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	notification, err = listener.WaitForNotification(ctx)
	if err != nil || notification.Payload != task.AnswerID {
		t.Fatalf("committed block notification: %v %v", notification, err)
	}
	appendStored(t, db, task, 1, "after chart")
	appendStored(t, db, task, 1, " and more")
	appendReadyBlock(t, db, task, 2, savedRows())
	a, err = NewDialogue(db).Snapshot(t.Context(), dialogueUser, task.AnswerID)
	if err != nil || len(a.Blocks) != 3 || a.Blocks[0].Type != "chart" || a.Blocks[1].Text != "after chart and more" || a.Blocks[2].Type != "rows" {
		t.Fatalf("ordered blocks: %+v %v", a.Blocks, err)
	}
	if !finishStored(t, db, task, generation.Outcome{Charged: true, Blocks: []dialogue.Block{savedChart(), {Type: "text", Version: 1, Text: "after chart and more final"}, savedRows()}}, storeNow) {
		t.Fatal("complete transition not applied")
	}
}

func TestIntegrationGenerationBlocksValidateAndNeverReplaceSavedData(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	if err := r.AppendBlock(t.Context(), task, 0, savedChart()); err == nil {
		t.Fatal("ready block accepted without transaction")
	}
	for _, b := range []dialogue.Block{
		{Type: "text", Version: 1, Text: "text"}, {Type: "chart", Version: 2, Body: savedChart().Body}, {Type: "other", Version: 1, Body: savedChart().Body},
		{Type: "chart", Version: 1, Body: json.RawMessage(`{`)}, {Type: "chart", Version: 1, Body: json.RawMessage(`null`)}, {Type: "chart", Version: 1, Body: json.RawMessage(`[]`)},
		{Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"  ","data":{}}`)}, {Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"alt"}`)},
		{Type: "chart", Version: 1, Body: json.RawMessage(`{"alt_text":"alt","data":[]}`)}, {Type: "chart", Version: 1, Text: "unexpected", Body: savedChart().Body},
	} {
		if err := blockWriteError(t, db, task, 0, b); err == nil {
			t.Fatalf("invalid ready block accepted: %+v", b)
		}
	}
	for _, position := range []int{-1, 1, 32768} {
		if err := blockWriteError(t, db, task, position, savedChart()); err == nil {
			t.Fatalf("invalid position%d accepted", position)
		}
	}
	err := db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 1, "gap") })
	if err == nil {
		t.Fatal("text gap accepted")
	}
	appendReadyBlock(t, db, task, 0, savedChart())
	if err := blockWriteError(t, db, task, 0, savedChart()); err == nil {
		t.Fatal("duplicate ready block silently replaced")
	}
	if err := blockWriteError(t, db, task, 0, savedRows()); err == nil {
		t.Fatal("existing block type replaced")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error { return r.Append(ctx, task, 0, "overwrite chart") })
	if err == nil {
		t.Fatal("text silently replaced chart")
	}
	if err := blockWriteError(t, db, task, 2, savedRows()); err == nil {
		t.Fatal("structured gap accepted")
	}
	dialogueExec(t, db, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,2,'text',1,'invalid legacy gap')`, task.UserID, task.AnswerID)
	if err := blockWriteError(t, db, task, 3, savedRows()); err == nil {
		t.Fatal("noncontiguous stored prefix extended")
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true, Blocks: []dialogue.Block{
			savedChart(), {Type: "text", Version: 1, Text: "filler"}, {Type: "text", Version: 1, Text: "invalid legacy gap"},
		}}, storeNow))
		return err
	})
	if err == nil {
		t.Fatal("completion silently repaired a noncontiguous published prefix")
	}
}

func TestIntegrationGenerationBlocksFinishPreservesWholePrefixSemantically(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	appendStored(t, db, task, 0, "prefix")
	appendReadyBlock(t, db, task, 1, savedChart())
	appendReadyBlock(t, db, task, 2, savedRows())
	valid := []dialogue.Block{{Type: "text", Version: 1, Text: "prefix final"}, savedChart(), savedRows()}
	for _, tc := range []string{"missing", "type", "version", "body", "number", "reordered", "invalid-json"} {
		t.Run(tc, func(t *testing.T) {
			blocks := append([]dialogue.Block(nil), valid...)
			switch tc {
			case "missing":
				blocks = blocks[:2]
			case "type":
				blocks[1].Type = "rows"
			case "version":
				blocks[1].Version = 2
			case "body":
				blocks[2].Body = json.RawMessage(`{"alt_text":"changed","data":{"rows":[]}}`)
			case "number":
				blocks[1].Body = json.RawMessage(strings.Replace(string(savedChart().Body), "9007199254740993", "9007199254740992", 1))
			case "reordered":
				blocks[1], blocks[2] = blocks[2], blocks[1]
			case "invalid-json":
				blocks[1].Body = json.RawMessage(`{`)
			}
			err := db.Run(t.Context(), func(ctx context.Context) error {
				_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true, Blocks: blocks}, storeNow))
				return err
			})
			if err == nil {
				t.Fatal("changed published prefix accepted")
			}
		})
	}
	// PostgreSQL jsonb reformats keys and numbers: semantically equal body variants
	// must survive completion, including exact integers larger than float64 precision.
	valid[1].Body = json.RawMessage(`{ "data": { "exact": 9007199254740993.0, "value_kind":"amount", "points":[], "kind":"bar" }, "title":"Totals", "alt_text":"expenses" }`)
	if !finishStored(t, db, task, generation.Outcome{Charged: true, Blocks: valid}, storeNow) {
		t.Fatal("semantic JSON equality rejected")
	}
	a, err := NewDialogue(db).Snapshot(t.Context(), task.UserID, task.AnswerID)
	if err != nil || a.Status != "complete" || len(a.Blocks) != 3 || a.Blocks[0].Text != "prefix final" {
		t.Fatalf("saved completion: %+v %v", a, err)
	}
}

func TestIntegrationGenerationBlocksStoppedOwnershipAndFailure(t *testing.T) {
	for _, state := range []string{"failed", "complete", "replaced", "deleted", "foreign"} {
		t.Run(state, func(t *testing.T) {
			db := dialogueDB(t)
			task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
			appendReadyBlock(t, db, task, 0, savedChart())
			appendStored(t, db, task, 1, "partial")
			switch state {
			case "failed":
				finishStored(t, db, task, generation.Outcome{ErrorCode: "MODEL_ERROR", Blocks: []dialogue.Block{savedChart()}}, storeNow)
				a, err := NewDialogue(db).Snapshot(t.Context(), task.UserID, task.AnswerID)
				if err != nil || len(a.Blocks) != 0 || a.Charged {
					t.Fatalf("failure retained partial: %+v %v", a, err)
				}
			case "complete":
				finishStored(t, db, task, generation.Outcome{Charged: true, Blocks: []dialogue.Block{savedChart(), {Type: "text", Version: 1, Text: "partial"}}}, storeNow)
			case "replaced":
				dialogueExec(t, db, `UPDATE answer SET replaced_at=$3 WHERE user_id=$1 AND id=$2`, task.UserID, task.AnswerID, storeNow)
			case "deleted":
				dialogueExec(t, db, `DELETE FROM user_state WHERE user_id=$1`, task.UserID)
			case "foreign":
				task.UserID = dialogueForeign
			}
			if err := blockWriteError(t, db, task, 2, savedRows()); !errors.Is(err, generation.ErrStopped) {
				t.Fatalf("late block state%s: %v", state, err)
			}
		})
	}
}

func TestIntegrationGenerationBlocksSerializeWithFinishBothOrders(t *testing.T) {
	for _, blockFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "block-first", false: "finish-first"}[blockFirst], func(t *testing.T) {
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
				if blockFirst {
					if err := r.AppendBlock(ctx, task, 0, savedChart()); err != nil {
						return err
					}
					go func() {
						done <- db.Run(t.Context(), func(ctx context.Context) error {
							_, err := r.Finish(ctx, task, generation.PrepareFinish(generation.Outcome{Charged: true, Blocks: []dialogue.Block{savedChart()}}, storeNow))
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
					go func() { done <- blockWriteError(t, db, task, 0, savedChart()) }()
				}
				return storeWaitBlocked(t, db, pid)
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if blockFirst && err != nil || !blockFirst && !errors.Is(err, generation.ErrStopped) {
					t.Fatalf("serialized ready block: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ready-block race did not complete")
			}
		})
	}
}

func TestIntegrationGenerationBlocksConcurrentSamePositionDoesNotReplaceWinner(t *testing.T) {
	db := dialogueDB(t)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, block := range []dialogue.Block{savedChart(), savedRows()} {
		wg.Go(func() { <-start; results <- blockWriteError(t, db, task, 0, block) })
	}
	close(start)
	wg.Wait()
	close(results)
	winners, losers := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners%d losers%d", winners, losers)
	}
	a, err := NewDialogue(db).Snapshot(t.Context(), task.UserID, task.AnswerID)
	if err != nil || len(a.Blocks) != 1 {
		t.Fatalf("winner snapshot: %+v %v", a, err)
	}
}

func TestIntegrationGenerationBlocksCancellationAndStorageErrorsAreSafe(t *testing.T) {
	db := dialogueDB(t)
	r := NewGenerations(db)
	task := acceptedQuestion(t, db, dialogueUser, "question", storeNow)
	err := db.Run(t.Context(), func(ctx context.Context) error {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return r.AppendBlock(cancelled, task, 0, savedChart())
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ready block: %v", err)
	}
	invalid := task
	invalid.UserID = "private-invalid-identifier"
	if err := blockWriteError(t, db, invalid, 0, savedChart()); err == nil || err.Error() != "generation storage unavailable" {
		t.Fatalf("ready block query leaked storage details: %v", err)
	}
	// Go accepts this JSON number but PostgreSQL jsonb cannot represent its exponent;
	// the INSERT error must roll back and contain no raw body or SQL details.
	huge := savedChart()
	huge.Body = json.RawMessage(`{"alt_text":"private financial details","data":{"number":1e999999}}`)
	if err := blockWriteError(t, db, task, 0, huge); err == nil || err.Error() != "generation storage unavailable" {
		t.Fatalf("ready block INSERT error: %v", err)
	}
	dialogueExec(t, db, `DROP TABLE answer_block`)
	if err := blockWriteError(t, db, task, 0, savedChart()); err == nil || err.Error() != "generation storage unavailable" {
		t.Fatalf("ready block count leaked storage details: %v", err)
	}
}
