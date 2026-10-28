package aistudio

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var aliasPattern = regexp.MustCompile(`\[(?:phone|email|card|account)_[0-9a-f]{24}\]`)

// Only declared text/search fields get context-free known-value replacement.
// In particular, protojson int64 financial values are strings too, not text.
func modelTextField(field string) bool {
	switch field {
	case "text", "content", "instructions", "description", "title", "name", "label", "q", "selection_name", "type_name", "mask", "note", "note_details", "bank_category", "source_name", "alt_text", "empty_text", "message", "summary", "output":
		return true
	}
	return false
}

type knownSpan struct {
	start, end int
	token      string
}

func (f *outboundRedactor) knownText(text string) string {
	var candidates []knownSpan
	protected := aliasPattern.FindAllStringIndex(text, -1)
	seen := map[string]bool{}
	for _, replacement := range f.replacements {
		if seen[replacement.value] {
			continue
		}
		seen[replacement.value] = true
		for offset := 0; offset < len(text); {
			relative := strings.Index(text[offset:], replacement.value)
			if relative < 0 {
				break
			}
			start := offset + relative
			end := start + len(replacement.value)
			offset = end
			if !digitBoundary(text, start, end) {
				continue
			}
			inside := false
			for _, span := range protected {
				if start < span[1] && end > span[0] {
					inside = true
					break
				}
			}
			if !inside {
				candidates = append(candidates, knownSpan{start, end, replacement.token})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start == candidates[j].start {
			return candidates[i].end > candidates[j].end
		}
		return candidates[i].start < candidates[j].start
	})
	var result strings.Builder
	at := 0
	for _, span := range candidates {
		if span.start < at {
			continue
		}
		result.WriteString(text[at:span.start])
		result.WriteString(span.token)
		at = span.end
	}
	if at == 0 {
		return text
	}
	result.WriteString(text[at:])
	return result.String()
}
func (f *outboundRedactor) knownValue(value any, field string, depth int) (any, error) {
	if depth > 64 {
		return nil, errPrivacy
	}
	switch v := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, item := range v {
			if text, ok := item.(string); ok && (key == "arguments" || key == "output") && (strings.HasPrefix(strings.TrimSpace(text), "{") || strings.HasPrefix(strings.TrimSpace(text), "[")) {
				raw, err := f.knownJSON([]byte(text), depth+1)
				if err != nil {
					return nil, err
				}
				result[key] = string(raw)
			} else {
				filtered, err := f.knownValue(item, key, depth+1)
				if err != nil {
					return nil, err
				}
				result[key] = filtered
			}
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			filtered, err := f.knownValue(item, field, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = filtered
		}
		return result, nil
	case string:
		if modelTextField(field) {
			return f.knownText(v), nil
		}
		return v, nil
	default:
		return value, nil
	}
}
func (f *outboundRedactor) knownJSON(raw []byte, depth int) ([]byte, error) {
	var value any
	if decodeJSON(raw, &value) != nil {
		return nil, errPrivacy
	}
	filtered, err := f.knownValue(value, "", depth)
	if err != nil {
		return nil, err
	}
	if reflect.DeepEqual(value, filtered) {
		return append([]byte(nil), raw...), nil
	}
	result, err := json.Marshal(filtered)
	if err != nil {
		return nil, errPrivacy
	}
	return result, nil
}
func (f *outboundRedactor) knownRequest(request Request) (Request, error) {
	result := request
	result.Instructions = f.knownText(request.Instructions)
	result.Input = make([]json.RawMessage, len(request.Input))
	result.Tools = make([]json.RawMessage, len(request.Tools))
	for i, raw := range request.Input {
		filtered, err := f.knownJSON(raw, 0)
		if err != nil {
			return Request{}, err
		}
		result.Input[i] = filtered
	}
	for i, raw := range request.Tools {
		filtered, err := f.knownJSON(raw, 0)
		if err != nil {
			return Request{}, err
		}
		result.Tools[i] = filtered
	}
	return result, nil
}
