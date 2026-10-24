package domain

import (
	"errors"
	"slices"
	"time"
)

// ErrInvalidOffTopicPolicy rejects an unusable admission policy or clock.
var ErrInvalidOffTopicPolicy = errors.New("invalid off-topic admission policy")

// OffTopicPolicy limits terminal outcomes since the last charged financial answer.
// It depends only on facts and instants, never on storage or the profile timezone.
type OffTopicPolicy struct {
	Max    int
	Window time.Duration
}

// OffTopicQuery bounds the recent facts needed to make an admission decision.
type OffTopicQuery struct {
	After  time.Time
	Latest int
}

// Query selects the strict expiry cutoff and the number of latest facts needed.
func (p OffTopicPolicy) Query(now time.Time) (OffTopicQuery, error) {
	if p.Max < 1 || p.Max > 32767 || p.Window < time.Microsecond || now.IsZero() {
		return OffTopicQuery{}, ErrInvalidOffTopicPolicy
	}
	return OffTopicQuery{After: now.Add(-p.Window), Latest: p.Max}, nil
}

// RetryAt returns zero while a fresh question is allowed. A fact expires exactly
// at completed_at+Window. With more than Max events, admission waits until fewer
// than Max remain, rather than merely waiting for the first expiry.
func (p OffTopicPolicy) RetryAt(now time.Time, completed []time.Time) (time.Time, error) {
	query, err := p.Query(now)
	if err != nil {
		return time.Time{}, err
	}
	recent := make([]time.Time, 0, len(completed))
	for _, at := range completed {
		if at.After(query.After) {
			recent = append(recent, at)
		}
	}
	if len(recent) < p.Max {
		return time.Time{}, nil
	}
	slices.SortFunc(recent, func(a, b time.Time) int { return b.Compare(a) })
	return recent[p.Max-1].Add(p.Window), nil
}

// OffTopicLedgerAction is a selected terminal effect; adapters only apply it.
type OffTopicLedgerAction uint8

const (
	// KeepOffTopic leaves the resettable ledger unchanged.
	KeepOffTopic OffTopicLedgerAction = iota
	// RecordOffTopic records one newly terminal off-topic answer.
	RecordOffTopic
	// ResetOffTopic clears the ledger after a charged financial completion.
	ResetOffTopic
)

// OffTopicLedgerEffect includes the diagnostic day's exact half-open bounds.
// The daily count uses retained answer history, including events before a reset.
type OffTopicLedgerEffect struct {
	Action           OffTopicLedgerAction
	DayStart, DayEnd time.Time
}

// PlanOffTopic selects a ledger effect from the normalized terminal outcome.
func PlanOffTopic(errorCode string, charged bool, at time.Time) OffTopicLedgerEffect {
	if errorCode == "" && charged {
		return OffTopicLedgerEffect{Action: ResetOffTopic}
	}
	if errorCode != "OFF_TOPIC" {
		return OffTopicLedgerEffect{Action: KeepOffTopic}
	}
	moscow := time.FixedZone("Europe/Moscow", 3*60*60)
	local := at.In(moscow)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, moscow)
	return OffTopicLedgerEffect{Action: RecordOffTopic, DayStart: start, DayEnd: start.AddDate(0, 0, 1)}
}
