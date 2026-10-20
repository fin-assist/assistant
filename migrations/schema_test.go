package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate"
	"github.com/fin-assist/assistant/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/assistant/migrations"
)

// The tests below pin the keys of the schema (docs/schema.md) with real inserts, and run
// the queries docs/schema.md proposes for the expected calls, so that a change of the schema
// that breaks one of them fails here. The repositories of the service will carry the same SQL.

const (
	userA = "0199f0a0-0000-7000-8000-00000000000a"
	userB = "0199f0a0-0000-7000-8000-00000000000b"
	// Absent from every table: an unknown or foreign ID.
	unknownID = "0199f0a0-0000-7000-8000-0000000000ff"
	dailyMax  = 5
)

// incrementLimit charges one question to the user's Moscow day of $2 (the injected clock),
// creating the user's row on the first call. No row back: the limit of that day is spent.
// It is the first statement of AskQuestion and RetryAnswer, so it also takes the user's row
// lock that serialises the user's writing transactions (docs/schema.md, «Блокировка»).
const incrementLimit = `
INSERT INTO user_state AS s (user_id, limit_day, limit_used)
VALUES ($1, ($2::timestamptz AT TIME ZONE 'Europe/Moscow')::date, 1)
ON CONFLICT (user_id) DO UPDATE
SET limit_used = CASE WHEN s.limit_day = EXCLUDED.limit_day THEN s.limit_used + 1 ELSE 1 END,
    limit_day  = EXCLUDED.limit_day
WHERE s.limit_day IS DISTINCT FROM EXCLUDED.limit_day OR s.limit_used < $3
RETURNING s.limit_day, s.limit_used`

const insertQuestion = `
INSERT INTO question (user_id, text, transfer_mode) VALUES ($1, $2, $3) RETURNING id`

const insertAnswer = `
INSERT INTO answer (user_id, question_id, charge_day) VALUES ($1, $2, $3) RETURNING id`

// lockUser takes the user's row lock first in a writing transaction that does not charge
// the limit (finishing an answer, the timeout job).
const lockUser = `SELECT 1 FROM user_state WHERE user_id = $1 FOR UPDATE`

// failAnswer ends a generating answer with an error, clears what a failed answer must not
// keep (blocks, chips, source) and returns the attempt to the day it was charged to. A second
// call finds no generating answer and changes nothing, so a refund happens once.
const failAnswer = `
WITH failed AS (
    UPDATE answer
    SET status = 'failed', charged = false, finished_at = now(),
        error_code = $3, error_text = $4, retryable = $5,
        calculated_at = NULL, data_from = NULL, data_to = NULL, chips = '[]'
    WHERE user_id = $1 AND id = $2 AND status = 'generating'
    RETURNING user_id, id, charge_day
), blocks AS (
    DELETE FROM answer_block b USING failed f
    WHERE b.user_id = f.user_id AND b.answer_id = f.id
), refund AS (
    UPDATE user_state s SET limit_used = s.limit_used - 1
    FROM failed f
    WHERE s.user_id = f.user_id AND s.limit_day = f.charge_day
)
SELECT count(*) FROM failed`

// completeAnswer finishes a generating answer; charged = false refunds the attempt when the
// functions reported missing data (REST 7.2 «done»).
const completeAnswer = `
WITH done AS (
    UPDATE answer SET status = 'complete', charged = $3, finished_at = now()
    WHERE user_id = $1 AND id = $2 AND status = 'generating'
    RETURNING user_id, charge_day, charged
), refund AS (
    UPDATE user_state s SET limit_used = s.limit_used - 1
    FROM done d
    WHERE s.user_id = d.user_id AND s.limit_day = d.charge_day AND NOT d.charged
)
SELECT count(*) FROM done`

// replaceForRetry marks a failed answer that allows a retry as replaced and returns its
// question; no row: RETRY_NOT_ALLOWED (or NOT_FOUND when the answer does not exist).
const replaceForRetry = `
UPDATE answer SET replaced_at = now()
WHERE user_id = $1 AND id = $2 AND status = 'failed' AND retryable AND replaced_at IS NULL
RETURNING question_id`

// appendChunk appends a text fragment to a text block of a generating answer and notifies
// the streams of this answer; the notification carries the event and the index only.
const appendChunk = `
WITH b AS (
    UPDATE answer_block b SET text = b.text || $4
    FROM answer a
    WHERE b.user_id = $1 AND b.answer_id = $2 AND b.idx = $3 AND b.type = 'text'
      AND a.user_id = b.user_id AND a.id = b.answer_id AND a.status = 'generating'
    RETURNING b.idx
)
SELECT pg_notify('answer_' || replace($2::text, '-', ''), 'chunk:' || idx) FROM b`

// insertBlock adds the next block of a generating answer; FOR SHARE waits for a concurrent
// failAnswer, so no block is added to an answer that has just failed.
const insertBlock = `
INSERT INTO answer_block (user_id, answer_id, idx, type, v, text, body)
SELECT a.user_id, a.id, $3, $4, $5, $6, $7
FROM answer a
WHERE a.user_id = $1 AND a.id = $2 AND a.status = 'generating'
FOR SHARE`

// readTextTail is what a stream reads after a "chunk" notification: the text after the
// characters it has already sent, and the new length.
const readTextTail = `
SELECT substr(text, $4 + 1), char_length(text)
FROM answer_block WHERE user_id = $1 AND answer_id = $2 AND idx = $3`

// snapshotAnswer is the first message of WatchAnswer. A replaced or another user's answer is
// not found.
const snapshotAnswer = `
SELECT a.status, a.currency, a.chips, a.calculated_at, q.transfer_mode
FROM answer a JOIN question q ON q.user_id = a.user_id AND q.id = a.question_id
WHERE a.user_id = $1 AND a.id = $2 AND a.replaced_at IS NULL`

const snapshotBlocks = `
SELECT idx, type, v, text, body FROM answer_block
WHERE user_id = $1 AND answer_id = $2 ORDER BY idx`

// historyPage returns questions older than the cursor (newest first) with their active
// answer; the service turns them into messages old-first and cuts to the requested limit.
const historyPage = `
SELECT q.id, q.text, a.id, a.status
FROM question q
LEFT JOIN answer a
       ON a.user_id = q.user_id AND a.question_id = q.id AND a.replaced_at IS NULL
WHERE q.user_id = $1 AND q.id < $2
ORDER BY q.id DESC
LIMIT $3`

// setTimezone applies usersettings.v1.SetUserTimezone: only a newer settings_version wins.
const setTimezone = `
INSERT INTO user_state AS s (user_id, time_zone, settings_version) VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
SET time_zone = EXCLUDED.time_zone, settings_version = EXCLUDED.settings_version
WHERE s.settings_version IS NULL OR s.settings_version < EXCLUDED.settings_version`

// staleGenerations is the timeout job: generations started before $1 (now − 3 min).
const staleGenerations = `
SELECT user_id, id FROM answer WHERE status = 'generating' AND created_at < $1 ORDER BY created_at`

// lockForPurge is the first statement of PurgeUserData after the tombstone: it takes the
// user's row lock, creating the row if needed, so it waits for an AskQuestion that has just
// inserted the user's first row and is not committed yet (docs/schema.md, «Удаление»).
const lockForPurge = `
INSERT INTO user_state AS s (user_id) VALUES ($1)
ON CONFLICT (user_id) DO UPDATE SET user_id = s.user_id`

// purgeUser is the data part of PurgeUserData: the cascade from the user's row.
const purgeUser = `DELETE FROM user_state WHERE user_id = $1`

func setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := migratetest.DSN(t)
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Up(context.Background(), cfg, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func at(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}

// ask runs the AskQuestion transaction: the limit, the question and its answer.
func ask(t *testing.T, db *pgxpool.Pool, user string, now time.Time) (questionID, answerID string) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var day time.Time
		var used int
		if err := tx.QueryRow(ctx, incrementLimit, user, now, dailyMax).Scan(&day, &used); err != nil {
			return fmt.Errorf("limit: %w", err)
		}
		if err := tx.QueryRow(ctx, insertQuestion, user, "Сколько я трачу на Самокат?", "with").Scan(&questionID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, insertAnswer, user, questionID, day).Scan(&answerID)
	})
	if err != nil {
		t.Fatal(err)
	}
	return questionID, answerID
}

// fail runs failAnswer under the user's lock and returns how many answers it failed.
func fail(t *testing.T, db *pgxpool.Pool, user, answerID string, retryable bool) int {
	t.Helper()
	ctx := context.Background()
	var n int
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, lockUser, user); err != nil {
			return err
		}
		code := "CANNOT_ANSWER"
		if retryable {
			code = "MODEL_ERROR"
		}
		return tx.QueryRow(ctx, failAnswer, user, answerID, code, "Не получилось ответить. Вопрос не потрачен", retryable).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func used(t *testing.T, db *pgxpool.Pool, user string) (string, int) {
	t.Helper()
	var day time.Time
	var n int
	if err := db.QueryRow(context.Background(), `SELECT limit_day, limit_used FROM user_state WHERE user_id = $1`, user).Scan(&day, &n); err != nil {
		t.Fatal(err)
	}
	return day.Format(time.DateOnly), n
}

// constraintOf returns the name of the violated constraint, or fails the test.
func constraintOf(t *testing.T, err error) string {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error %v, want a constraint violation", err)
	}
	return pg.ConstraintName
}

func TestLimitIsChargedAtomicallyUpToDailyMax(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	now := at("2026-10-19T12:00:00+03:00")

	const callers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	charged, refused := 0, 0
	for range callers {
		wg.Go(func() {
			var day time.Time
			var n int
			err := db.QueryRow(ctx, incrementLimit, userA, now, dailyMax).Scan(&day, &n)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				refused++
			case err != nil:
				t.Error(err)
			default:
				charged++
			}
		})
	}
	wg.Wait()
	if charged != dailyMax || refused != callers-dailyMax {
		t.Fatalf("charged %d, refused %d; want %d and %d", charged, refused, dailyMax, callers-dailyMax)
	}
	if day, n := used(t, db, userA); day != "2026-10-19" || n != dailyMax {
		t.Fatalf("user_state %s/%d, want 2026-10-19/%d", day, n, dailyMax)
	}
}

func TestLimitResetsAtMoscowMidnight(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	var day time.Time
	var n int
	for range dailyMax {
		if err := db.QueryRow(ctx, incrementLimit, userA, at("2026-10-19T20:59:59Z"), dailyMax).Scan(&day, &n); err != nil {
			t.Fatal(err)
		}
	}
	// 20:59:59Z is 23:59:59 in Moscow: still the same day, the limit is spent.
	err := db.QueryRow(ctx, incrementLimit, userA, at("2026-10-19T20:59:59.999Z"), dailyMax).Scan(&day, &n)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("error %v, want no row (limit spent)", err)
	}
	// 21:00:00Z is 00:00 in Moscow: a new day, the counter starts again.
	if err := db.QueryRow(ctx, incrementLimit, userA, at("2026-10-19T21:00:00Z"), dailyMax).Scan(&day, &n); err != nil {
		t.Fatal(err)
	}
	if day.Format(time.DateOnly) != "2026-10-20" || n != 1 {
		t.Fatalf("day %s, used %d; want 2026-10-20 and 1", day.Format(time.DateOnly), n)
	}
}

func TestFailedAnswerRefundsOnceAndOnlyItsDay(t *testing.T) {
	db := setup(t)
	_, a1 := ask(t, db, userA, at("2026-10-19T23:59:00+03:00"))
	_, a2 := ask(t, db, userA, at("2026-10-19T23:59:30+03:00"))
	if _, n := used(t, db, userA); n != 2 {
		t.Fatalf("used %d, want 2", n)
	}

	if got := fail(t, db, userA, a1, true); got != 1 {
		t.Fatalf("failed %d answers, want 1", got)
	}
	if got := fail(t, db, userA, a1, true); got != 0 {
		t.Fatalf("second fail changed %d answers, want 0: a refund happens once", got)
	}
	if _, n := used(t, db, userA); n != 1 {
		t.Fatalf("used %d after one refund, want 1", n)
	}

	// After midnight the user asks again; the refund of yesterday's attempt does not touch
	// today's counter.
	ask(t, db, userA, at("2026-10-20T00:00:10+03:00"))
	if got := fail(t, db, userA, a2, false); got != 1 {
		t.Fatalf("failed %d answers, want 1", got)
	}
	if day, n := used(t, db, userA); day != "2026-10-20" || n != 1 {
		t.Fatalf("user_state %s/%d, want 2026-10-20/1", day, n)
	}
}

func TestCompleteAnswerRefundsOnlyWhenNotCharged(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	now := at("2026-10-19T12:00:00+03:00")
	_, charged := ask(t, db, userA, now)
	_, free := ask(t, db, userA, now)

	for _, c := range []struct {
		id      string
		charged bool
	}{{charged, true}, {free, false}} {
		var n int
		if err := db.QueryRow(ctx, completeAnswer, userA, c.id, c.charged).Scan(&n); err != nil || n != 1 {
			t.Fatalf("complete %s: %d, %v", c.id, n, err)
		}
	}
	if _, n := used(t, db, userA); n != 1 {
		t.Fatalf("used %d, want 1: only the answer with missing data is refunded", n)
	}
	var n int
	if err := db.QueryRow(ctx, completeAnswer, userA, free, false).Scan(&n); err != nil || n != 0 {
		t.Fatalf("second complete: %d, %v; want 0: a finished answer does not change", n, err)
	}
	if _, n := used(t, db, userA); n != 1 {
		t.Fatalf("used %d after a repeated complete, want 1", n)
	}
}

func TestRetryReplacesFailedAnswerAndKeepsOneActive(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	now := at("2026-10-19T12:00:00+03:00")
	q, failed := ask(t, db, userA, now)
	fail(t, db, userA, failed, true)

	retry := func(ctx context.Context, tx pgx.Tx) (string, error) {
		var day time.Time
		var n int
		if err := tx.QueryRow(ctx, incrementLimit, userA, now, dailyMax).Scan(&day, &n); err != nil {
			return "", err
		}
		var question string
		if err := tx.QueryRow(ctx, replaceForRetry, userA, failed).Scan(&question); err != nil {
			return "", err
		}
		var id string
		err := tx.QueryRow(ctx, insertAnswer, userA, question, day).Scan(&id)
		return id, err
	}

	// Two retries of the same answer at once: one replaces it, the other finds nothing to
	// replace (RETRY_NOT_ALLOWED) and charges nothing.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	newIDs := make(chan string, 2)
	for range 2 {
		wg.Go(func() {
			err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
				id, err := retry(ctx, tx)
				if err == nil {
					newIDs <- id
				}
				return err
			})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	close(newIDs)
	ok, notAllowed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pgx.ErrNoRows):
			notAllowed++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || notAllowed != 1 {
		t.Fatalf("retries: %d succeeded, %d refused; want 1 and 1", ok, notAllowed)
	}
	if _, n := used(t, db, userA); n != 1 {
		t.Fatalf("used %d, want 1: the refused retry rolled its charge back", n)
	}
	newID := <-newIDs

	// The replaced answer is gone for its stream and for the history.
	var status string
	err := db.QueryRow(ctx, snapshotAnswer, userA, failed).Scan(&status, new(string), new([]byte), new(*time.Time), new(string))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("snapshot of the replaced answer: %v, want no row (NOT_FOUND)", err)
	}
	var answerID, answerStatus *string
	if err := db.QueryRow(ctx, historyPage, userA, unknownMaxID, 10).Scan(new(string), new(string), &answerID, &answerStatus); err != nil {
		t.Fatal(err)
	}
	if answerID == nil || *answerID != newID || *answerStatus != "generating" {
		t.Fatalf("history shows answer %v, want the new one %s", answerID, newID)
	}

	// The schema itself refuses a second active answer of the question.
	_, err = db.Exec(ctx, insertAnswer, userA, q, now)
	if got := constraintOf(t, err); got != "answer_active_key" {
		t.Fatalf("second active answer: constraint %q, want answer_active_key", got)
	}
}

// The largest UUIDv7: the cursor of the first history page.
const unknownMaxID = "ffffffff-ffff-7fff-bfff-ffffffffffff"

func TestOnlyRetryableFailedAnswerCanBeReplaced(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	now := at("2026-10-19T12:00:00+03:00")
	_, generating := ask(t, db, userA, now)
	_, notRetryable := ask(t, db, userA, now)
	fail(t, db, userA, notRetryable, false)
	_, complete := ask(t, db, userA, now)
	if _, err := db.Exec(ctx, completeAnswer, userA, complete, true); err != nil {
		t.Fatal(err)
	}

	for name, id := range map[string]string{"generating": generating, "not retryable": notRetryable, "complete": complete, "unknown": unknownID} {
		if err := db.QueryRow(ctx, replaceForRetry, userA, id).Scan(new(string)); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: %v, want no row (RETRY_NOT_ALLOWED / NOT_FOUND)", name, err)
		}
	}
	// Another user's failed answer is not found by its ID.
	_, foreign := ask(t, db, userB, now)
	fail(t, db, userB, foreign, true)
	if err := db.QueryRow(ctx, replaceForRetry, userA, foreign).Scan(new(string)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another user's answer: %v, want no row", err)
	}
}

// The schema keeps keys, foreign keys and NOT NULL only (the owner's decision of 19.10.2026);
// value and state rules are the service code's (docs/schema.md, «Инварианты в коде сервиса»).
func TestKeysRejectInvalidRows(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	q, a := ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))
	if _, err := db.Exec(ctx, insertBlock, userA, a, 0, "text", 1, "", nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, sql  string
		args       []any
		constraint string
	}{
		{"second row of a user", `INSERT INTO user_state (user_id) VALUES ($1)`, []any{userA}, "user_state_pkey"},
		{"question id taken", `INSERT INTO question (user_id, id, text, transfer_mode) VALUES ($1, $2, 'q', 'with')`, []any{userA, q}, "question_pkey"},
		{"question of a user without a row", `INSERT INTO question (user_id, text, transfer_mode) VALUES ($1, 'q', 'with')`, []any{userB}, "question_user_fkey"},
		{"answer of another user's question", `INSERT INTO answer (user_id, question_id, charge_day) VALUES ($1, $2, '2026-10-19')`, []any{userB, q}, "answer_question_fkey"},
		{"second active answer", `INSERT INTO answer (user_id, question_id, charge_day) VALUES ($1, $2, '2026-10-19')`, []any{userA, q}, "answer_active_key"},
		{"block index taken", `INSERT INTO answer_block (user_id, answer_id, idx, type, v, text) VALUES ($1, $2, 0, 'text', 1, '')`, []any{userA, a}, "answer_block_pkey"},
		{"block of another user's answer", `INSERT INTO answer_block (user_id, answer_id, idx, type, v, text) VALUES ($1, $2, 1, 'text', 1, '')`, []any{userB, a}, "answer_block_answer_fkey"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := db.Exec(ctx, c.sql, c.args...)
			if got := constraintOf(t, err); got != c.constraint {
				t.Fatalf("constraint %q, want %q (error %v)", got, c.constraint, err)
			}
		})
	}

	// A replaced answer frees the question for a new active one, but its id stays taken. The
	// transaction rolls back, so the answer stays active for the checks below.
	t.Run("answer id taken", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE answer SET replaced_at = now() WHERE user_id = $1 AND id = $2`, userA, a); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO answer (user_id, id, question_id, charge_day) VALUES ($1, $2, $3, '2026-10-19')`, userA, a, q)
			return err
		})
		if got := constraintOf(t, err); got != "answer_pkey" {
			t.Fatalf("constraint %q, want answer_pkey", got)
		}
	})

	notNull := []struct{ name, sql, column string }{
		{"question text", `INSERT INTO question (user_id, text, transfer_mode) VALUES ($1, NULL, 'with')`, "text"},
		{"question transfer mode", `INSERT INTO question (user_id, text, transfer_mode) VALUES ($1, 'q', NULL)`, "transfer_mode"},
		{"answer charge day", `INSERT INTO answer (user_id, question_id, charge_day) SELECT $1, id, NULL FROM question WHERE user_id = $1 LIMIT 1`, "charge_day"},
		{"block type", `INSERT INTO answer_block (user_id, answer_id, idx, type, v) SELECT $1, id, 5, NULL, 1 FROM answer WHERE user_id = $1 LIMIT 1`, "type"},
	}
	for _, c := range notNull {
		t.Run("NOT NULL "+c.name, func(t *testing.T) {
			_, err := db.Exec(ctx, c.sql, userA)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "23502" || pg.ColumnName != c.column {
				t.Fatalf("error %v, want a NOT NULL violation of %s", err, c.column)
			}
		})
	}

	// Defaults of a new answer: generating, charged, RUB, no chips, UUIDv7 id.
	var status, currency string
	var charged bool
	var chips []byte
	var version int
	err := db.QueryRow(ctx, `SELECT status, charged, currency, chips, uuid_extract_version(id) FROM answer WHERE user_id = $1 AND id = $2`, userA, a).
		Scan(&status, &charged, &currency, &chips, &version)
	if err != nil || status != "generating" || !charged || currency != "RUB" || string(chips) != "[]" || version != 7 {
		t.Fatalf("defaults %s %v %s %s v%d, %v", status, charged, currency, chips, version, err)
	}
}

func TestStreamReadsChunksAfterNotification(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	_, a := ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))

	listener, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Release()
	channel := "answer_" + strings.ReplaceAll(a, "-", "")
	if _, err := listener.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(ctx, insertBlock, userA, a, 0, "text", 1, "За апрель — сентябрь", nil); err != nil {
		t.Fatal(err)
	}
	// A rolled-back write notifies nobody.
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, appendChunk, userA, a, 0, " lost"); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil || err.Error() != "rollback" {
		t.Fatalf("error %v, want rollback", err)
	}
	// A chunk of two-byte letters: the stream counts characters, not bytes.
	if _, err := db.Exec(ctx, appendChunk, userA, a, 0, " на Самокат ушло **7 200 ₽**"); err != nil {
		t.Fatal(err)
	}

	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := listener.Conn().WaitForNotification(wait)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != channel || n.Payload != "chunk:0" {
		t.Fatalf("notification %s %q, want %s chunk:0", n.Channel, n.Payload, channel)
	}

	var tail string
	var length int
	sent := len([]rune("За апрель — сентябрь"))
	if err := db.QueryRow(ctx, readTextTail, userA, a, 0, sent).Scan(&tail, &length); err != nil {
		t.Fatal(err)
	}
	if tail != " на Самокат ушло **7 200 ₽**" || length != sent+len([]rune(tail)) {
		t.Fatalf("tail %q, length %d", tail, length)
	}

	// Once the answer failed, a late chunk changes nothing, its blocks are gone, and no
	// block can be added.
	fail(t, db, userA, a, true)
	tag, err := db.Exec(ctx, appendChunk, userA, a, 0, " late")
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("late chunk returned %d rows, want 0", tag.RowsAffected())
	}
	tag, err = db.Exec(ctx, insertBlock, userA, a, 1, "rows", 1, nil, `{"data": {"rows": []}}`)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("block added to a failed answer: %d rows", tag.RowsAffected())
	}
	rows, err := db.Query(ctx, snapshotBlocks, userA, a)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("failed answer keeps %d blocks, want 0", len(blocks))
	}
}

func TestSnapshotHasBlocksInOrderAndHidesForeignAnswers(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	_, a := ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))
	if _, err := db.Exec(ctx, insertBlock, userA, a, 0, "text", 1, "Текст", nil); err != nil {
		t.Fatal(err)
	}
	chart := `{"title": "Самокат по месяцам", "alt_text": "…", "data": {"kind": "bar", "value_kind": "amount", "points": [{"value": 288000}]}}`
	if _, err := db.Exec(ctx, insertBlock, userA, a, 1, "chart", 1, nil, chart); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE answer SET calculated_at = $3, data_from = $4, data_to = $5, chips = chips || $6::jsonb WHERE user_id = $1 AND id = $2`,
		userA, a, at("2026-09-25T19:11:00+03:00"), at("2026-04-01T00:00:00+03:00"), at("2026-10-01T00:00:00+03:00"),
		`[{"label": "Самокат · 9 операций", "screen": "operations"}]`); err != nil {
		t.Fatal(err)
	}

	var status, currency, mode string
	var chips []byte
	var calculated *time.Time
	if err := db.QueryRow(ctx, snapshotAnswer, userA, a).Scan(&status, &currency, &chips, &calculated, &mode); err != nil {
		t.Fatal(err)
	}
	if status != "generating" || currency != "RUB" || mode != "with" || calculated == nil || !strings.Contains(string(chips), "Самокат") {
		t.Fatalf("snapshot %s %s %s %v %s", status, currency, mode, calculated, chips)
	}
	rows, err := db.Query(ctx, snapshotBlocks, userA, a)
	if err != nil {
		t.Fatal(err)
	}
	type block struct {
		Idx  int16
		Type string
		V    int16
		Text *string
		Body map[string]any
	}
	blocks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[block])
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "chart" || blocks[1].Body["title"] != "Самокат по месяцам" {
		t.Fatalf("blocks %+v", blocks)
	}
	// An amount in data keeps its exact integer value (jsonb numeric, not double).
	var amount int64
	if err := db.QueryRow(ctx, `SELECT (body #>> '{data,points,0,value}')::bigint FROM answer_block WHERE user_id = $1 AND answer_id = $2 AND idx = 1`, userA, a).Scan(&amount); err != nil || amount != 288000 {
		t.Fatalf("amount %d, %v", amount, err)
	}

	// Another user asking for this answer gets nothing (NOT_FOUND), not an error that
	// confirms it exists.
	if err := db.QueryRow(ctx, snapshotAnswer, userB, a).Scan(&status, &currency, &chips, &calculated, &mode); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign snapshot: %v, want no row", err)
	}
	rows, err = db.Query(ctx, snapshotBlocks, userB, a)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := pgx.CollectRows(rows, pgx.RowToMap)
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign blocks: %d, %v", len(foreign), err)
	}
}

func TestHistoryPagesByQuestionNewestFirst(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	var questions []string
	for i := range 5 {
		q, _ := ask(t, db, userA, at("2026-10-19T12:00:00+03:00").Add(time.Duration(i)*time.Minute))
		questions = append(questions, q)
	}
	ask(t, db, userB, at("2026-10-19T12:00:00+03:00"))

	page := func(cursor string, limit int) []string {
		rows, err := db.Query(ctx, historyPage, userA, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
			var q string
			err := r.Scan(&q, new(string), new(*string), new(*string))
			return q, err
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}

	first := page(unknownMaxID, 2)
	if len(first) != 2 || first[0] != questions[4] || first[1] != questions[3] {
		t.Fatalf("first page %v, want the two newest of %v", first, questions)
	}
	second := page(first[1], 10)
	if len(second) != 3 || second[0] != questions[2] || second[2] != questions[0] {
		t.Fatalf("second page %v", second)
	}
	if rest := page(questions[0], 10); len(rest) != 0 {
		t.Fatalf("older than the first question: %v", rest)
	}
}

func TestTimezoneAppliesOnlyNewerVersion(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	steps := []struct {
		zone    string
		version int64
		want    string
	}{
		{"Asia/Yekaterinburg", 2, "Asia/Yekaterinburg"},
		{"Europe/Kaliningrad", 1, "Asia/Yekaterinburg"}, // late delivery of an older version
		{"Europe/Kaliningrad", 2, "Asia/Yekaterinburg"}, // repeated delivery
		{"Asia/Vladivostok", 3, "Asia/Vladivostok"},
	}
	for _, s := range steps {
		if _, err := db.Exec(ctx, setTimezone, userA, s.zone, s.version); err != nil {
			t.Fatal(err)
		}
		var zone string
		if err := db.QueryRow(ctx, `SELECT time_zone FROM user_state WHERE user_id = $1`, userA).Scan(&zone); err != nil {
			t.Fatal(err)
		}
		if zone != s.want {
			t.Fatalf("after %s v%d: %s, want %s", s.zone, s.version, zone, s.want)
		}
	}
	// A user whose time zone came before any question has an unused limit, and the first
	// question then charges it from 1.
	var fresh bool
	if err := db.QueryRow(ctx, `SELECT limit_day IS NULL AND limit_used = 0 FROM user_state WHERE user_id = $1`, userA).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("limit of a user without questions: fresh=%v, %v", fresh, err)
	}
	ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))
	var zone string
	if err := db.QueryRow(ctx, `SELECT time_zone FROM user_state WHERE user_id = $1`, userA).Scan(&zone); err != nil || zone != "Asia/Vladivostok" {
		t.Fatalf("time zone after a question: %s, %v", zone, err)
	}
	if day, n := used(t, db, userA); day != "2026-10-19" || n != 1 {
		t.Fatalf("limit %s/%d, want 2026-10-19/1", day, n)
	}
}

func TestTimeoutJobFindsOnlyStaleGenerations(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	_, stale := ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))
	_, fresh := ask(t, db, userB, at("2026-10-19T12:00:00+03:00"))
	_, done := ask(t, db, userA, at("2026-10-19T12:00:00+03:00"))
	if _, err := db.Exec(ctx, completeAnswer, userA, done, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE answer SET created_at = now() - interval '4 minutes' WHERE id = ANY($1)`, []string{stale, done}); err != nil {
		t.Fatal(err)
	}

	var cutoff time.Time
	if err := db.QueryRow(ctx, `SELECT now() - interval '3 minutes'`).Scan(&cutoff); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, staleGenerations, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var user, id string
		err := r.Scan(&user, &id)
		return id, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != stale {
		t.Fatalf("stale generations %v, want only %s (not %s, not %s)", ids, stale, fresh, done)
	}
}

func TestPurgeRemovesOnlyThatUsersData(t *testing.T) {
	db := setup(t)
	ctx := context.Background()
	now := at("2026-10-19T12:00:00+03:00")
	for _, user := range []string{userA, userB} {
		_, a := ask(t, db, user, now)
		if _, err := db.Exec(ctx, insertBlock, user, a, 0, "text", 1, "Текст", nil); err != nil {
			t.Fatal(err)
		}
		fail(t, db, user, a, true)
		_, b := ask(t, db, user, now)
		if _, err := db.Exec(ctx, insertBlock, user, b, 0, "text", 1, "Текст", nil); err != nil {
			t.Fatal(err)
		}
	}

	tag, err := db.Exec(ctx, purgeUser, userA)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("purge: %v, %d rows", err, tag.RowsAffected())
	}
	// A repeated purge finds nothing and is still a success for PurgeUserData.
	if tag, err := db.Exec(ctx, purgeUser, userA); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("repeated purge: %v, %d rows", err, tag.RowsAffected())
	}

	for _, table := range []string{"user_state", "question", "answer", "answer_block"} {
		var a, b int
		q := `SELECT count(*) FILTER (WHERE user_id = $1), count(*) FILTER (WHERE user_id = $2) FROM ` + table
		if err := db.QueryRow(ctx, q, userA, userB).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		if a != 0 || b == 0 {
			t.Errorf("%s: %d rows of the purged user, %d of the other; want 0 and some", table, a, b)
		}
	}
}

// Stand-in for the tombstone table of go-tombstone (FIN-110), which has no migrations yet: the
// tests below check the order of statements this schema needs from AskQuestion and
// PurgeUserData, not the library.
const createTombstone = `CREATE TABLE test_tombstone (user_id uuid PRIMARY KEY)`

const isDeleted = `SELECT EXISTS (SELECT 1 FROM test_tombstone WHERE user_id = $1)`

// waitBlocked waits until n backends of the test database wait for a lock.
func waitBlocked(t *testing.T, db *pgxpool.Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got int
		err := db.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends wait for a lock, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countRows(t *testing.T, db *pgxpool.Pool, user string) int {
	t.Helper()
	var n int
	err := db.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM user_state WHERE user_id = $1)
     + (SELECT count(*) FROM question WHERE user_id = $1)
     + (SELECT count(*) FROM answer WHERE user_id = $1)`, user).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// purge runs PurgeUserData in tx: tombstone, the row lock, the cascade.
func purge(ctx context.Context, tx pgx.Tx, user string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO test_tombstone (user_id) VALUES ($1) ON CONFLICT DO NOTHING`, user); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, lockForPurge, user); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, purgeUser, user)
	return err
}

// askChecked runs AskQuestion with the tombstone check right after the limit statement, which
// holds the user's row lock: errDeleted means the call is refused with TOKEN_REVOKED.
func askChecked(ctx context.Context, tx pgx.Tx, user string, now time.Time) error {
	var day time.Time
	var n int
	if err := tx.QueryRow(ctx, incrementLimit, user, now, dailyMax).Scan(&day, &n); err != nil {
		return err
	}
	var deleted bool
	if err := tx.QueryRow(ctx, isDeleted, user).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return errDeleted
	}
	var q string
	if err := tx.QueryRow(ctx, insertQuestion, user, "q", "with").Scan(&q); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, insertAnswer, user, q, day)
	return err
}

var errDeleted = errors.New("user deleted")

// A question asked while the user is being purged leaves no data in either order.
func TestPurgeAndConcurrentQuestionLeaveNoData(t *testing.T) {
	now := at("2026-10-19T12:00:00+03:00")

	t.Run("question first, first row of the user", func(t *testing.T) {
		db := setup(t)
		ctx := context.Background()
		if _, err := db.Exec(ctx, createTombstone); err != nil {
			t.Fatal(err)
		}
		ask, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ask.Rollback(ctx) }()
		var day time.Time
		var n int
		if err := ask.QueryRow(ctx, incrementLimit, userA, now, dailyMax).Scan(&day, &n); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() { done <- pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return purge(ctx, tx, userA) }) }()
		// The purge waits on the uncommitted first row of the user instead of deleting
		// nothing and letting the question commit after it.
		waitBlocked(t, db, 1)

		var deleted bool
		if err := ask.QueryRow(ctx, isDeleted, userA).Scan(&deleted); err != nil || deleted {
			t.Fatalf("tombstone visible before the purge commits: %v, %v", deleted, err)
		}
		var q string
		if err := ask.QueryRow(ctx, insertQuestion, userA, "q", "with").Scan(&q); err != nil {
			t.Fatal(err)
		}
		if _, err := ask.Exec(ctx, insertAnswer, userA, q, day); err != nil {
			t.Fatal(err)
		}
		if err := ask.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n := countRows(t, db, userA); n != 0 {
			t.Fatalf("%d rows of the purged user, want 0", n)
		}
	})

	t.Run("purge first", func(t *testing.T) {
		db := setup(t)
		ctx := context.Background()
		if _, err := db.Exec(ctx, createTombstone); err != nil {
			t.Fatal(err)
		}
		ask(t, db, userA, now) // the user already has data
		p, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Rollback(ctx) }()
		if err := purge(ctx, p, userA); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			done <- pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error { return askChecked(ctx, tx, userA, now) })
		}()
		// The question waits on the row the purge holds, then sees its tombstone.
		waitBlocked(t, db, 1)
		if err := p.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, errDeleted) {
			t.Fatalf("question after the purge: %v, want the tombstone refusal", err)
		}
		if n := countRows(t, db, userA); n != 0 {
			t.Fatalf("%d rows of the purged user, want 0", n)
		}
	})
}

// Every table of the schema and every column of it carries a description in Russian
// (COMMENT ON TABLE, COMMENT ON COLUMN, owner's request in the review of FIN-114), so a table
// or a column added later without one fails here. schema_migrations belongs to golang-migrate.
func TestEveryTableAndColumnHasDescription(t *testing.T) {
	db := setup(t)
	// One row per table (column NULL) and per column of it.
	rows, err := db.Query(context.Background(), `
		WITH described_table AS (
			SELECT c.oid, c.relname
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public'
			  AND c.relkind IN ('r', 'p')
			  AND NOT c.relispartition
			  AND c.relname <> 'schema_migrations'
		)
		SELECT relname, NULL::text, coalesce(obj_description(oid, 'pg_class'), '')
		FROM described_table
		UNION ALL
		SELECT t.relname, a.attname::text, coalesce(col_description(t.oid, a.attnum), '')
		FROM described_table t
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 1, 2 NULLS FIRST`)
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		table       string
		column      *string
		description string
	}
	objects, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (object, error) {
		var o object
		err := row.Scan(&o.table, &o.column, &o.description)
		return o, err
	})
	if err != nil {
		t.Fatal(err)
	}
	var tables, columns int
	for _, o := range objects {
		name, statement := "table "+o.table, "COMMENT ON TABLE"
		if o.column != nil {
			name, statement = "column "+o.table+"."+*o.column, "COMMENT ON COLUMN"
			columns++
		} else {
			tables++
		}
		description := strings.TrimSpace(o.description)
		switch {
		case description == "":
			t.Errorf("%s has no %s", name, statement)
		case !startsInRussian.MatchString(description):
			t.Errorf("%s: description does not start in Russian: %q", name, description)
		}
	}
	if tables == 0 || columns == 0 {
		t.Fatalf("found %d tables and %d columns in schema public", tables, columns)
	}
}

// startsInRussian accepts a description whose first letter is Cyrillic.
var startsInRussian = regexp.MustCompile(`^\p{Cyrillic}`)
