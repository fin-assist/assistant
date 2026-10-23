package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// OffTopicRetryAt reads the resettable ledger on primary after Lock. Reading only
// the latest limit events also gives the right expiry when concurrent completions
// have already taken the count beyond the admission threshold.
func (r *Questions) OffTopicRetryAt(ctx context.Context, user string, now time.Time) (time.Time, error) {
	tx, err := writeTransaction(ctx, r.db)
	if err != nil {
		return time.Time{}, err
	}
	if r.offTopicMax < 1 || r.offTopicMax > 32767 || r.offTopicWindow < time.Microsecond || now.IsZero() {
		return time.Time{}, errors.New("invalid off-topic admission policy")
	}
	rows, err := tx.Query(ctx, `SELECT completed_at FROM off_topic_event
 WHERE user_id=$1 AND completed_at>$2 ORDER BY completed_at DESC LIMIT $3`, user, now.Add(-r.offTopicWindow), r.offTopicMax)
	if err != nil {
		return time.Time{}, questionFailure(ctx)
	}
	defer rows.Close()
	count := 0
	var oldest time.Time
	for rows.Next() {
		if err := rows.Scan(&oldest); err != nil {
			return time.Time{}, questionFailure(ctx)
		}
		count++
	}
	if rows.Err() != nil {
		return time.Time{}, questionFailure(ctx)
	}
	if count < r.offTopicMax {
		return time.Time{}, nil
	}
	return oldest.Add(r.offTopicWindow), nil
}

// recordOffTopic runs only after the answer's generating-to-terminal CAS, while
// holding the user's lock. Deleting the ledger on success orders reset and insert
// even when two terminal transitions have exactly the same timestamp.
func recordOffTopic(ctx context.Context, tx pgx.Tx, user, answer, code string, charged bool, at time.Time) (int, error) {
	if code == "" && charged {
		_, err := tx.Exec(ctx, `DELETE FROM off_topic_event WHERE user_id=$1`, user)
		return 0, err
	}
	if code != "OFF_TOPIC" {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO off_topic_event(user_id,answer_id,completed_at) VALUES ($1,$2,$3)`, user, answer, at); err != nil {
		return 0, err
	}
	// The daily diagnostic count includes outcomes preceding a reset. It comes
	// from retained answers, not from the resettable admission ledger.
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	local := at.In(moscow)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, moscow)
	var daily int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM answer WHERE user_id=$1 AND status='failed'
 AND error_code='OFF_TOPIC' AND finished_at >= $2 AND finished_at < $3`, user, start, start.AddDate(0, 0, 1)).Scan(&daily)
	return daily, err
}
