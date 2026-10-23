package account

import (
	"context"
	"time"
)

// Repository stores user settings and reads the current counter on primary.
type Repository interface {
	State(context.Context, string) (State, error)
	SetTimezone(context.Context, string, string, int32) error
	Delete(context.Context, string) error
}

// Transactions runs a callback in the service-owned transaction.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Tombstones serializes writes with deletion. Purge and Guard use the current transaction.
type Tombstones interface {
	Guard(context.Context, string) (bool, error)
	Purge(context.Context, string, func(context.Context, string) error) error
}

// Service implements account use cases without transport or database types.
type Service struct {
	tx       Transactions
	repo     Repository
	deleted  Tombstones
	now      func() time.Time
	dailyMax int
}

// New wires required dependencies; the assembly supplies a validated daily maximum.
func New(tx Transactions, repo Repository, deleted Tombstones, now func() time.Time, dailyMax int) *Service {
	return &Service{tx: tx, repo: repo, deleted: deleted, now: now, dailyMax: dailyMax}
}

// GetLimit reads primary without creating state for a new user.
func (s *Service) GetLimit(ctx context.Context, id string) (Limit, error) {
	state, err := s.repo.State(ctx, id)
	if err != nil {
		return Limit{}, err
	}
	return CalculateLimit(s.now(), state, s.dailyMax), nil
}

// SetTimezone applies a versioned event unless deletion has already won the lock.
func (s *Service) SetTimezone(ctx context.Context, id, zone string, version int32) error {
	return s.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := s.deleted.Guard(ctx, id)
		if err != nil || !alive {
			return err
		}
		return s.repo.SetTimezone(ctx, id, zone, version)
	})
}

// Purge deletes every assistant record and remembers deletion in the same transaction.
func (s *Service) Purge(ctx context.Context, id string) error {
	return s.tx.Run(ctx, func(ctx context.Context) error { return s.deleted.Purge(ctx, id, s.repo.Delete) })
}
