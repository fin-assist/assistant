// Package dialogue owns reading the assistant's saved dialogue and observing answers.
package dialogue

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound hides absent, replaced and foreign answers behind the same result.
var ErrNotFound = errors.New("answer not found")

// ErrUnavailable means the answer notification transport is unavailable.
var ErrUnavailable = errors.New("answer observation unavailable")

// Block is the stored shared-catalog form. Text is separate from non-text Body.
type Block struct {
	Type    string
	Version int32
	Text    string
	Body    json.RawMessage
}

// Source records where the figures came from. ChangedSince is calculated at read time.
type Source struct {
	CalculatedAt time.Time
	From, To     *time.Time
	TransferMode string
	ChangedSince []string
}

// Answer is one active saved answer and its user's current timezone.
type Answer struct {
	ID                string
	CreatedAt         time.Time
	Status, ErrorCode string
	Retryable         *bool
	Charged           bool
	Currency          string
	Blocks            []Block
	Chips             []json.RawMessage
	Source            *Source
	TimeZone          string
}

// Message is a question or an active answer; exactly one of Answer and Question is used.
type Message struct {
	ID        string
	CreatedAt time.Time
	Question  string
	Answer    *Answer
}

// Page is a bounded page of messages, oldest first.
type Page struct {
	Messages []Message
	HasOlder bool
	TimeZone string
}

// Version contains the latest data changes used to mark stale sources.
type Version struct{ Categories, Uploads *time.Time }
