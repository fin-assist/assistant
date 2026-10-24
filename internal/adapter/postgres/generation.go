package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
	"github.com/fin-assist/assistant/internal/generation"
)

// Generations persists streaming progress and exactly-once terminal accounting.
type Generations struct{ db *DB }

// NewGenerations uses the service primary and caller-owned write transactions.
func NewGenerations(db *DB) *Generations { return &Generations{db: db} }
func generationFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("generation storage unavailable")
}

// History returns newest fitting complete pairs, in chronological order, without splitting pairs.
func (r *Generations) History(ctx context.Context, task domain.GenerationTask, maxBytes int) ([]generation.HistoryPair, error) {
	result := []generation.HistoryPair{}
	if maxBytes <= 0 {
		return result, nil
	}
	rows, err := r.db.primary.Query(ctx, `SELECT q.text,coalesce((SELECT string_agg(CASE WHEN b.type='text' THEN coalesce(b.markdown,'') ELSE coalesce(b.body->>'alt_text','') END,E'\n' ORDER BY b.position) FROM answer_block b WHERE b.user_id=a.user_id AND b.answer_id=a.id),'')
 FROM question q JOIN answer a ON a.user_id=q.user_id AND a.question_id=q.id
 WHERE q.user_id=$1 AND q.id<$2 AND a.replaced_at IS NULL AND a.status='complete' ORDER BY q.id DESC`, task.UserID, task.QuestionID)
	if err != nil {
		return nil, generationFailure(ctx)
	}
	defer rows.Close()
	for rows.Next() {
		var p generation.HistoryPair
		if err := rows.Scan(&p.Question, &p.Answer); err != nil {
			return nil, generationFailure(ctx)
		}
		size := len(p.Question) + len(p.Answer)
		if size > maxBytes {
			break
		}
		maxBytes -= size
		result = append(result, p)
	}
	if rows.Err() != nil {
		return nil, generationFailure(ctx)
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result, nil
}

// Append serializes a text block append with terminal updates without reviving an answer.
func (r *Generations) Append(ctx context.Context, task domain.GenerationTask, position int, text string) error {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return err
	}
	if position < 0 || position > 32767 {
		return errors.New("invalid answer block position")
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT true FROM answer WHERE user_id=$1 AND id=$2 AND question_id=$3 AND status='generating' AND replaced_at IS NULL FOR SHARE`, task.UserID, task.AnswerID, task.QuestionID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return generation.ErrStopped
	}
	if err != nil {
		return generationFailure(ctx)
	}
	count, err := savedBlockCount(ctx, tx, task)
	if err != nil {
		return err
	}
	if position > count {
		return errors.New("answer block position is not contiguous")
	}
	tag, err := tx.Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown) VALUES ($1,$2,$3,'text',1,$4)
 ON CONFLICT(user_id,answer_id,position) DO UPDATE SET markdown=coalesce(answer_block.markdown,'') || EXCLUDED.markdown WHERE answer_block.type='text' AND answer_block.schema_version=1`, task.UserID, task.AnswerID, position, text)
	if err != nil {
		return generationFailure(ctx)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("answer block is not appendable text")
	}
	if err := notifyAnswer(ctx, tx, task.AnswerID); err != nil {
		return generationFailure(ctx)
	}
	return nil
}

// AppendBlock appends one complete catalog block without replacing published data.
// The caller owns the guarded transaction. Ready blocks serialize with all text
// appends and terminal transitions through the answer lock.
func (r *Generations) AppendBlock(ctx context.Context, task domain.GenerationTask, position int, block dialogue.Block) error {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return err
	}
	var envelope struct {
		AltText string                     `json:"alt_text"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if position < 0 || position > 32767 || block.Version != 1 || (block.Type != "chart" && block.Type != "rows") || block.Text != "" || json.Unmarshal(block.Body, &envelope) != nil || strings.TrimSpace(envelope.AltText) == "" || envelope.Data == nil {
		return errors.New("invalid ready answer block")
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT true FROM answer WHERE user_id=$1 AND id=$2 AND question_id=$3 AND status='generating' AND replaced_at IS NULL FOR UPDATE`, task.UserID, task.AnswerID, task.QuestionID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return generation.ErrStopped
	}
	if err != nil {
		return generationFailure(ctx)
	}
	count, err := savedBlockCount(ctx, tx, task)
	if err != nil {
		return err
	}
	if position != count {
		return errors.New("ready answer block position is not next")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,body) VALUES ($1,$2,$3,$4,$5,$6)`, task.UserID, task.AnswerID, position, block.Type, block.Version, []byte(block.Body)); err != nil {
		return generationFailure(ctx)
	}
	if err := notifyAnswer(ctx, tx, task.AnswerID); err != nil {
		return generationFailure(ctx)
	}
	return nil
}

func savedBlockCount(ctx context.Context, tx pgx.Tx, task domain.GenerationTask) (int, error) {
	var count, first, next int
	err := tx.QueryRow(ctx, `SELECT count(*),coalesce(min(position),0),coalesce(max(position)::integer+1,0) FROM answer_block WHERE user_id=$1 AND answer_id=$2`, task.UserID, task.AnswerID).Scan(&count, &first, &next)
	if err != nil {
		return 0, generationFailure(ctx)
	}
	if first != 0 || next != count {
		return 0, errors.New("saved answer blocks are not contiguous")
	}
	return count, nil
}

// Finish applies a terminal transition and its refund at most once under the user lock.
func (r *Generations) Finish(ctx context.Context, task domain.GenerationTask, terminal generation.Terminal) (generation.FinishResult, error) {
	out, now := terminal.Outcome, terminal.At
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return generation.FinishResult{}, err
	}
	var limitDay pgtype.Date
	var used int
	err = tx.QueryRow(ctx, `SELECT limit_day,limit_used FROM user_state WHERE user_id=$1 FOR UPDATE`, task.UserID).Scan(&limitDay, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return generation.FinishResult{}, nil
	}
	if err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	var chargeDay pgtype.Date
	var wasCharged bool
	err = tx.QueryRow(ctx, `SELECT charge_day,charged FROM answer WHERE user_id=$1 AND id=$2 AND question_id=$3 AND status='generating' AND replaced_at IS NULL FOR UPDATE`, task.UserID, task.AnswerID, task.QuestionID).Scan(&chargeDay, &wasCharged)
	if errors.Is(err, pgx.ErrNoRows) {
		return generation.FinishResult{}, nil
	}
	if err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	var errorCode *string
	if out.ErrorCode != "" {
		errorCode = &out.ErrorCode
	}
	if terminal.PreserveBlockPrefix {
		if err := stableBlockPrefix(ctx, tx, task, out.Blocks); err != nil {
			return generation.FinishResult{}, err
		}
	}
	if len(out.Blocks) > 32768 {
		return generation.FinishResult{}, errors.New("too many answer blocks")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM answer_block WHERE user_id=$1 AND answer_id=$2`, task.UserID, task.AnswerID); err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	for i, b := range out.Blocks {
		var markdown *string
		var body []byte
		if b.Type == "text" {
			markdown = &b.Text
		} else {
			body = b.Body
		}
		if _, err := tx.Exec(ctx, `INSERT INTO answer_block(user_id,answer_id,position,type,schema_version,markdown,body) VALUES ($1,$2,$3,$4,$5,$6,$7)`, task.UserID, task.AnswerID, i, b.Type, b.Version, markdown, body); err != nil {
			return generation.FinishResult{}, generationFailure(ctx)
		}
	}
	chips := out.Chips
	if chips == nil {
		chips = []json.RawMessage{}
	}
	encoded, err := json.Marshal(chips)
	if err != nil {
		return generation.FinishResult{}, errors.New("invalid answer chips")
	}
	var calculated, from, to *time.Time
	if out.Source != nil {
		calculated = &out.Source.CalculatedAt
		from = out.Source.From
		to = out.Source.To
	}
	if _, err := tx.Exec(ctx, `UPDATE answer SET status=$3,charged=$4,error_code=$5,retryable=$6,calculated_at=$7,data_from=$8,data_to=$9,chips=$10,finished_at=$11 WHERE user_id=$1 AND id=$2`, task.UserID, task.AnswerID, terminal.Status, out.Charged, errorCode, terminal.Retryable, calculated, from, to, encoded, now); err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	if terminal.RefundCurrentDay && wasCharged && limitDay.Valid && chargeDay.Time.Equal(limitDay.Time) {
		if used < 1 {
			return generation.FinishResult{}, errors.New("invalid assistant refund counter")
		}
		if _, err := tx.Exec(ctx, `UPDATE user_state SET limit_used=limit_used-1 WHERE user_id=$1`, task.UserID); err != nil {
			return generation.FinishResult{}, generationFailure(ctx)
		}
	}
	daily, err := applyOffTopic(ctx, tx, task.UserID, task.AnswerID, now, terminal.Ledger)
	if err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	if err := notifyAnswer(ctx, tx, task.AnswerID); err != nil {
		return generation.FinishResult{}, generationFailure(ctx)
	}
	return generation.FinishResult{Applied: true, OffTopicDailyCount: daily}, nil
}

func stableBlockPrefix(ctx context.Context, tx pgx.Tx, task domain.GenerationTask, blocks []dialogue.Block) error {
	bodies := make([]json.RawMessage, len(blocks))
	for i, block := range blocks {
		bodies[i] = block.Body
	}
	encoded, err := json.Marshal(bodies)
	if err != nil {
		return errors.New("invalid completed answer block JSON")
	}
	// PostgreSQL compares jsonb semantically, including exact numeric values. It
	// ignores JSON formatting and key order without converting numbers to float64.
	rows, err := tx.Query(ctx, `SELECT position,type,schema_version,coalesce(markdown,''),coalesce(body=($3::jsonb->position),false) FROM answer_block WHERE user_id=$1 AND answer_id=$2 ORDER BY position`, task.UserID, task.AnswerID, encoded)
	if err != nil {
		return generationFailure(ctx)
	}
	defer rows.Close()
	expected := 0
	for rows.Next() {
		var position int
		var version int32
		var kind, text string
		var sameBody bool
		if err := rows.Scan(&position, &kind, &version, &text, &sameBody); err != nil {
			return generationFailure(ctx)
		}
		if position != expected || position >= len(blocks) || blocks[position].Type != kind || blocks[position].Version != version {
			return errors.New("completed answer changed saved block prefix")
		}
		if kind == "text" {
			if version != 1 || !strings.HasPrefix(blocks[position].Text, text) {
				return errors.New("completed answer regressed saved text")
			}
		} else if !sameBody {
			return errors.New("completed answer changed saved structured block")
		}
		expected++
	}
	if rows.Err() != nil {
		return generationFailure(ctx)
	}
	return nil
}

// Expired returns a bounded primary batch for a worker to terminalize under fresh guards.
func (r *Generations) Expired(ctx context.Context, before time.Time, limit int) ([]domain.GenerationTask, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid expired answer batch")
	}
	rows, err := r.db.primary.Query(ctx, `SELECT a.user_id::text,q.id::text,a.id::text,q.text,q.transfer_mode,coalesce(s.time_zone,'Europe/Moscow'),q.created_at,a.created_at
 FROM answer a JOIN question q ON q.user_id=a.user_id AND q.id=a.question_id JOIN user_state s ON s.user_id=a.user_id
 WHERE a.status='generating' AND a.replaced_at IS NULL AND a.created_at<=$1 ORDER BY a.created_at,a.user_id,a.id LIMIT $2`, before, limit)
	if err != nil {
		return nil, generationFailure(ctx)
	}
	defer rows.Close()
	tasks := []domain.GenerationTask{}
	for rows.Next() {
		var task domain.GenerationTask
		if err := rows.Scan(&task.UserID, &task.QuestionID, &task.AnswerID, &task.Text, &task.TransferMode, &task.TimeZone, &task.QuestionCreatedAt, &task.CreatedAt); err != nil {
			return nil, generationFailure(ctx)
		}
		tasks = append(tasks, task)
	}
	if rows.Err() != nil {
		return nil, generationFailure(ctx)
	}
	return tasks, nil
}
