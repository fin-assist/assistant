package asking

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/assistant/internal/account"
)

// Transactions owns the primary transaction and commits only a nil callback result.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Tombstones guards every write before any key or user-row lock.
type Tombstones interface {
	Guard(context.Context, string) (bool, error)
}

// Consent reads the current grant from the user service's primary.
type Consent interface {
	Granted(context.Context, string) (bool, error)
}

// Keys is implemented by an adapter which translates effect errors before they
// reach the library for outcome classification. The service still controls rollback.
type Keys interface {
	Reserve(context.Context, idem.Request) (idem.Reservation, idem.Result, error)
	WithCompletion(context.Context) (context.Context, context.CancelFunc)
	DoReserved(context.Context, idem.Reservation, func(context.Context) (idem.Response, error)) (idem.Result, error)
	Release(context.Context, idem.Reservation) error
}

// Service accepts questions and retries, with no network I/O in a transaction.
type Service struct {
	tx       Transactions
	repo     Repository
	deleted  Tombstones
	keys     Keys
	consent  Consent
	starter  Starter
	now      func() time.Time
	dailyMax int
}

// New wires the acceptance workflow. DailyMax and dependencies are validated by assembly.
func New(tx Transactions, repo Repository, deleted Tombstones, keys Keys, consent Consent, starter Starter, now func() time.Time, dailyMax int) *Service {
	return &Service{tx: tx, repo: repo, deleted: deleted, keys: keys, consent: consent, starter: starter, now: now, dailyMax: dailyMax}
}

// Ask accepts one new question, or replays its already committed result.
func (s *Service) Ask(ctx context.Context, user string, key idem.Request, text, mode string) (Reply, error) {
	return s.accept(ctx, user, key, func(ctx context.Context, now time.Time) (Reply, error) {
		task, err := s.repo.Create(ctx, user, text, mode, now)
		return Reply{Task: task}, err
	})
}

// Retry accepts a replacement from the saved question, never caller-provided text or mode.
func (s *Service) Retry(ctx context.Context, user string, key idem.Request, answer string) (Reply, error) {
	return s.accept(ctx, user, key, func(ctx context.Context, now time.Time) (Reply, error) {
		task, err := s.repo.Retry(ctx, user, answer, now)
		return Reply{Task: task}, err
	})
}

func (s *Service) accept(ctx context.Context, user string, key idem.Request, create func(context.Context, time.Time) (Reply, error)) (out Reply, rpcErr error) {
	// The key scope cannot be supplied independently of the verified caller.
	if key.Scope != user || user == "" {
		return Reply{}, ErrDeleted
	}
	var token idem.Reservation
	var result idem.Result
	committed := false
	defer func() {
		if committed || token == (idem.Reservation{}) {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.tx.Run(cleanup, func(txCtx context.Context) error { return s.keys.Release(txCtx, token) }); err != nil {
			rpcErr = errors.New("assistant reservation cleanup failed")
		}
	}()
	err := s.tx.Run(ctx, func(txCtx context.Context) error {
		if err := s.guard(txCtx, user); err != nil {
			return err
		}
		var err error
		token, result, err = s.keys.Reserve(txCtx, key)
		return err
	})
	if err != nil {
		return Reply{}, err
	}
	if !result.Replayed {
		granted, err := s.consent.Granted(ctx, user)
		if err != nil {
			if ctx.Err() != nil {
				return Reply{}, ctx.Err()
			}
			return Reply{}, ErrUnavailable
		}
		if !granted {
			return Reply{}, ErrConsent
		}
		if s.starter == nil || !s.starter.Ready() {
			return Reply{}, ErrUnavailable
		}
		completion, cancel := s.keys.WithCompletion(ctx)
		defer cancel()
		err = s.tx.Run(completion, func(txCtx context.Context) error {
			if err := s.guard(txCtx, user); err != nil {
				return err
			}
			var err, callbackErr error
			result, err = s.keys.DoReserved(txCtx, token, func(effectCtx context.Context) (response idem.Response, effectErr error) {
				defer func() { callbackErr = effectErr }()
				state, err := s.repo.Lock(effectCtx, user)
				if err != nil {
					return idem.Response{}, err
				}
				now := s.now() // after the user lock, so a midnight wait cannot charge yesterday
				limit := account.CalculateLimit(now, state, s.dailyMax)
				if limit.Remaining == 0 {
					return idem.Response{}, &LimitError{ResetsAt: limit.ResetsAt}
				}
				// Retry can produce a cacheable 422: validate/create before charging so
				// committing that refusal never commits a consumed daily attempt.
				reply, err := create(effectCtx, now)
				if err != nil {
					return idem.Response{}, err
				}
				day := limit.ResetsAt.AddDate(0, 0, -1)
				if err = s.repo.Charge(effectCtx, user, day, s.dailyMax-limit.Remaining+1); err != nil {
					return idem.Response{}, err
				}
				reply.Remaining = limit.Remaining - 1
				body, err := json.Marshal(reply)
				return idem.Response{Body: body}, err
			})
			// Acceptance refusals are retriable state, including RetryDenied:
			// generating can become failed later while the caller keeps its key.
			// Roll back even if the shared default policy tentatively stored 422.
			if callbackErr != nil {
				return callbackErr
			}
			return err
		})
		if err != nil {
			if errors.Is(err, idem.ErrNotStored) && result.Err != nil {
				return Reply{}, result.Err
			}
			return Reply{}, err
		}
	}
	committed = true
	if result.Err != nil {
		return Reply{}, result.Err
	}
	if err := json.Unmarshal(result.Response.Body, &out); err != nil {
		return Reply{}, errors.New("invalid saved assistant acceptance")
	}
	if out.Task.UserID != user || out.Task.AnswerID == "" || out.Task.QuestionID == "" || out.Remaining < 0 {
		return Reply{}, errors.New("invalid saved assistant acceptance")
	}
	if !result.Replayed {
		s.starter.Start(out.Task)
	}
	return out, nil
}

func (s *Service) guard(ctx context.Context, user string) error {
	alive, err := s.deleted.Guard(ctx, user)
	if err != nil {
		return err
	}
	if !alive {
		return ErrDeleted
	}
	return nil
}
