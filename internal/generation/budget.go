package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"unicode/utf8"
)

var (
	// ErrBudgetExceeded reports a reservation exceeding a configured runtime quota.
	ErrBudgetExceeded = errors.New("generation budget exceeded")
	// ErrInvalidBudget reports invalid limits or an invalid reservation input.
	ErrInvalidBudget = errors.New("invalid generation budget or reservation")
)

// BudgetLimits are explicit assembly policy; this package supplies no defaults.
// Token quotas refer to EstimateTokens, not the provider's tokenizer or billing.
type BudgetLimits struct {
	InputTokensPerStep  int
	InputTokensTotal    int
	HistoryTokens       int
	RequestBytesPerStep int
	RequestBytesTotal   int
	MaxSteps            int
	MaxFunctions        int
}

// Budget atomically reserves one generation's requests and external function calls.
// Reservations remain consumed after an uncertain network outcome. Do not copy a Budget.
type Budget struct {
	mu           sync.Mutex
	limits       BudgetLimits
	steps        int
	functions    int
	inputTokens  int
	requestBytes int
}

// NewBudget validates independent byte and estimated-token quotas.
func NewBudget(limits BudgetLimits) (*Budget, error) {
	if limits.InputTokensPerStep <= 0 || limits.InputTokensTotal <= 0 || limits.HistoryTokens <= 0 || limits.RequestBytesPerStep <= 0 || limits.RequestBytesTotal <= 0 || limits.MaxSteps <= 0 || limits.MaxFunctions <= 0 || limits.InputTokensPerStep > limits.InputTokensTotal || limits.RequestBytesPerStep > limits.RequestBytesTotal || limits.HistoryTokens > limits.InputTokensPerStep {
		return nil, ErrInvalidBudget
	}
	return &Budget{limits: limits}, nil
}

// EstimateTokens is a transparent baseline: ceil(UTF-8 byte length / 4) over
// the full serialized request, including JSON syntax, whitespace and escapes.
// It is neither an Alice tokenizer nor a cost estimate; only byte quotas are strict.
func EstimateTokens(raw []byte) int {
	tokens := len(raw) / 4
	if len(raw)%4 != 0 {
		tokens++
	}
	return tokens
}

// ReserveRequest consumes a step, actual serialized bytes and estimated tokens
// together, before sending. A rejected request consumes none of these quotas.
func (b *Budget) ReserveRequest(rawJSON []byte) error {
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(rawJSON) || !json.Valid(rawJSON) {
		return ErrInvalidBudget
	}
	requestBytes, inputTokens := len(rawJSON), EstimateTokens(rawJSON)
	b.mu.Lock()
	defer b.mu.Unlock()
	// Subtraction keeps comparisons and subsequent increments safe even when
	// callers configure limits at the largest representable int.
	if b.steps >= b.limits.MaxSteps || requestBytes > b.limits.RequestBytesPerStep || requestBytes > b.limits.RequestBytesTotal-b.requestBytes || inputTokens > b.limits.InputTokensPerStep || inputTokens > b.limits.InputTokensTotal-b.inputTokens {
		return ErrBudgetExceeded
	}
	b.steps++
	b.requestBytes += requestBytes
	b.inputTokens += inputTokens
	return nil
}

// ReserveFunctions reserves an entire batch before its external calls start.
// Built-in provider search is not an external function and must not be passed here.
func (b *Budget) ReserveFunctions(n int) error {
	if n < 0 {
		return ErrInvalidBudget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limits.MaxFunctions-b.functions {
		return ErrBudgetExceeded
	}
	b.functions += n
	return nil
}

// RemainingInputBytes is the next request's allowance, capped by both its step
// quota and the total remaining bytes; no steps remaining means zero.
func (b *Budget) RemainingInputBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.steps >= b.limits.MaxSteps {
		return 0
	}
	return min(b.limits.RequestBytesPerStep, b.limits.RequestBytesTotal-b.requestBytes)
}

// RemainingInputTokens is the next request's estimated-token allowance. It does
// not relax the independent byte guard or reserve room for the request's history.
func (b *Budget) RemainingInputTokens() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.steps >= b.limits.MaxSteps {
		return 0
	}
	return min(b.limits.InputTokensPerStep, b.limits.InputTokensTotal-b.inputTokens)
}
