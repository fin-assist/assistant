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

// LimitError carries the next reset without user-facing text.
type LimitError struct{ ResetsAt time.Time }

func (e *LimitError) Error() string { return "assistant question limit exceeded" }

// Repository operates inside the caller's guarded primary transaction.
type Repository interface {
	Lock(context.Context, string) (account.State, error)
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
