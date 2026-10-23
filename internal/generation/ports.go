package generation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
)

// ErrStopped means deletion, replacement or a prior terminal transition won.
var ErrStopped = errors.New("answer generation stopped")

// HistoryPair contains one complete question and answer in chronological order.
// Structured blocks contribute their server-produced text alternative.
type HistoryPair struct{ Question, Answer string }

// Outcome is validated server policy, not a provider's arbitrary JSON result.
// A nonempty ErrorCode clears all partial blocks, chips and source and refunds.
type Outcome struct {
	ErrorCode string
	Charged   bool
	Blocks    []dialogue.Block
	Chips     []json.RawMessage
	Source    *dialogue.Source
}

// FinishResult contains only effects of a newly committed terminal transition.
type FinishResult struct {
	Applied            bool
	OffTopicDailyCount int
}

// Repository reads primary and writes only in the use case's guarded transaction.
type Repository interface {
	History(context.Context, domain.GenerationTask, int) ([]HistoryPair, error)
	Append(context.Context, domain.GenerationTask, int, string) error
	AppendBlock(context.Context, domain.GenerationTask, int, dialogue.Block) error
	Finish(context.Context, domain.GenerationTask, Outcome, time.Time) (FinishResult, error)
	Expired(context.Context, time.Time, int) ([]domain.GenerationTask, error)
}

// Transactions supplies service-owned atomic writes.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Tombstones serializes every write with account deletion. Guard returns true
// while the user is alive and false when a tombstone already exists.
type Tombstones interface {
	Guard(context.Context, string) (bool, error)
}
