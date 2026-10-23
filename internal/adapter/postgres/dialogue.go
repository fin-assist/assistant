package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/dialogue"
)

// Dialogue reads coherent primary snapshots of saved dialogue.
type Dialogue struct{ db *DB }

// NewDialogue uses the service primary pool.
func NewDialogue(db *DB) *Dialogue { return &Dialogue{db: db} }

func dialogueFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("dialogue storage unavailable")
}

func (r *Dialogue) read(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.db.primary.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return dialogueFailure(ctx)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err = fn(tx); err != nil {
		if errors.Is(err, dialogue.ErrNotFound) {
			return err
		}
		return dialogueFailure(ctx)
	}
	if err = tx.Commit(ctx); err != nil {
		return dialogueFailure(ctx)
	}
	return nil
}

func dialogueZone(ctx context.Context, tx pgx.Tx, user string) (string, error) {
	var zone string
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT time_zone FROM user_state WHERE user_id=$1),'Europe/Moscow')`, user).Scan(&zone)
	return zone, err
}

// List counts questions and active answers separately and returns a page oldest first.
func (r *Dialogue) List(ctx context.Context, user, before string, limit int) (dialogue.Page, error) {
	page := dialogue.Page{Messages: []dialogue.Message{}}
	if limit < 1 || limit > 50 {
		return page, errors.New("invalid dialogue page limit")
	}
	err := r.read(ctx, func(tx pgx.Tx) error {
		var err error
		page.TimeZone, err = dialogueZone(ctx, tx, user)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `WITH messages AS (
 SELECT id,created_at,text,false AS is_answer,id AS question_id,0 AS role_order FROM question WHERE user_id=$1
 UNION ALL
 SELECT id,created_at,'' AS text,true AS is_answer,question_id,1 AS role_order FROM answer WHERE user_id=$1 AND replaced_at IS NULL
 ), cursor AS (
 SELECT id AS question_id,0 AS role_order FROM question WHERE user_id=$1 AND id=NULLIF($2,'')::uuid
 UNION ALL
 SELECT question_id,1 AS role_order FROM answer WHERE user_id=$1 AND id=NULLIF($2,'')::uuid
 )
 SELECT id::text,created_at,text,is_answer FROM messages
 WHERE $2='' OR (question_id,role_order)<(SELECT question_id,role_order FROM cursor)
 ORDER BY question_id DESC,role_order DESC LIMIT $3`, user, before, limit+1)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var m dialogue.Message
			var answer bool
			if err := rows.Scan(&m.ID, &m.CreatedAt, &m.Question, &answer); err != nil {
				rows.Close()
				return err
			}
			if len(page.Messages) == limit {
				page.HasOlder = true
				break
			}
			if answer {
				m.Answer = &dialogue.Answer{}
				ids = append(ids, m.ID)
			}
			page.Messages = append(page.Messages, m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		answers, err := readAnswers(ctx, tx, user, ids, page.TimeZone)
		if err != nil {
			return err
		}
		for i := range page.Messages {
			m := &page.Messages[i]
			if m.Answer != nil {
				a, ok := answers[m.ID]
				if !ok {
					return dialogue.ErrNotFound
				}
				m.Answer = a
			}
		}
		for i, j := 0, len(page.Messages)-1; i < j; i, j = i+1, j-1 {
			page.Messages[i], page.Messages[j] = page.Messages[j], page.Messages[i]
		}
		return nil
	})
	if err != nil {
		return dialogue.Page{}, err
	}
	return page, nil
}

// Snapshot hides replaced and foreign answers, and reads all blocks in the same snapshot.
func (r *Dialogue) Snapshot(ctx context.Context, user, answer string) (dialogue.Answer, error) {
	var result dialogue.Answer
	err := r.read(ctx, func(tx pgx.Tx) error {
		zone, err := dialogueZone(ctx, tx, user)
		if err != nil {
			return err
		}
		answers, err := readAnswers(ctx, tx, user, []string{answer}, zone)
		if err != nil {
			return err
		}
		if len(answers) == 0 {
			return dialogue.ErrNotFound
		}
		for _, a := range answers {
			result = *a
		}
		return nil
	})
	return result, err
}

func readAnswers(ctx context.Context, tx pgx.Tx, user string, ids []string, zone string) (map[string]*dialogue.Answer, error) {
	answers := make(map[string]*dialogue.Answer, len(ids))
	if len(ids) == 0 {
		return answers, nil
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text,a.created_at,a.status,coalesce(a.error_code,''),a.retryable,a.charged,a.currency,
 a.calculated_at,a.data_from,a.data_to,q.transfer_mode,a.chips
 FROM answer a JOIN question q ON q.user_id=a.user_id AND q.id=a.question_id
 WHERE a.user_id=$1 AND a.id=ANY($2::uuid[]) AND a.replaced_at IS NULL`, user, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		a := &dialogue.Answer{TimeZone: zone, Blocks: []dialogue.Block{}, Chips: []json.RawMessage{}}
		s := &dialogue.Source{}
		var calculated *time.Time
		var chips []byte
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Status, &a.ErrorCode, &a.Retryable, &a.Charged, &a.Currency, &calculated, &s.From, &s.To, &s.TransferMode, &chips); err != nil {
			rows.Close()
			return nil, err
		}
		if calculated != nil {
			s.CalculatedAt = *calculated
			a.Source = s
		}
		if err := json.Unmarshal(chips, &a.Chips); err != nil {
			rows.Close()
			return nil, err
		}
		answers[a.ID] = a
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.Query(ctx, `SELECT b.answer_id::text,b.type,b.schema_version,coalesce(b.markdown,''),b.body
 FROM answer_block b JOIN answer a ON a.user_id=b.user_id AND a.id=b.answer_id
 WHERE b.user_id=$1 AND b.answer_id=ANY($2::uuid[]) AND a.replaced_at IS NULL ORDER BY b.answer_id,b.position`, user, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var b dialogue.Block
		if err := rows.Scan(&id, &b.Type, &b.Version, &b.Text, &b.Body); err != nil {
			return nil, err
		}
		if a := answers[id]; a != nil {
			a.Blocks = append(a.Blocks, b)
		}
	}
	return answers, rows.Err()
}
