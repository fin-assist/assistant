package aistudio

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParserModerationRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, id, status, reason string
		started, refused         bool
	}{
		{"moderated before text", "r", "incomplete", "content_filter", true, true},
		{"token exhaustion", "r", "incomplete", "max_output_tokens", true, false},
		{"unknown reason", "r", "incomplete", "private-provider-reason", true, false},
		{"missing reason", "r", "incomplete", "", true, false},
		{"wrong ID", "other", "incomplete", "content_filter", true, false},
		{"wrong status", "r", "completed", "content_filter", true, false},
		{"missing creation", "r", "incomplete", "content_filter", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []map[string]any{}
			if tc.started {
				events = append(events, parserCreated())
			}
			events = append(events, parserEvent("response.incomplete", map[string]any{"response": map[string]any{"id": tc.id, "status": tc.status, "incomplete_details": map[string]any{"reason": tc.reason}, "output": []any{}}}))
			result, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), nil)
			if err == nil || errors.Is(err, ErrRefused) != tc.refused || result.ID != "" {
				t.Fatalf("result=%+v err=%v refused=%v", result, err, tc.refused)
			}
		})
	}
	var fragments int
	events := parserTextEvents()[:6]
	events = append(events, parserEvent("response.incomplete", map[string]any{"response": map[string]any{"id": "r", "status": "incomplete", "incomplete_details": map[string]any{"reason": "content_filter"}}}))
	result, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), func(Delta) error { fragments++; return nil })
	if !errors.Is(err, ErrRefused) || fragments != 2 || len(result.Output) != 0 {
		t.Fatalf("partial moderation err=%v fragments=%d result=%+v", err, fragments, result)
	}
	for _, raw := range []string{
		`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":null}}`,
		`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","error":{"code":"failure"},"incomplete_details":{"reason":"content_filter"}}}`,
		`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter","reason":"max_output_tokens"}}}`,
	} {
		body := parserBody([]map[string]any{parserCreated()}) + "data: " + raw + "\n\n"
		if _, err := parseStream(context.Background(), strings.NewReader(body), parserLimits(), nil); !errors.Is(err, errStream) {
			t.Fatalf("malformed moderation accepted: %v", err)
		}
	}
}
