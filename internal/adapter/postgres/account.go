package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fin-assist/assistant/internal/account"
)

// Accounts stores assistant user state. All writes require a guarded service transaction.
type Accounts struct{ db *DB }

// NewAccounts uses the service's pools and transaction context.
func NewAccounts(db *DB) *Accounts { return &Accounts{db: db} }

// State reads the current counter from primary. A missing row is an unused allowance.
func (r *Accounts) State(ctx context.Context, id string) (account.State, error) {
	var day pgtype.Date
	var used int
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT limit_day,limit_used FROM user_state WHERE user_id=$1`, id).Scan(&day, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return account.State{}, nil
	}
	if err != nil {
		return account.State{}, storageFailure(ctx)
	}
	return account.State{LimitDay: day.Time, Used: used}, nil
}

// SetTimezone applies only a newer settings version; it never changes the counter.
func (r *Accounts) SetTimezone(ctx context.Context, id, zone string, version int32) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("account: transaction required")
	}
	_, err := tx.Exec(ctx, `INSERT INTO user_state AS s (user_id,time_zone,settings_version) VALUES ($1,$2,$3)
 ON CONFLICT (user_id) DO UPDATE SET time_zone=EXCLUDED.time_zone,settings_version=EXCLUDED.settings_version
 WHERE s.settings_version IS NULL OR s.settings_version<EXCLUDED.settings_version`, id, zone, version)
	if err != nil {
		return storageFailure(ctx)
	}
	return nil
}

// Delete removes the user root; foreign keys cascade to questions, answers and blocks.
func (r *Accounts) Delete(ctx context.Context, id string) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("account: transaction required")
	}
	_, err := tx.Exec(ctx, `DELETE FROM user_state WHERE user_id=$1`, id)
	if err != nil {
		return storageFailure(ctx)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify('assistant_users',$1)`, id); err != nil {
		return storageFailure(ctx)
	}
	return nil
}

func storageFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("account storage unavailable")
}
