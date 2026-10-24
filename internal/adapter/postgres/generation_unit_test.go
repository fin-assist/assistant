package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

func unitTask() domain.GenerationTask {
	return domain.GenerationTask{UserID: dialogueUser, QuestionID: dialogueQuestion, AnswerID: dialogueAnswer}
}

func TestUnitAppendRejectsStoppedAndInconsistentAnswers(t *testing.T) {
	repo := NewGenerations(unitDatabase())
	for _, tc := range []struct {
		name     string
		position int
		rows     []unitRow
		affected int64
		fail     int
		want     error
		valid    bool
	}{
		{name: "success", rows: []unitRow{row(true), row(0, 0, 0)}, affected: 1, valid: true},
		{name: "stopped", rows: []unitRow{{err: pgx.ErrNoRows}}, want: generation.ErrStopped},
		{name: "invalid-position", position: -1},
		{name: "driver-read", rows: []unitRow{{err: errUnitDriver}}},
		{name: "saved-count-driver", rows: []unitRow{row(true), {err: errUnitDriver}}},
		{name: "noncontiguous-existing", rows: []unitRow{row(true), row(1, 1, 2)}},
		{name: "skipped-position", position: 2, rows: []unitRow{row(true), row(0, 0, 0)}},
		{name: "non-text-conflict", rows: []unitRow{row(true), row(0, 0, 0)}},
		{name: "insert-failure", rows: []unitRow{row(true), row(0, 0, 0)}, affected: 1, fail: 1},
		{name: "notify-failure", rows: []unitRow{row(true), row(0, 0, 0)}, affected: 1, fail: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &unitTx{rows: tc.rows, affected: tc.affected, failExec: tc.fail}
			err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.Append(ctx, unitTask(), tc.position, "new text") })
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("invalid append result=%v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got%v want%v", err, tc.want)
			}
			if tc.want != nil && tx.execs != 0 {
				t.Fatal("stopped answer received write")
			}
		})
	}
}

func TestUnitReadyBlockValidationAndStopPolicy(t *testing.T) {
	repo := NewGenerations(unitDatabase())
	for _, tc := range []struct {
		name  string
		block dialogue.Block
		rows  []unitRow
		fail  int
		valid bool
	}{
		{name: "success", block: savedChart(), rows: []unitRow{row(true), row(0, 0, 0)}, valid: true},
		{name: "invalid-envelope", block: dialogue.Block{Type: "chart", Version: 1}},
		{name: "stopped", block: savedRows(), rows: []unitRow{{err: pgx.ErrNoRows}}},
		{name: "driver", block: savedRows(), rows: []unitRow{{err: errUnitDriver}}},
		{name: "non-next", block: savedRows(), rows: []unitRow{row(true), row(1, 0, 1)}},
		{name: "insert", block: savedRows(), rows: []unitRow{row(true), row(0, 0, 0)}, fail: 1},
		{name: "notify", block: savedRows(), rows: []unitRow{row(true), row(0, 0, 0)}, fail: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &unitTx{rows: tc.rows, affected: 1, failExec: tc.fail}
			err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.AppendBlock(ctx, unitTask(), 0, tc.block) })
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("invalid block result=%v", err)
			}
			if tc.name == "invalid-envelope" && tx.rowIndex != 0 {
				t.Fatal("invalid block reached database read")
			}
			if tc.name == "stopped" && !errors.Is(err, generation.ErrStopped) {
				t.Fatal(err)
			}
		})
	}
}

func finishRows(used int, day time.Time) []unitRow {
	return []unitRow{row(pgtype.Date{Time: day, Valid: true}, used), row(pgtype.Date{Time: day, Valid: true}, true)}
}

func TestUnitFinishSelectsAccountingEffectsAndHidesStorageFailures(t *testing.T) {
	repo := NewGenerations(unitDatabase())
	for _, tc := range []struct {
		name, code     string
		charged        bool
		daily          int
		wantUsedRefund bool
	}{
		{name: "charged", charged: true},
		{name: "free", wantUsedRefund: true},
		{name: "failed", code: "MODEL_ERROR", wantUsedRefund: true},
		{name: "off-topic", code: "OFF_TOPIC", daily: 4, wantUsedRefund: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := finishRows(1, storeNow)
			if tc.code == "OFF_TOPIC" {
				rows = append(rows, row(tc.daily))
			}
			tx := &unitTx{rows: rows, affected: 1, queryRows: []*unitRows{{}}}
			out := generation.Outcome{ErrorCode: tc.code, Charged: tc.charged}
			if tc.name == "charged" {
				from, to := storeNow.Add(-time.Hour), storeNow
				out.Blocks = []dialogue.Block{{Type: "text", Version: 1, Text: "saved"}, savedChart()}
				out.Chips = []json.RawMessage{json.RawMessage(`{"screen":"operations"}`)}
				out.Source = &dialogue.Source{CalculatedAt: storeNow, From: &from, To: &to}
			}
			terminal := generation.PrepareFinish(out, storeNow)
			result, err := withUnitTransaction(t, repo.db, tx, func(ctx context.Context) (generation.FinishResult, error) {
				return repo.Finish(ctx, unitTask(), terminal)
			})
			if err != nil || !result.Applied || result.OffTopicDailyCount != tc.daily {
				t.Fatalf("finish=%+v error=%v", result, err)
			}
			refunded := false
			for _, command := range tx.commands {
				if strings.Contains(command, "SET limit_used=limit_used-1") {
					refunded = true
				}
			}
			if refunded != tc.wantUsedRefund {
				t.Fatalf("refund effect=%v want%v", refunded, tc.wantUsedRefund)
			}
		})
	}
	for _, stage := range []string{"missing-user", "missing-answer", "user-error", "answer-error", "invalid-refund"} {
		t.Run(stage, func(t *testing.T) {
			rows := finishRows(1, storeNow)
			switch stage {
			case "missing-user":
				rows[0].err = pgx.ErrNoRows
			case "missing-answer":
				rows[1].err = pgx.ErrNoRows
			case "user-error":
				rows[0].err = errUnitDriver
			case "answer-error":
				rows[1].err = errUnitDriver
			case "invalid-refund":
				rows = finishRows(0, storeNow)
			}
			tx := &unitTx{rows: rows, affected: 1}
			result, err := withUnitTransaction(t, repo.db, tx, func(ctx context.Context) (generation.FinishResult, error) {
				return repo.Finish(ctx, unitTask(), generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow))
			})
			if result.Applied || strings.Contains(errorText(err), "PRIVATE") {
				t.Fatalf("invalid terminal applied=%+v error=%v", result, err)
			}
			if strings.HasPrefix(stage, "missing") {
				if err != nil || tx.execs != 0 {
					t.Fatalf("late terminal effects=%d error=%v", tx.execs, err)
				}
			} else if err == nil {
				t.Fatal("storage failure accepted")
			}
		})
	}
	for failure := 1; failure <= 4; failure++ {
		tx := &unitTx{rows: finishRows(1, storeNow), affected: 1, failExec: failure}
		result, err := withUnitTransaction(t, repo.db, tx, func(ctx context.Context) (generation.FinishResult, error) {
			return repo.Finish(ctx, unitTask(), generation.PrepareFinish(generation.Outcome{ErrorCode: "MODEL_ERROR"}, storeNow))
		})
		if err == nil || result.Applied || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("failed terminal result=%+v err=%v", result, err)
		}
	}
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestUnitAccountWritesTranslateErrorsAndPurgeAbsentUsers(t *testing.T) {
	repo := NewAccounts(unitDatabase())
	tx := &unitTx{}
	if err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.SetTimezone(ctx, dialogueUser, "UTC", 1) }); err != nil {
		t.Fatal(err)
	}
	if err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.Delete(ctx, dialogueUser) }); err != nil {
		t.Fatalf("absent user purge is not idempotent: %v", err)
	}
	tx = &unitTx{failExec: 1}
	if err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.SetTimezone(ctx, dialogueUser, "UTC", 1) }); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("timezone error=%v", err)
	}
	for failure := 1; failure <= 3; failure++ {
		tx := &unitTx{failExec: failure}
		if err := unitExecute(t, repo.db, tx, func(ctx context.Context) error { return repo.Delete(ctx, dialogueUser) }); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("purge error=%v", err)
		}
	}
}

func TestUnitFinishRefusesChangedPrefixAndInvalidChips(t *testing.T) {
	repo := NewGenerations(unitDatabase())
	for _, tc := range []struct {
		name   string
		out    generation.Outcome
		prefix *unitRows
		want   string
	}{
		{name: "regressed-text", out: generation.Outcome{Charged: true, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "different"}}}, prefix: &unitRows{values: [][]any{{0, "text", int32(1), "saved", false}}}, want: "regressed"},
		{name: "changed-kind", out: generation.Outcome{Charged: true, Blocks: []dialogue.Block{savedRows()}}, prefix: &unitRows{values: [][]any{{0, "text", int32(1), "saved", false}}}, want: "changed"},
		{name: "changed-chart", out: generation.Outcome{Charged: true, Blocks: []dialogue.Block{savedChart()}}, prefix: &unitRows{values: [][]any{{0, "chart", int32(1), "", false}}}, want: "changed"},
		{name: "invalid-chip", out: generation.Outcome{Charged: true, Chips: []json.RawMessage{json.RawMessage(`{`)}}, prefix: &unitRows{}, want: "invalid answer chips"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &unitTx{rows: finishRows(1, storeNow), affected: 1, queryRows: []*unitRows{tc.prefix}}
			result, err := withUnitTransaction(t, repo.db, tx, func(ctx context.Context) (generation.FinishResult, error) {
				return repo.Finish(ctx, unitTask(), generation.PrepareFinish(tc.out, storeNow))
			})
			if err == nil || result.Applied || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("changed outcome=%+v error=%v", result, err)
			}
			if tc.name != "invalid-chip" && tx.execs != 0 {
				t.Fatal("prefix refusal wrote terminal data")
			}
		})
	}
}

func TestUnitOffTopicFactReaderPreservesTimesAndSanitizesRows(t *testing.T) {
	q := NewQuestions(unitDatabase())
	tx := &unitTx{queryRows: []*unitRows{{values: [][]any{{storeNow}, {storeNow.Add(-time.Hour)}}}}}
	result, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) ([]time.Time, error) {
		return q.RecentOffTopic(ctx, dialogueUser, storeNow.Add(-24*time.Hour), 5)
	})
	if err != nil || len(result) != 2 || !result[0].Equal(storeNow) || !result[1].Equal(storeNow.Add(-time.Hour)) {
		t.Fatalf("facts=%v error=%v", result, err)
	}
	for _, rows := range []*unitRows{nil, {err: errUnitDriver}, {values: [][]any{{"invalid timestamp"}}}} {
		tx := &unitTx{}
		if rows != nil {
			tx.queryRows = []*unitRows{rows}
		}
		result, err := withUnitTransaction(t, q.db, tx, func(ctx context.Context) ([]time.Time, error) {
			return q.RecentOffTopic(ctx, dialogueUser, storeNow.Add(-24*time.Hour), 5)
		})
		if err == nil || len(result) != 0 || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("invalid facts=%v error=%v", result, err)
		}
	}
}
