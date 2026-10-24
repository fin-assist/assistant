package generation

import (
	"time"

	"github.com/fin-assist/assistant/internal/domain"
)

// Terminal contains the use case's selected terminal writes and ledger effect.
// The repository still owns the atomic generating-to-terminal CAS and applies a
// refund only against the original charge on the still-current daily counter.
type Terminal struct {
	Outcome             Outcome
	At                  time.Time
	Status              string
	Retryable           *bool
	RefundCurrentDay    bool
	PreserveBlockPrefix bool
	Ledger              domain.OffTopicLedgerEffect
}

// PrepareFinish normalizes terminal policy before any storage operation.
// Failures never preserve partial content or consume the reserved daily attempt.
func PrepareFinish(out Outcome, at time.Time) Terminal {
	terminal := Terminal{Status: "complete", At: at, PreserveBlockPrefix: out.ErrorCode == ""}
	if out.ErrorCode != "" {
		terminal.Status = "failed"
		canRetry := out.ErrorCode == "MODEL_ERROR"
		terminal.Retryable = &canRetry
		out.Charged = false
		out.Blocks, out.Chips, out.Source = nil, nil, nil
	}
	terminal.Outcome = out
	terminal.RefundCurrentDay = !out.Charged
	terminal.Ledger = domain.PlanOffTopic(out.ErrorCode, out.Charged, at)
	return terminal
}
