// Package idempotency translates acceptance outcomes for the shared key engine.
package idempotency

import (
	"context"
	"errors"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/assistant/internal/asking"
)

// Engine is the shared library behavior needed by Keys.
type Engine interface {
	Reserve(context.Context, idem.Request) (idem.Reservation, idem.Result, error)
	WithCompletion(context.Context) (context.Context, context.CancelFunc)
	DoReserved(context.Context, idem.Reservation, func(context.Context) (idem.Response, error)) (idem.Result, error)
	Release(context.Context, idem.Reservation) error
}

// Keys maps domain refusals before the library decides whether to cache them.
type Keys struct{ Engine }

// New wraps a shared engine without taking transaction ownership.
func New(engine Engine) *Keys { return &Keys{Engine: engine} }

// DoReserved converts effect errors before the library's outcome classification.
func (k *Keys) DoReserved(ctx context.Context, token idem.Reservation, effect func(context.Context) (idem.Response, error)) (idem.Result, error) {
	return k.Engine.DoReserved(ctx, token, func(ctx context.Context) (idem.Response, error) {
		response, err := effect(ctx)
		return response, ContractError(err)
	})
}

// ContractError maps only known domain refusals. Unknown errors remain internal
// failures, so the library rolls back instead of storing an accidental success.
func ContractError(err error) error {
	var limit *asking.LimitError
	var rateLimit *asking.RateLimitError
	switch {
	case errors.Is(err, asking.ErrNotFound):
		return finerr.NotFound()
	case errors.Is(err, asking.ErrRetryDenied):
		return finerr.RetryNotAllowed()
	case errors.Is(err, asking.ErrDeleted):
		return finerr.TokenRevoked()
	case errors.Is(err, asking.ErrConsent):
		return finerr.ConsentRequired()
	case errors.As(err, &limit):
		return finerr.LimitExceeded(limit.ResetsAt)
	case errors.As(err, &rateLimit):
		return finerr.RateLimited(rateLimit.RetryAt)
	default:
		return err
	}
}
