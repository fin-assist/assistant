package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/migrations"
)

const dialogueUser = "10000000-0000-0000-0000-000000000001"
const dialogueForeign = "10000000-0000-0000-0000-000000000002"
const dialogueQuestion = "20000000-0000-0000-0000-000000000001"
const dialogueAnswer = "30000000-0000-0000-0000-000000000001"

func dialogueDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), Options{PrimaryDSN: migratetest.DSN(t), AppName: "dialogue-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		sql, err := migrations.FS.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Writer(t.Context()).Exec(t.Context(), string(sql)); err != nil {
			t.Fatal(err)
		}
	}

	return db
}
func dialogueExec(t *testing.T, db *DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Writer(t.Context()).Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func seedDialogue(t *testing.T, db *DB) {
	dialogueExec(t, db, `INSERT INTO user_state(user_id) VALUES ($1),($2)`, dialogueUser, dialogueForeign)
	dialogueExec(t, db, `INSERT INTO question(user_id,id,text,transfer_mode) VALUES ($1,$2,'question','without')`, dialogueUser, dialogueQuestion)
	dialogueExec(t, db, `INSERT INTO answer(user_id,id,question_id,charge_day,status,calculated_at,data_from,data_to,chips) VALUES ($1,$2,$3,'2026-10-23','complete','2026-10-23T12:00:00Z','2026-10-01T00:00:00Z','2026-11-01T00:00:00Z','[{"type":"operations"}]')`, dialogueUser, dialogueAnswer, dialogueQuestion)
	dialogueExec(t, db, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,0,'text',1,$3)`, dialogueUser, dialogueAnswer, strings.Repeat("x", 128*1024))
	dialogueExec(t, db, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,body) VALUES ($1,$2,1,'chart',2,'{"title":"chart","data":{"items":[]}}')`, dialogueUser, dialogueAnswer)
}

func TestIntegrationDialogueReadUsesSharedManagerAndOuterOptions(t *testing.T) {
	db := dialogueDB(t)
	repo := NewDialogue(db)
	assertOptions := func(ctx context.Context, isolation, readOnly string) error {
		return repo.read(ctx, func(tx pgx.Tx) error {
			var actualIsolation, actualReadOnly string
			if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only')`).Scan(&actualIsolation, &actualReadOnly); err != nil {
				return err
			}
			if actualIsolation != isolation || actualReadOnly != readOnly {
				t.Errorf("snapshot options: %s %s, want %s %s", actualIsolation, actualReadOnly, isolation, readOnly)
			}
			return nil
		})
	}
	if err := assertOptions(t.Context(), "repeatable read", "on"); err != nil {
		t.Fatal(err)
	}
	rolledBack := errors.New("rollback outer transaction")
	err := db.RunWith(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable}, func(ctx context.Context) error {
		if err := assertOptions(ctx, "serializable", "off"); err != nil {
			return err
		}
		if _, err := db.Writer(ctx).Exec(ctx, `INSERT INTO user_state(user_id) VALUES ($1)`, dialogueUser); err != nil {
			return err
		}
		if _, err := db.Writer(ctx).Exec(ctx, `INSERT INTO question(user_id,text,transfer_mode) VALUES ($1,'uncommitted question','without')`, dialogueUser); err != nil {
			return err
		}
		page, err := repo.List(ctx, dialogueUser, "", 10)
		if err != nil {
			return err
		}
		if len(page.Messages) != 1 {
			t.Error("nested read did not observe the outer transaction")
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	page, err := repo.List(t.Context(), dialogueUser, "", 10)
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("nested read committed the outer transaction: %+v %v", page, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.List(cancelled, dialogueUser, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}
func TestIntegrationDialogueReadsAndPages(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	r := NewDialogue(db)
	a, err := r.Snapshot(t.Context(), dialogueUser, dialogueAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != dialogueAnswer || a.TimeZone != "Europe/Moscow" || a.Status != "complete" || len(a.Blocks) != 2 || len(a.Blocks[0].Text) != 128*1024 || a.Blocks[1].Version != 2 || len(a.Blocks[1].Body) == 0 || len(a.Chips) != 1 || a.Source == nil || a.Source.TransferMode != "without" || a.Source.From == nil || a.Source.To == nil {
		t.Fatalf("incomplete answer snapshot: %+v", a.Source)
	}
	page, err := r.List(t.Context(), dialogueUser, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasOlder || len(page.Messages) != 1 || page.Messages[0].Answer == nil {
		t.Fatalf("latest page: %+v", page)
	}
	page, err = r.List(t.Context(), dialogueUser, dialogueAnswer, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.HasOlder || len(page.Messages) != 1 || page.Messages[0].ID != dialogueQuestion || page.Messages[0].Question != "question" {
		t.Fatalf("exclusive boundary: %+v", page)
	}
	page, err = r.List(t.Context(), dialogueUser, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.HasOlder || len(page.Messages) != 2 || page.Messages[0].ID != dialogueQuestion || page.Messages[1].ID != dialogueAnswer {
		t.Fatalf("order: %+v", page)
	}
	dialogueExec(t, db, `UPDATE user_state SET time_zone='Asia/Tokyo' WHERE user_id=$1`, dialogueUser)
	a, err = r.Snapshot(t.Context(), dialogueUser, dialogueAnswer)
	if err != nil || a.TimeZone != "Asia/Tokyo" {
		t.Fatalf("current timezone: %q %v", a.TimeZone, err)
	}
}
func TestIntegrationDialogueHidesForeignReplacedAndDeleted(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	r := NewDialogue(db)
	for _, id := range []string{dialogueAnswer, "30000000-0000-0000-0000-000000000099"} {
		if _, err := r.Snapshot(t.Context(), dialogueForeign, id); !errors.Is(err, dialogue.ErrNotFound) {
			t.Fatalf("foreign/missing: %v", err)
		}
	}
	p, err := r.List(t.Context(), dialogueForeign, "", 50)
	if err != nil || len(p.Messages) != 0 {
		t.Fatalf("foreign page: %+v %v", p, err)
	}
	dialogueExec(t, db, `UPDATE answer SET replaced_at=now() WHERE user_id=$1 AND id=$2`, dialogueUser, dialogueAnswer)
	if _, err := r.Snapshot(t.Context(), dialogueUser, dialogueAnswer); !errors.Is(err, dialogue.ErrNotFound) {
		t.Fatalf("replaced: %v", err)
	}
	p, err = r.List(t.Context(), dialogueUser, "", 50)
	if err != nil || len(p.Messages) != 1 || p.Messages[0].Answer != nil {
		t.Fatalf("replaced in page: %+v %v", p, err)
	}
	dialogueExec(t, db, `DELETE FROM user_state WHERE user_id=$1`, dialogueUser)
	p, err = r.List(t.Context(), dialogueUser, "", 50)
	if err != nil || len(p.Messages) != 0 || p.TimeZone != "Europe/Moscow" {
		t.Fatalf("deleted: %+v %v", p, err)
	}
}
func TestIntegrationDialogueFailureAndCancellation(t *testing.T) {
	db := dialogueDB(t)
	r := NewDialogue(db)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.List(ctx, dialogueUser, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := r.Snapshot(ctx, dialogueUser, dialogueAnswer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	for _, limit := range []int{0, 51} {
		if _, err := r.List(t.Context(), dialogueUser, "", limit); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
	if _, err := r.List(t.Context(), dialogueUser, "sensitive-invalid-cursor", 1); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("raw error leaked: %v", err)
	}
	dialogueExec(t, db, `DROP TABLE answer_block`)
	if _, err := r.Snapshot(t.Context(), dialogueUser, dialogueAnswer); err == nil || errors.Is(err, dialogue.ErrNotFound) {
		t.Fatalf("storage failure: %v", err)
	}
}

func TestIntegrationDialogueSnapshotTransactionIsCoherentAndReadOnly(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	r := NewDialogue(db)
	err := r.read(t.Context(), func(tx pgx.Tx) error {
		var isolation, readOnly string
		if err := tx.QueryRow(t.Context(), `SELECT current_setting('transaction_isolation'),current_setting('transaction_read_only')`).Scan(&isolation, &readOnly); err != nil {
			return err
		}
		if isolation != "repeatable read" || readOnly != "on" {
			t.Fatalf("transaction %q read-only %q", isolation, readOnly)
		}
		zone, err := dialogueZone(t.Context(), tx, dialogueUser)
		if err != nil {
			return err
		}
		// A writer commits between reading the profile and reading the answer's blocks.
		dialogueExec(t, db, `UPDATE user_state SET time_zone='Asia/Tokyo' WHERE user_id=$1`, dialogueUser)
		dialogueExec(t, db, `UPDATE answer_block SET markdown='new content' WHERE user_id=$1 AND answer_id=$2 AND position=0`, dialogueUser, dialogueAnswer)
		answers, err := readAnswers(t.Context(), tx, dialogueUser, []string{dialogueAnswer}, zone)
		if err != nil {
			return err
		}
		a := answers[dialogueAnswer]
		if a.TimeZone != "Europe/Moscow" || len(a.Blocks[0].Text) != 128*1024 {
			t.Fatal("mixed committed versions inside snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.Snapshot(t.Context(), dialogueUser, strings.ToUpper(dialogueAnswer))
	if err != nil || a.TimeZone != "Asia/Tokyo" || a.Blocks[0].Text != "new content" {
		t.Fatalf("next snapshot did not refresh: %v", err)
	}
}

func TestIntegrationDialogueFailedAndGeneratingAnswerFields(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	r := NewDialogue(db)
	dialogueExec(t, db, `UPDATE answer SET status='failed',error_code='MODEL_ERROR',retryable=true,charged=false,calculated_at=NULL,data_from=NULL,data_to=NULL,chips='[]' WHERE user_id=$1 AND id=$2`, dialogueUser, dialogueAnswer)
	dialogueExec(t, db, `DELETE FROM answer_block WHERE user_id=$1 AND answer_id=$2`, dialogueUser, dialogueAnswer)
	a, err := r.Snapshot(t.Context(), dialogueUser, dialogueAnswer)
	if err != nil || a.ErrorCode != "MODEL_ERROR" || a.Retryable == nil || !*a.Retryable || a.Charged || a.Source != nil || len(a.Chips) != 0 || len(a.Blocks) != 0 {
		t.Fatalf("failed snapshot: %+v %v", a, err)
	}
	dialogueExec(t, db, `UPDATE answer SET status='generating',error_code=NULL,retryable=NULL,charged=true WHERE user_id=$1 AND id=$2`, dialogueUser, dialogueAnswer)
	a, err = r.Snapshot(t.Context(), dialogueUser, dialogueAnswer)
	if err != nil || a.Status != "generating" || a.Retryable != nil || a.ErrorCode != "" || !a.Charged {
		t.Fatalf("generating snapshot: %+v %v", a, err)
	}
	// Pagination uses UUID, including when creation timestamps are equal.
	dialogueExec(t, db, `UPDATE question SET created_at='2026-10-23T00:00:00Z' WHERE user_id=$1`, dialogueUser)
	dialogueExec(t, db, `UPDATE answer SET created_at='2026-10-23T00:00:00Z' WHERE user_id=$1`, dialogueUser)
	p, err := r.List(t.Context(), dialogueUser, dialogueAnswer, 1)
	if err != nil || len(p.Messages) != 1 || p.Messages[0].ID != dialogueQuestion {
		t.Fatalf("equal timestamp page: %+v %v", p, err)
	}
}

func TestIntegrationDialogueRetryStaysWithQuestionAcrossPages(t *testing.T) {
	db := dialogueDB(t)
	seedDialogue(t, db)
	const q2 = "20000000-0000-0000-0000-000000000002"
	const a2 = "30000000-0000-0000-0000-000000000002"
	const retry = "40000000-0000-0000-0000-000000000001"
	dialogueExec(t, db, `INSERT INTO question(user_id,id,text,transfer_mode) VALUES ($1,$2,'second','with')`, dialogueUser, q2)
	dialogueExec(t, db, `INSERT INTO answer(user_id,id,question_id,charge_day,status) VALUES ($1,$2,$3,'2026-10-23','complete')`, dialogueUser, a2, q2)
	dialogueExec(t, db, `UPDATE answer SET replaced_at=now() WHERE user_id=$1 AND id=$2`, dialogueUser, dialogueAnswer)
	dialogueExec(t, db, `INSERT INTO answer(user_id,id,question_id,charge_day,status) VALUES ($1,$2,$3,'2026-10-23','complete')`, dialogueUser, retry, dialogueQuestion)
	r := NewDialogue(db)
	for _, tc := range []struct {
		before string
		limit  int
		want   []string
		older  bool
	}{
		{"", 50, []string{dialogueQuestion, retry, q2, a2}, false},
		{"", 2, []string{q2, a2}, true},
		{q2, 2, []string{dialogueQuestion, retry}, false},
		{retry, 1, []string{dialogueQuestion}, false},
		{a2, 2, []string{retry, q2}, true},
		{dialogueAnswer, 1, []string{dialogueQuestion}, false},
	} {
		page, err := r.List(t.Context(), dialogueUser, tc.before, tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		if page.HasOlder != tc.older || len(page.Messages) != len(tc.want) {
			t.Fatalf("before %q: %+v", tc.before, page)
		}
		for i, id := range tc.want {
			if page.Messages[i].ID != id {
				t.Fatalf("before %q message %d got %q want %q", tc.before, i, page.Messages[i].ID, id)
			}
		}
	}
}
