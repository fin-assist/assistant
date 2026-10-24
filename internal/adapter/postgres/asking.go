package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/asking"
	"github.com/fin-assist/assistant/internal/domain"
)

// Questions persists acceptance under the caller's guarded user lock.
type Questions struct{ db *DB }

// NewQuestions uses the service transaction manager.
func NewQuestions(db *DB) *Questions {
	return &Questions{db: db}
}

func questionFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("question storage unavailable")
}
func writeTransaction(ctx context.Context, db *DB) (pgx.Tx, error) {
	tx, ok := db.Tx(ctx)
	if !ok {
		return nil, errors.New("assistant write requires transaction")
	}
	return tx, nil
}

// Lock establishes and locks the user's root after the caller's tombstone guard.
func (r *Questions) Lock(ctx context.Context, user string) (account.State, error) {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return account.State{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO user_state(user_id) VALUES ($1) ON CONFLICT(user_id) DO NOTHING`, user); err != nil {
		return account.State{}, questionFailure(ctx)
	}
	var day pgtype.Date
	var used int
	if err := tx.QueryRow(ctx, `SELECT limit_day,limit_used FROM user_state WHERE user_id=$1 FOR UPDATE`, user).Scan(&day, &used); err != nil {
		return account.State{}, questionFailure(ctx)
	}
	return account.State{LimitDay: day.Time, Used: used}, nil
}

// Charge stores the use case's already-validated daily counter without choosing a quota.
func (r *Questions) Charge(ctx context.Context, user string, day time.Time, used int) error {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return err
	}
	if used < 1 || used > 32767 || day.IsZero() {
		return errors.New("invalid question charge")
	}
	tag, err := tx.Exec(ctx, `UPDATE user_state SET limit_day=$2,limit_used=$3 WHERE user_id=$1`, user, day.Format("2006-01-02"), used)
	if err != nil {
		return questionFailure(ctx)
	}
	if tag.RowsAffected() != 1 {
		return asking.ErrDeleted
	}
	return nil
}

// Create allocates both UUIDv7 identifiers inside the locked acceptance transaction.
func (r *Questions) Create(ctx context.Context, user, text, mode string, now time.Time) (domain.GenerationTask, error) {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return domain.GenerationTask{}, err
	}
	var question string
	if err := tx.QueryRow(ctx, `INSERT INTO question(user_id,text,transfer_mode,created_at) VALUES ($1,$2,$3,$4) RETURNING id::text`, user, text, mode, now).Scan(&question); err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	task, err := newGenerationAnswer(ctx, tx, user, question, now)
	if err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	if err := notifyAnswer(ctx, tx, task.AnswerID); err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	return task, nil
}

// Retry replaces one failed retryable answer while retaining its original question.
func (r *Questions) Retry(ctx context.Context, user, answer string, now time.Time) (domain.GenerationTask, error) {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return domain.GenerationTask{}, err
	}
	var question, status string
	var code *string
	var retryable *bool
	var replaced *time.Time
	err = tx.QueryRow(ctx, `SELECT question_id::text,status,error_code,retryable,replaced_at FROM answer WHERE user_id=$1 AND id=$2 FOR UPDATE`, user, answer).Scan(&question, &status, &code, &retryable, &replaced)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.GenerationTask{}, asking.ErrNotFound
	}
	if err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	allowed := retryable != nil && *retryable
	if retryable == nil {
		allowed = code != nil && *code == "MODEL_ERROR"
	}
	if replaced != nil || status != "failed" || !allowed {
		return domain.GenerationTask{}, asking.ErrRetryDenied
	}
	if _, err := tx.Exec(ctx, `UPDATE answer SET replaced_at=$3 WHERE user_id=$1 AND id=$2`, user, answer, now); err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	task, err := newGenerationAnswer(ctx, tx, user, question, now)
	if err != nil {
		return domain.GenerationTask{}, questionFailure(ctx)
	}
	for _, id := range []string{answer, task.AnswerID} {
		if err := notifyAnswer(ctx, tx, id); err != nil {
			return domain.GenerationTask{}, questionFailure(ctx)
		}
	}
	return task, nil
}

func newGenerationAnswer(ctx context.Context, tx pgx.Tx, user, question string, now time.Time) (domain.GenerationTask, error) {
	task := domain.GenerationTask{UserID: user, QuestionID: question}
	err := tx.QueryRow(ctx, `WITH inserted AS (
 INSERT INTO answer(user_id,question_id,charge_day,created_at) SELECT user_id,$2,limit_day,$3 FROM user_state WHERE user_id=$1 RETURNING id,created_at
 ) SELECT i.id::text,i.created_at,q.text,q.transfer_mode,q.created_at,coalesce(s.time_zone,'Europe/Moscow')
 FROM inserted i JOIN question q ON q.user_id=$1 AND q.id=$2 JOIN user_state s ON s.user_id=$1`, user, question, now).Scan(&task.AnswerID, &task.CreatedAt, &task.Text, &task.TransferMode, &task.QuestionCreatedAt, &task.TimeZone)
	return task, err
}

func notifyAnswer(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_notify('assistant_answers',$1)`, id)
	return err
}
