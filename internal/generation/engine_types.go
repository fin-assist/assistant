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

// ModelItem preserves completed output for the next stateless request. The model
// adapter supplies a unique CallID and keeps that same key in Raw function items.
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
	OffTopicDailyCount(int)
}

// ModelDelta is one incremental text fragment within a model step.
type ModelDelta struct {
	OutputIndex, ContentIndex int
	Text                      string
}

// Model starts one isolated session for each generation. No session context is
// shared between users, answers, or replacement generations.
type Model interface{ NewSession() ModelSession }

// ModelSession owns all steps and unsent preflight retries of one generation.
// It reserves the complete serialized request before any network effect.
type ModelSession interface {
	Stream(context.Context, ModelRequest, func(ModelDelta) error, func([]byte) error) (ModelResult, error)
}

// ModelSessionFunc adapts a stateless stream implementation to an isolated port.
type ModelSessionFunc func(context.Context, ModelRequest, func(ModelDelta) error, func([]byte) error) (ModelResult, error)

func (f ModelSessionFunc) Stream(ctx context.Context, request ModelRequest, emit func(ModelDelta) error, reserve func([]byte) error) (ModelResult, error) {
	return f(ctx, request, emit, reserve)
}

// EngineLimits are explicit per-question policy, independent of transport memory limits.
type EngineLimits struct {
	Budget                                             BudgetLimits
	MaxOutputTokens, ParallelFunctions, MaxSearchCalls int
	Observer                                           Observer
}

// ToolResultReference connects trusted data to its original server-scoped request.
// The structured presenter uses these values rather than model-written numbers.
type ToolResultReference struct {
	CallID, Name string
	Arguments    json.RawMessage
	// Args preserves the handler's validated typed arguments. Built-in readers use
	// tooldata AggregateArgs, FindArgs, AnalyticsArgs and PeriodsArgs; extensions
	// retain their own type without adding cases to the engine.
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
	tools        *ToolRegistry
	limits       EngineLimits
	now          func() time.Time
}

// New requires explicit policy and injected dependencies; no generation starts here.
func New(repository Repository, transactions Transactions, tombstones Tombstones, model Model, data tooldata.Data, limits EngineLimits, now func() time.Time) (*Engine, error) {
	if data == nil {
		return nil, ErrInvalidBudget
	}
	tools, err := NewToolRegistry(BuiltinTools(data)...)
	if err != nil {
		return nil, err
	}
	return NewWithTools(repository, transactions, tombstones, model, tools, limits, now)
}

// NewWithTools injects a validated registry independently of any data-service bundle.
func NewWithTools(repository Repository, transactions Transactions, tombstones Tombstones, model Model, tools *ToolRegistry, limits EngineLimits, now func() time.Time) (*Engine, error) {
	if repository == nil || transactions == nil || tombstones == nil || model == nil || tools == nil || len(tools.tools) == 0 || now == nil || limits.MaxOutputTokens < 16 || limits.ParallelFunctions <= 0 || limits.ParallelFunctions > limits.Budget.MaxFunctions || limits.MaxSearchCalls <= 0 {
		return nil, ErrInvalidBudget
	}
	if _, err := NewBudget(limits.Budget); err != nil {
		return nil, err
	}
	return &Engine{repository: repository, transactions: transactions, tombstones: tombstones, model: model, tools: tools, limits: limits, now: now}, nil
}
