// Package aistudio implements the bounded, stateless Responses HTTP transport.
// It does not own dialogue persistence, consent, retries, or the generation loop.
package aistudio

import (
	"context"
	"encoding/json"
)

// Limits bounds decoded data independently of the model's token budget.
// The assembly must supply explicit positive values; the transport has no policy defaults.
type Limits struct {
	RequestBytes  int64
	ResponseBytes int64
	EventBytes    int
	ArgumentBytes int
	OutputItems   int
}

// Request is one model step. Input items include the full context managed by the caller;
// no server-side conversation or previous_response_id is used.
type Request struct {
	Instructions    string            `json:"instructions"`
	Input           []json.RawMessage `json:"input"`
	Tools           []json.RawMessage `json:"tools,omitempty"`
	MaxOutputTokens int               `json:"max_output_tokens"`
	// MaxToolCalls bounds built-in tools for this HTTP step, not application functions.
	MaxToolCalls int `json:"max_tool_calls"`
}

// Usage preserves integer token counters, including AI Studio's tool tokens.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	ToolTokens   int64 `json:"tool_tokens"`
}

// Item is a completed output item, including its original JSON for the next model step.
// Raw is untrusted provider data; callers must validate tool arguments before execution.
type Item struct {
	ID        string
	Type      string
	CallID    string
	Name      string
	Arguments string
	Raw       json.RawMessage
}

// Result is returned only after a complete, validated terminal response.
type Result struct {
	ID     string
	Output []Item
	Usage  Usage
}

// Delta is an incremental output text fragment. Indexes are local to this model step.
// A partial callback is not evidence that a response completed successfully.
type Delta struct {
	OutputIndex  int
	ContentIndex int
	Text         string
}

// KeySource obtains a protected key for each request, allowing file rotation.
type KeySource interface {
	Key(context.Context) (string, error)
}
