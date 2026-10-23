package generation

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fin-assist/assistant/internal/tooldata"
)

// ErrRefused identifies an explicit model refusal without provider text.
var ErrRefused = errors.New("model refused the answer")

// ErrInputTooLarge is a transport preflight rejection before credentials,
// network access, text callbacks or a successful request reservation.
var ErrInputTooLarge = errors.New("model input exceeds transport limit")

// ModelRequest is one stateless model step; the transport adds its wire policy.
type ModelRequest struct {
	Instructions                  string
	Input, Tools                  []json.RawMessage
	MaxOutputTokens, MaxToolCalls int
}

// ModelItem preserves completed provider output for the next stateless request.
type ModelItem struct {
	ID, Type, CallID, Name, Arguments string
	Raw                               json.RawMessage
}

// ModelResult is available only after validated provider completion.
type ModelResult struct {
	Output []ModelItem
	Usage  Usage
}

// Usage preserves provider counters; they are not substitutes for request reservations.
type Usage struct{ InputTokens, OutputTokens, TotalTokens, ToolTokens int64 }

// Observer receives only bounded registry names and terminal codes, never dialogue data.
type Observer interface {
	Step(Usage)
	Function(string)
	Searches(int)
	Finished(string, bool, time.Duration)
}

// ModelDelta is one incremental text fragment within a model step.
type ModelDelta struct {
	OutputIndex, ContentIndex int
	Text                      string
}

// Model reserves the complete serialized wire request before any network effect.
// A guard error must be returned unchanged and without callbacks or network access.
type Model interface {
	Stream(context.Context, ModelRequest, func(ModelDelta) error, func([]byte) error) (ModelResult, error)
}

// EngineLimits are explicit per-question policy, independent of transport memory limits.
type EngineLimits struct {
	Budget                                             BudgetLimits
	MaxOutputTokens, ParallelFunctions, MaxSearchCalls int
	Observer                                           Observer
}

// ToolResultReference connects trusted data to its original server-scoped request.
// A future structured presenter must use these values rather than model-written numbers.
type ToolResultReference struct {
	CallID, Name string
	Arguments    json.RawMessage
	// Args is one of tooldata.AggregateArgs, FindArgs, AnalyticsArgs or PeriodsArgs;
	// overview has no arguments. It is validated before the corresponding RPC.
	Args   any
	Result tooldata.Result
}

// Engine owns the stateless model/tool cycle and guarded durable answer writes.
// It is immutable after construction and can run different committed answers concurrently.
type Engine struct {
	repository   Repository
	transactions Transactions
	tombstones   Tombstones
	model        Model
	data         tooldata.Data
	limits       EngineLimits
	now          func() time.Time
}

// New requires explicit policy and injected dependencies; no generation starts here.
func New(repository Repository, transactions Transactions, tombstones Tombstones, model Model, data tooldata.Data, limits EngineLimits, now func() time.Time) (*Engine, error) {
	if repository == nil || transactions == nil || tombstones == nil || model == nil || data == nil || now == nil || limits.MaxOutputTokens < 16 || limits.ParallelFunctions <= 0 || limits.ParallelFunctions > limits.Budget.MaxFunctions || limits.MaxSearchCalls <= 0 {
		return nil, ErrInvalidBudget
	}
	if _, err := NewBudget(limits.Budget); err != nil {
		return nil, err
	}
	return &Engine{repository: repository, transactions: transactions, tombstones: tombstones, model: model, data: data, limits: limits, now: now}, nil
}
