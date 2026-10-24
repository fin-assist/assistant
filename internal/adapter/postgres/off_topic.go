package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/assistant/internal/domain"
)

// RecentOffTopic reads bounded completion facts under the caller's user lock.
// The use case supplies the cutoff and limit; this adapter decides no admission.
func (r *Questions) RecentOffTopic(ctx context.Context, user string, after time.Time, latest int) ([]time.Time, error) {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT completed_at FROM off_topic_event
 WHERE user_id=$1 AND completed_at>$2 ORDER BY completed_at DESC LIMIT $3`, user, after, latest)
	if err != nil {
		return nil, questionFailure(ctx)
	}
	defer rows.Close()
	var completed []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			return nil, questionFailure(ctx)
		}
		completed = append(completed, at)
	}
	if rows.Err() != nil {
		return nil, questionFailure(ctx)
	}
	return completed, nil
}

// applyOffTopic runs only after the generating-to-terminal CAS under the user
// lock. The use case selected the action and diagnostic day before this write.
func applyOffTopic(ctx context.Context, tx pgx.Tx, user, answer string, at time.Time, effect domain.OffTopicLedgerEffect) (int, error) {
	switch effect.Action {
	case domain.ResetOffTopic:
		_, err := tx.Exec(ctx, `DELETE FROM off_topic_event WHERE user_id=$1`, user)
		return 0, err
	case domain.KeepOffTopic:
		return 0, nil
	case domain.RecordOffTopic:
	default:
		return 0, errors.New("invalid off-topic ledger effect")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO off_topic_event(user_id,answer_id,completed_at) VALUES ($1,$2,$3)`, user, answer, at); err != nil {
		return 0, err
	}
	var daily int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM answer WHERE user_id=$1 AND status='failed'
 AND error_code='OFF_TOPIC' AND finished_at >= $2 AND finished_at < $3`, user, effect.DayStart, effect.DayEnd).Scan(&daily)
	return daily, err
}
