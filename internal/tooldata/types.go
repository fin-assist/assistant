// Package tooldata declares the data boundary consumed by assistant tools.
// Provider messages and transport errors stay outside this package.
package tooldata

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fin-assist/assistant/internal/datarevision"
)

var (
	// ErrInvalidArguments reports an invalid server subject or tool argument.
	ErrInvalidArguments = errors.New("invalid financial tool arguments")
	// ErrUnavailable reports an unavailable or failed data dependency.
	ErrUnavailable = errors.New("financial data unavailable")
	// ErrInvalidResponse reports data that cannot safely be used as evidence.
	ErrInvalidResponse = errors.New("invalid financial data response")
	// ErrResultTooLarge reports a result exceeding an explicit assembly quota.
	ErrResultTooLarge = errors.New("financial data result exceeds its limit")
)

// Subject is established by the server, never by model arguments.
type Subject struct{ UserID, TransferMode string }

// Filter selects operations; date bounds form a half-open interval.
type Filter struct {
	From, To                     *time.Time
	Query, CategoryID, Selection string
	Kind                         string
	AmountFrom, AmountTo         *int64
}

// AggregateArgs selects a bounded set of aggregate groups.
type AggregateArgs struct {
	Filter  Filter
	AllTime bool
	GroupBy []string
	Limit   int
}

// FindArgs selects a bounded list of operations.
type FindArgs struct {
	Filter  Filter
	AllTime bool
	Order   string
	Limit   int
}

// AnalyticsArgs selects a calendar period, in the subject's transfer mode.
type AnalyticsArgs struct{ Period, PeriodKey string }

// PeriodsArgs selects the type of available calendar periods.
type PeriodsArgs struct{ Period string }

// Range is a half-open interval; it never represents DataBounds.data_to.
type Range struct{ Start, End time.Time }

// Version identifies exact source revisions; wall clocks are not evidence.
type Version struct {
	Revisions datarevision.Current
}

// Source preserves metadata supplied by a data service, without inventing missing dates.
type Source struct {
	Range        *Range
	CalculatedAt *time.Time
	TimeZone     string
	Version      Version
}

// Evidence contains facts for server policy. It does not decide whether an answer is charged.
type Evidence struct {
	// Revisions are private provenance even when a no-data reply has no renderable source.
	Revisions   datarevision.Current
	NoData      bool
	LockedPaths []string
	Truncated   bool
	Source      *Source
}

// Result separates the bounded model payload from trusted rendering facts and policy evidence.
type Result struct {
	JSON       json.RawMessage
	Evidence   Evidence
	Overview   *Overview
	Aggregate  *Aggregate
	Operations *Operations
	Analytics  *Analytics
}

// Data reads financial information without a user token or model-provided identity.
type Data interface {
	Overview(context.Context, Subject) (Result, error)
	Aggregate(context.Context, Subject, AggregateArgs) (Result, error)
	Find(context.Context, Subject, FindArgs) (Result, error)
	Analytics(context.Context, Subject, AnalyticsArgs) (Result, error)
	Periods(context.Context, Subject, PeriodsArgs) (Result, error)
}
