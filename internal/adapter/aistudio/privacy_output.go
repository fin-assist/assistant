package aistudio

import (
	"encoding/json"
	"strings"
)

type outputPart struct{ output, content int }
type outputChunk struct {
	delta Delta
	ready bool
}

// outputRestorer holds at most one proper token prefix per part. Ready chunks
// wait behind that prefix to preserve callback order, bounded by ResponseBytes.
type outputRestorer struct {
	values                    map[string]privacyReplacement
	pending                   map[outputPart]*outputChunk
	context                   map[outputPart]string
	queue                     []*outputChunk
	emit                      func(Delta) error
	limit, produced, buffered int64
}

// Label-only requisites keep their exact value for later text search. The label
// makes restored history recognizable without retaining any private mapping.
func outputDisplayValue(replacement privacyReplacement, prefix string) string {
	value := replacement.value
	if digits(value) != value {
		return value
	}
	switch {
	case strings.HasPrefix(replacement.token, "[phone_") && len(value) == 11:
		if labelBefore(phoneLabel, prefix, len(prefix)) {
			return value
		}
		return "телефон " + value
	case strings.HasPrefix(replacement.token, "[account_") && len(value) == 20:
		if labelBefore(accountLabel, prefix, len(prefix)) {
			return value
		}
		return "счёт " + value
	case strings.HasPrefix(replacement.token, "[card_") && len(value) >= 13 && len(value) <= 19:
		if labelBefore(cardLabel, prefix, len(prefix)) {
			return value
		}
		return "карта " + value
	}
	return value
}

func newOutputRestorer(replacements []privacyReplacement, limit int64, emit func(Delta) error) *outputRestorer {
	values := make(map[string]privacyReplacement)
	for _, r := range replacements {
		if _, seen := values[r.token]; !seen {
			values[r.token] = r
		}
	}
	return &outputRestorer{values: values, pending: make(map[outputPart]*outputChunk), context: make(map[outputPart]string), emit: emit, limit: limit}
}
func outputContext(text string) string {
	if len(text) > 64 {
		return strings.Clone(text[len(text)-64:])
	}
	return text
}

func (r *outputRestorer) restore(text string, budget int64) (string, error) {
	result, _, err := r.restoreWithContext(text, budget, "")
	return result, err
}
func (r *outputRestorer) restoreWithContext(text string, budget int64, prefix string) (string, string, error) {
	var result strings.Builder
	at := 0
	write := func(value string) error {
		if int64(len(value)) > budget {
			return errPrivacy
		}
		budget -= int64(len(value))
		result.WriteString(value)
		prefix = outputContext(prefix + value)
		return nil
	}
	for _, span := range aliasPattern.FindAllStringIndex(text, -1) {
		replacement, known := r.values[text[span[0]:span[1]]]
		if !known {
			continue
		}
		if err := write(text[at:span[0]]); err != nil {
			return "", "", err
		}
		if err := write(outputDisplayValue(replacement, prefix)); err != nil {
			return "", "", err
		}
		at = span[1]
	}
	if at == 0 {
		if int64(len(text)) > budget {
			return "", "", errPrivacy
		}
		return text, outputContext(prefix + text), nil
	}
	if err := write(text[at:]); err != nil {
		return "", "", err
	}
	return result.String(), prefix, nil
}
func (r *outputRestorer) split(text string) (string, string) {
	start := strings.LastIndexByte(text, '[')
	if start >= 0 {
		suffix := text[start:]
		for token := range r.values {
			if len(suffix) < len(token) && strings.HasPrefix(token, suffix) {
				return text[:start], strings.Clone(suffix)
			}
		}
	}
	return text, ""
}
func (r *outputRestorer) append(delta Delta, ready bool) (*outputChunk, error) {
	if int64(len(delta.Text)) > r.limit-r.produced-r.buffered {
		return nil, errPrivacy
	}
	chunk := &outputChunk{delta: delta, ready: ready}
	r.queue = append(r.queue, chunk)
	r.buffered += int64(len(delta.Text))
	return chunk, nil
}
func (r *outputRestorer) replace(chunk *outputChunk, text string, ready bool) error {
	available := r.limit - r.produced - r.buffered + int64(len(chunk.delta.Text))
	if int64(len(text)) > available {
		return errPrivacy
	}
	r.buffered += int64(len(text)) - int64(len(chunk.delta.Text))
	chunk.delta.Text = text
	chunk.ready = ready
	return nil
}
func (r *outputRestorer) flush() error {
	for len(r.queue) > 0 && r.queue[0].ready {
		chunk := r.queue[0]
		r.queue[0] = nil
		r.queue = r.queue[1:]
		r.buffered -= int64(len(chunk.delta.Text))
		r.produced += int64(len(chunk.delta.Text))
		if err := r.emit(chunk.delta); err != nil {
			return err
		}
	}
	return nil
}
func (r *outputRestorer) delta(delta Delta) error {
	if len(r.values) == 0 {
		return r.emit(delta)
	}
	key := outputPart{delta.OutputIndex, delta.ContentIndex}
	previous := r.pending[key]
	text := delta.Text
	available := r.limit - r.produced - r.buffered
	if previous != nil {
		text = previous.delta.Text + text
		available += int64(len(previous.delta.Text))
	}
	head, tail := r.split(text)
	restored, prefix, err := r.restoreWithContext(head, available, r.context[key])
	if err != nil {
		return err
	}
	r.context[key] = prefix
	if previous != nil && head == "" && tail != "" {
		return r.replace(previous, tail, false)
	}
	if previous != nil {
		if err = r.replace(previous, restored, true); err != nil {
			return err
		}
		delete(r.pending, key)
	} else if restored != "" || tail == "" {
		delta.Text = restored
		if _, err = r.append(delta, true); err != nil {
			return err
		}
	}
	if tail != "" {
		delta.Text = tail
		chunk, err := r.append(delta, false)
		if err != nil {
			return err
		}
		r.pending[key] = chunk
	}
	return r.flush()
}
func (r *outputRestorer) done(delta Delta) error {
	key := outputPart{delta.OutputIndex, delta.ContentIndex}
	if chunk := r.pending[key]; chunk != nil {
		chunk.ready = true
		delete(r.pending, key)
	}
	return r.flush()
}
func (r *outputRestorer) finish() error {
	if len(r.pending) != 0 {
		return errPrivacy
	}
	return r.flush()
}

// The provider parser validates its original bytes first. Only confirmed message
// text is then restored locally, keeping final Raw identical to emitted text.
func (r *outputRestorer) result(result Result) (Result, error) {
	if len(r.values) == 0 {
		return result, nil
	}
	var total int64
	for i, item := range result.Output {
		if item.Type != "message" {
			continue
		}
		var fields map[string]json.RawMessage
		if decodeProviderJSON(item.Raw, &fields) != nil || fields == nil {
			return Result{}, errPrivacy
		}
		var content []json.RawMessage
		if json.Unmarshal(fields["content"], &content) != nil {
			return Result{}, errPrivacy
		}
		changed := false
		for j, raw := range content {
			var part map[string]json.RawMessage
			if decodeProviderJSON(raw, &part) != nil || part == nil {
				return Result{}, errPrivacy
			}
			var text string
			if json.Unmarshal(part["text"], &text) != nil {
				return Result{}, errPrivacy
			}
			restored, err := r.restore(text, r.limit-total)
			if err != nil {
				return Result{}, err
			}
			total += int64(len(restored))
			if restored != text {
				part["text"], _ = json.Marshal(restored)
				content[j], _ = json.Marshal(part)
				changed = true
			}
		}
		if changed {
			fields["content"], _ = json.Marshal(content)
			result.Output[i].Raw, _ = json.Marshal(fields)
		}
	}
	if total != r.produced {
		return Result{}, errPrivacy
	}
	return result, nil
}
