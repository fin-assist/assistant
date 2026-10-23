package aistudio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

var errStream = errors.New("AI Studio returned an invalid response stream")

// ErrRefused reports an explicit model refusal without exposing provider text.
var ErrRefused = errors.New("AI Studio refused the response")

type streamEvent struct {
	Type         string          `json:"type"`
	Sequence     *int64          `json:"sequence_number"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	ItemID       string          `json:"item_id"`
	Delta        *string         `json:"delta"`
	Text         *string         `json:"text"`
	Arguments    *string         `json:"arguments"`
	Name         string          `json:"name"`
	Item         json.RawMessage `json:"item"`
	Part         json.RawMessage `json:"part"`
	Response     json.RawMessage `json:"response"`
}

type wireItem struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Role      string            `json:"role"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Content   []json.RawMessage `json:"content"`
}

type wirePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type streamPart struct {
	text     []byte
	textDone bool
	done     bool
}

type streamItem struct {
	initial       wireItem
	parts         []streamPart
	arguments     []byte
	argumentsDone bool
	done          json.RawMessage
}

type responseStream struct {
	limits       Limits
	emit         func(Delta) error
	id           string
	progress     bool
	completed    bool
	ended        bool
	lastSequence int64
	sequenceSeen bool
	items        []streamItem
	result       Result
}

// parseStream accepts a successful terminal response only after validating all
// streamed fragments against its final output. Partial text is never a result.
func parseStream(ctx context.Context, r io.Reader, limits Limits, emit func(Delta) error) (Result, error) {
	if limits.ResponseBytes <= 0 || limits.EventBytes <= 0 || limits.ArgumentBytes <= 0 || limits.OutputItems <= 0 {
		return Result{}, errStream
	}
	bounded := &io.LimitedReader{R: r, N: limits.ResponseBytes}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, min(4096, limits.EventBytes)), limits.EventBytes)
	s := responseStream{limits: limits, emit: emit}
	var data []byte
	var eventName string
	eventSize := 0
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if !scanner.Scan() {
			break
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		line := scanner.Bytes()
		eventSize += len(line) + 1
		if eventSize > limits.EventBytes {
			return Result{}, errStream
		}
		if len(line) == 0 {
			if len(data) > 0 {
				if err := s.event(eventName, data[:len(data)-1]); err != nil {
					return Result{}, err
				}
			}
			data = data[:0]
			eventName = ""
			eventSize = 0
			continue
		}
		if line[0] == ':' {
			continue
		}
		key, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(key) {
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
		case "event":
			if eventName != "" {
				return Result{}, errStream
			}
			eventName = string(value)
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if scanner.Err() != nil || len(data) != 0 || !s.completed {
		return Result{}, errStream
	}
	// A LimitedReader reports EOF at its cap; probe one byte so an exact-sized
	// response succeeds while a larger body cannot masquerade as a completed one.
	if bounded.N == 0 {
		var extra [1]byte
		n, err := r.Read(extra[:])
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if n != 0 || err != io.EOF {
			return Result{}, errStream
		}
	}
	return s.result, nil
}

func (s *responseStream) event(name string, data []byte) error {
	if bytes.Equal(data, []byte("[DONE]")) {
		if !s.completed || s.ended {
			return errStream
		}
		s.ended = true
		return nil
	}
	var e streamEvent
	if !utf8.Valid(data) || decodeJSON(data, &e) != nil || e.Type == "" || (name != "" && name != e.Type) || s.completed {
		return errStream
	}
	if e.Sequence != nil {
		if *e.Sequence < 0 || (s.sequenceSeen && *e.Sequence <= s.lastSequence) {
			return errStream
		}
		s.sequenceSeen, s.lastSequence = true, *e.Sequence
	}
	switch e.Type {
	case "response.created":
		if s.id != "" {
			return errStream
		}
		var r struct {
			ID, Status string
			Output     []json.RawMessage
		}
		if decodeJSON(e.Response, &r) != nil || r.ID == "" || (r.Status != "queued" && r.Status != "in_progress") || len(r.Output) != 0 {
			return errStream
		}
		s.id = r.ID
		return nil
	case "response.failed", "response.incomplete", "error":
		return errStream
	}
	if s.id == "" {
		return errStream
	}
	switch e.Type {
	case "response.refusal.delta", "response.refusal.done":
		return ErrRefused
	case "response.in_progress":
		var r struct{ ID, Status string }
		if s.progress || decodeJSON(e.Response, &r) != nil || r.ID != s.id || r.Status != "in_progress" {
			return errStream
		}
		s.progress = true
		return nil
	case "response.output_item.added":
		return s.addItem(e)
	case "response.output_item.done":
		return s.finishItem(e)
	case "response.content_part.added", "response.content_part.done", "response.output_text.delta", "response.output_text.done":
		return s.textEvent(e)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return s.argumentEvent(e)
	case "response.completed":
		return s.finish(e.Response)
	default:
		// These events describe provider-owned reasoning/search progress or
		// annotations. The completed raw items retain their authoritative data.
		if strings.HasPrefix(e.Type, "response.reasoning_") || strings.HasPrefix(e.Type, "response.web_search_call.") || e.Type == "response.output_text.annotation.added" || e.Type == "rate_limits.updated" || e.Type == "ping" {
			return nil
		}
		return errStream
	}
}

func (s *responseStream) addItem(e streamEvent) error {
	if e.OutputIndex == nil || *e.OutputIndex != len(s.items) || len(s.items) >= s.limits.OutputItems {
		return errStream
	}
	var item wireItem
	if decodeJSON(e.Item, &item) != nil || item.ID == "" || !supportedItem(item.Type) || (item.Status != "" && item.Status != "in_progress") {
		return errStream
	}
	for _, previous := range s.items {
		if previous.initial.ID == item.ID || (item.CallID != "" && previous.initial.CallID == item.CallID) {
			return errStream
		}
	}
	if item.Type == "message" && (item.Role != "assistant" || len(item.Content) != 0) {
		return errStream
	}
	if item.Type == "function_call" && (item.CallID == "" || item.Name == "" || item.Arguments != "") {
		return errStream
	}
	s.items = append(s.items, streamItem{initial: item})
	return nil
}

func supportedItem(kind string) bool {
	return kind == "message" || kind == "function_call" || kind == "reasoning" || kind == "web_search_call"
}

func (s *responseStream) item(e streamEvent) (*streamItem, error) {
	if e.OutputIndex == nil || *e.OutputIndex < 0 || *e.OutputIndex >= len(s.items) {
		return nil, errStream
	}
	item := &s.items[*e.OutputIndex]
	if item.done != nil || e.ItemID != item.initial.ID {
		return nil, errStream
	}
	return item, nil
}

func (s *responseStream) textEvent(e streamEvent) error {
	item, err := s.item(e)
	if err != nil || item.initial.Type != "message" || e.ContentIndex == nil || *e.ContentIndex < 0 {
		return errStream
	}
	index := *e.ContentIndex
	if e.Type == "response.content_part.added" {
		var part wirePart
		if index != len(item.parts) || decodeJSON(e.Part, &part) != nil {
			return errStream
		}
		if part.Type == "refusal" {
			return ErrRefused
		}
		if part.Type != "output_text" || part.Text != "" {
			return errStream
		}
		item.parts = append(item.parts, streamPart{})
		return nil
	}
	if index >= len(item.parts) {
		return errStream
	}
	part := &item.parts[index]
	if part.done {
		return errStream
	}
	switch e.Type {
	case "response.output_text.delta":
		if part.textDone || e.Delta == nil {
			return errStream
		}
		part.text = append(part.text, *e.Delta...)
		if s.emit != nil {
			return s.emit(Delta{OutputIndex: *e.OutputIndex, ContentIndex: index, Text: *e.Delta})
		}
	case "response.output_text.done":
		if part.textDone || e.Text == nil || *e.Text != string(part.text) {
			return errStream
		}
		part.textDone = true
	case "response.content_part.done":
		var final wirePart
		if decodeJSON(e.Part, &final) != nil {
			return errStream
		}
		if final.Type == "refusal" {
			return ErrRefused
		}
		if !part.textDone || final.Type != "output_text" || final.Text != string(part.text) {
			return errStream
		}
		part.done = true
	}
	return nil
}

func (s *responseStream) argumentEvent(e streamEvent) error {
	item, err := s.item(e)
	if err != nil || item.initial.Type != "function_call" || item.argumentsDone {
		return errStream
	}
	if e.Type == "response.function_call_arguments.delta" {
		if e.Delta == nil || len(*e.Delta) > s.limits.ArgumentBytes-len(item.arguments) {
			return errStream
		}
		item.arguments = append(item.arguments, *e.Delta...)
		return nil
	}
	if e.Arguments == nil || *e.Arguments != string(item.arguments) || len(*e.Arguments) > s.limits.ArgumentBytes || (e.Name != "" && e.Name != item.initial.Name) {
		return errStream
	}
	item.argumentsDone = true
	return nil
}

func (s *responseStream) finishItem(e streamEvent) error {
	if e.OutputIndex == nil || *e.OutputIndex < 0 || *e.OutputIndex >= len(s.items) {
		return errStream
	}
	item := &s.items[*e.OutputIndex]
	var final wireItem
	if item.done != nil || decodeJSON(e.Item, &final) != nil || final.ID != item.initial.ID || final.Type != item.initial.Type || (final.Status != "" && final.Status != "completed") {
		return errStream
	}
	switch final.Type {
	case "function_call":
		if !item.argumentsDone || final.CallID != item.initial.CallID || final.Name != item.initial.Name || final.Arguments != string(item.arguments) || len(final.Arguments) > s.limits.ArgumentBytes {
			return errStream
		}
	case "message":
		if final.Status != "completed" || final.Role != "assistant" {
			return errStream
		}
		for i, raw := range final.Content {
			var part wirePart
			if decodeJSON(raw, &part) != nil {
				return errStream
			}
			if part.Type == "refusal" {
				return ErrRefused
			}
			if i >= len(item.parts) || !item.parts[i].done || part.Type != "output_text" || part.Text != string(item.parts[i].text) {
				return errStream
			}
		}
		if len(final.Content) != len(item.parts) {
			return errStream
		}
	}
	item.done = bytes.Clone(e.Item)
	return nil
}

func (s *responseStream) finish(raw json.RawMessage) error {
	var response struct {
		ID         string            `json:"id"`
		Status     string            `json:"status"`
		Output     []json.RawMessage `json:"output"`
		Usage      *Usage            `json:"usage"`
		Error      json.RawMessage   `json:"error"`
		Incomplete json.RawMessage   `json:"incomplete_details"`
	}
	if decodeJSON(raw, &response) != nil || response.ID != s.id || response.Status != "completed" || response.Output == nil || len(response.Output) != len(s.items) || response.Usage == nil || !nullJSON(response.Error) || !nullJSON(response.Incomplete) {
		return errStream
	}
	u := *response.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.TotalTokens < 0 || u.ToolTokens < 0 {
		return errStream
	}
	result := Result{ID: response.ID, Usage: u, Output: make([]Item, 0, len(response.Output))}
	for index, rawItem := range response.Output {
		var item wireItem
		if s.items[index].done == nil || !equalJSON(rawItem, s.items[index].done) || decodeJSON(rawItem, &item) != nil {
			return errStream
		}
		result.Output = append(result.Output, Item{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Raw: bytes.Clone(rawItem)})
	}
	s.result, s.completed = result, true
	return nil
}

func nullJSON(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func equalJSON(a, b json.RawMessage) bool {
	var first, second any
	return decodeJSON(a, &first) == nil && decodeJSON(b, &second) == nil && reflect.DeepEqual(first, second)
}
