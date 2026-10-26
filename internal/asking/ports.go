// Package asking owns accepting and retrying an assistant question.
package asking

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/assistant/internal/account"
	"github.com/fin-assist/assistant/internal/domain"
)

var (
	// ErrNotFound also hides a foreign answer.
	ErrNotFound = errors.New("answer not found")
	// ErrRetryDenied means the answer cannot be replaced.
	ErrRetryDenied = errors.New("answer retry not allowed")
	// ErrDeleted rejects writes after account deletion.
	ErrDeleted = errors.New("assistant account deleted")
	// ErrConsent means current assistant consent is missing.
	ErrConsent = errors.New("assistant consent required")
	// ErrUnavailable means generation is not configured or a dependency failed.
	ErrUnavailable = errors.New("assistant generation unavailable")
)

// RequestRetryError carries a usecase decision about repeating the original RPC.
// It never grants a new generation or proves the caller's replay window is open.
type RequestRetryError struct {
	cause   error
	allowed bool
}

func (e *RequestRetryError) Error() string { return e.cause.Error() }

// Unwrap preserves the original domain refusal for the transport adapter.
func (e *RequestRetryError) Unwrap() error { return e.cause }

// RequestRetryPermission reports the decision made at the refusal's execution phase.
func (e *RequestRetryError) RequestRetryPermission() bool { return e.allowed }

func requestError(cause error, allowed bool) error {
	return &RequestRetryError{cause: cause, allowed: allowed}
}

// LimitError carries the next reset without user-facing text.
type LimitError struct{ ResetsAt time.Time }

func (e *LimitError) Error() string { return "assistant question limit exceeded" }

// RateLimitError carries when the rolling off-topic gate may admit a new question.
type RateLimitError struct{ RetryAt time.Time }

func (e *RateLimitError) Error() string { return "assistant off-topic limit exceeded" }

// Repository operates inside the caller's guarded primary transaction.
type Repository interface {
	Lock(context.Context, string) (account.State, error)
	// RecentOffTopic returns at most latest completion instants, newest first,
	// strictly after the supplied cutoff. It runs in the caller's primary
	// transaction: once before the consent read and again under the user lock.
	RecentOffTopic(context.Context, string, time.Time, int) ([]time.Time, error)
	// AnswerExists returns ErrNotFound unless the user owns the answer, replaced
	// ones included (those are refused later with ErrRetryDenied).
	AnswerExists(context.Context, string, string) error
	Charge(context.Context, string, time.Time, int) error
	Create(context.Context, string, string, string, time.Time) (domain.GenerationTask, error)
	Retry(context.Context, string, string, time.Time) (domain.GenerationTask, error)
}

// Reply is the durable idempotent outcome; Remaining is the value at acceptance.
type Reply struct {
	Task      domain.GenerationTask
	Remaining int
}

// Starter starts only committed tasks, independently of the request context.
type Starter interface {
	Ready() bool
	Start(domain.GenerationTask)
}
