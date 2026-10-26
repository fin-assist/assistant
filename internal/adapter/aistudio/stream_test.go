package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func parserLimits() Limits {
	return Limits{ResponseBytes: 100_000, EventBytes: 10_000, ArgumentBytes: 1000, OutputItems: 10}
}

func parserEvent(kind string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["type"] = kind
	return fields
}

func parserBody(events []map[string]any) string {
	var b strings.Builder
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		b.WriteString("event: ")
		b.WriteString(event["type"].(string))
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	return b.String()
}

func parserCreated() map[string]any {
	return parserEvent("response.created", map[string]any{"response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}})
}

func parserCompleted(items ...any) map[string]any {
	if items == nil {
		items = []any{}
	}
	return parserEvent("response.completed", map[string]any{"response": map[string]any{"id": "r", "status": "completed", "output": items, "usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10, "tool_tokens": 2}}})
}

func parserTextEvents() []map[string]any {
	part := map[string]any{"type": "output_text", "text": "Привет 🌍", "annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.org", "title": "Source", "start_index": 0, "end_index": 6}}}
	item := map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	address := func(fields map[string]any) map[string]any {
		fields["output_index"] = 0
		fields["content_index"] = 0
		fields["item_id"] = "m"
		return fields
	}
	return []map[string]any{
		parserCreated(),
		parserEvent("response.in_progress", map[string]any{"response": map[string]any{"id": "r", "status": "in_progress"}}),
		parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}),
		parserEvent("response.content_part.added", address(map[string]any{"part": map[string]any{"type": "output_text", "text": ""}})),
		parserEvent("response.output_text.delta", address(map[string]any{"delta": "При"})),
		parserEvent("response.output_text.delta", address(map[string]any{"delta": "вет 🌍"})),
		parserEvent("response.output_text.done", address(map[string]any{"text": "Привет 🌍"})),
		parserEvent("response.content_part.done", address(map[string]any{"part": part})),
		parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": item}),
		parserCompleted(item),
	}
}

func TestUnitParserTextCitationsSSE(t *testing.T) {
	events := parserTextEvents()
	for i := range events {
		events[i]["sequence_number"] = i
	}
	body := parserBody(events)
	// JSON permits whitespace between members: SSE joins multiple data lines
	// with a newline, including comments and CRLF framing.
	body = strings.Replace(body, `,"sequence_number":`, ",\ndata: \"sequence_number\":", 1)
	body = ": keepalive\nid: ignored\nretry: 100\n\n" + body + "data: [DONE]\n\n"
	body = strings.ReplaceAll(body, "\n", "\r\n")
	var deltas []Delta
	result, err := parseStream(context.Background(), strings.NewReader(body), parserLimits(), func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "r" || result.Usage != (Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10, ToolTokens: 2}) || len(result.Output) != 1 || result.Output[0].Type != "message" {
		t.Fatalf("result: %+v", result)
	}
	if !reflect.DeepEqual(deltas, []Delta{{Text: "При"}, {Text: "вет 🌍"}}) {
		t.Fatalf("deltas: %+v", deltas)
	}
	if !strings.Contains(string(result.Output[0].Raw), `"url_citation"`) || !strings.Contains(string(result.Output[0].Raw), `"https://example.org"`) {
		t.Fatalf("lost citation: %s", result.Output[0].Raw)
	}
}

func TestUnitParserParallelFunctionsAndProviderItems(t *testing.T) {
	events := []map[string]any{parserCreated()}
	var final []any
	for i, id := range []string{"f0", "f1"} {
		item := map[string]any{"id": id, "type": "function_call", "call_id": "find", "name": "find", "arguments": "", "status": "in_progress"}
		events = append(events, parserEvent("response.output_item.added", map[string]any{"output_index": i, "item": item}))
	}
	for _, fragment := range []string{`{"q":`, `"рубли"}`} {
		for i, id := range []string{"f0", "f1"} {
			events = append(events, parserEvent("response.function_call_arguments.delta", map[string]any{"output_index": i, "item_id": id, "delta": fragment}))
		}
	}
	for i, id := range []string{"f0", "f1"} {
		arguments := `{"q":"рубли"}`
		events = append(events, parserEvent("response.function_call_arguments.done", map[string]any{"output_index": i, "item_id": id, "name": "find", "arguments": arguments}))
		item := map[string]any{"id": id, "type": "function_call", "call_id": "find", "name": "find", "arguments": arguments, "status": "completed"}
		events = append(events, parserEvent("response.output_item.done", map[string]any{"output_index": i, "item": item}))
		final = append(final, item)
	}
	for i, kind := range []string{"reasoning", "web_search_call"} {
		index := i + 2
		item := map[string]any{"id": kind, "type": kind, "status": "in_progress"}
		events = append(events, parserEvent("response.output_item.added", map[string]any{"output_index": index, "item": item}))
		telemetry := "response.reasoning_summary_text.delta"
		if kind == "web_search_call" {
			telemetry = "response.web_search_call.searching"
		}
		events = append(events, parserEvent(telemetry, map[string]any{"output_index": index, "item_id": kind, "delta": "private reasoning"}))
		done := map[string]any{"id": kind, "type": kind, "status": "completed", "opaque_provider_data": map[string]any{"value": json.Number("9007199254740993")}}
		events = append(events, parserEvent("response.output_item.done", map[string]any{"output_index": index, "item": done}))
		final = append(final, done)
	}
	events = append(events, parserCompleted(final...))
	calls := 0
	result, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), func(Delta) error { calls++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(result.Output) != 4 || result.Output[0].Arguments != `{"q":"рубли"}` || result.Output[0].CallID != "find" || result.Output[1].CallID != "find" || result.Output[0].ID == result.Output[1].ID || !strings.Contains(string(result.Output[2].Raw), "9007199254740993") {
		t.Fatalf("result: %+v; callbacks %d", result, calls)
	}
}

func TestUnitParserRejectsInvalidLifecycles(t *testing.T) {
	tests := map[string]func([]map[string]any) []map[string]any{
		"missing created":   func(e []map[string]any) []map[string]any { return e[1:] },
		"duplicate created": func(e []map[string]any) []map[string]any { return append([]map[string]any{e[0]}, e...) },
		"duplicate progress": func(e []map[string]any) []map[string]any {
			return append(e[:2], append([]map[string]any{e[1]}, e[2:]...)...)
		},
		"missing completion":   func(e []map[string]any) []map[string]any { return e[:len(e)-1] },
		"duplicate completion": func(e []map[string]any) []map[string]any { return append(e, e[len(e)-1]) },
		"duplicate item": func(e []map[string]any) []map[string]any {
			return append(e[:3], append([]map[string]any{e[2]}, e[3:]...)...)
		},
		"bad output index": func(e []map[string]any) []map[string]any { e[2]["output_index"] = -1; return e },
		"unknown item": func(e []map[string]any) []map[string]any {
			e[2]["item"].(map[string]any)["type"] = "file_search_call"
			return e
		},
		"foreign item":          func(e []map[string]any) []map[string]any { e[4]["item_id"] = "foreign"; return e },
		"no content index":      func(e []map[string]any) []map[string]any { delete(e[4], "content_index"); return e },
		"content gap":           func(e []map[string]any) []map[string]any { e[3]["content_index"] = 1; return e },
		"delta before content":  func(e []map[string]any) []map[string]any { return append(e[:3], e[4:]...) },
		"delta after text done": func(e []map[string]any) []map[string]any { e[5], e[6] = e[6], e[5]; return e },
		"text mismatch":         func(e []map[string]any) []map[string]any { e[6]["text"] = "secret"; return e },
		"part before text done": func(e []map[string]any) []map[string]any { e[6], e[7] = e[7], e[6]; return e },
		"item before part done": func(e []map[string]any) []map[string]any { return append(e[:7], e[8:]...) },
		"changed final output": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["output"] = []any{}
			return e
		},
		"wrong response ID": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["id"] = "foreign"
			return e
		},
		"incomplete status": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["status"] = "incomplete"
			return e
		},
		"missing usage": func(e []map[string]any) []map[string]any {
			delete(e[9]["response"].(map[string]any), "usage")
			return e
		},
		"negative usage": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["usage"].(map[string]any)["tool_tokens"] = -1
			return e
		},
		"error with completion": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["error"] = map[string]any{"message": "private"}
			return e
		},
		"fractional usage": func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["usage"].(map[string]any)["total_tokens"] = 1.5
			return e
		},
		"sequence regression": func(e []map[string]any) []map[string]any {
			e[0]["sequence_number"] = 3
			e[1]["sequence_number"] = 2
			return e
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseStream(context.Background(), strings.NewReader(parserBody(change(parserTextEvents()))), parserLimits(), nil)
			if !errors.Is(err, errStream) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error: %v", err)
			}
		})
	}
	for _, kind := range []string{"response.failed", "response.incomplete", "error", "response.unknown_critical"} {
		t.Run(kind, func(t *testing.T) {
			_, err := parseStream(context.Background(), strings.NewReader(parserBody([]map[string]any{parserCreated(), parserEvent(kind, map[string]any{"message": "PRIVATE"})})), parserLimits(), nil)
			if !errors.Is(err, errStream) {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestUnitParserBoundsFramingAndCancellation(t *testing.T) {
	body := parserBody(parserTextEvents())
	tests := map[string]struct {
		body   string
		limits Limits
	}{
		"bad JSON":                  {`data: {"type":"response.created",` + "\n\n", parserLimits()},
		"duplicate JSON member":     {`data: {"type":"response.created","type":"response.completed"}` + "\n\n", parserLimits()},
		"mismatched event name":     {strings.Replace(body, "event: response.created", "event: different", 1), parserLimits()},
		"unfinished event":          {strings.TrimSuffix(body, "\n\n"), parserLimits()},
		"premature done":            {"data: [DONE]\n\n", parserLimits()},
		"truncated line":            {body[:len(body)/2], parserLimits()},
		"malformed unicode":         {strings.Replace(body, "Привет", string([]byte{0xff}), 1), parserLimits()},
		"too many items":            {body, Limits{ResponseBytes: 100000, EventBytes: 10000, ArgumentBytes: 1000, OutputItems: 0}},
		"event cap":                 {body, Limits{ResponseBytes: 100000, EventBytes: 40, ArgumentBytes: 1000, OutputItems: 10}},
		"aggregate event cap":       {":" + strings.Repeat("x", 100) + "\n:" + strings.Repeat("x", 100) + "\n\n" + body, Limits{ResponseBytes: 100000, EventBytes: 150, ArgumentBytes: 1000, OutputItems: 10}},
		"body cap":                  {body, Limits{ResponseBytes: int64(len(body) - 1), EventBytes: 10000, ArgumentBytes: 1000, OutputItems: 10}},
		"body extra after terminal": {body + strings.Repeat(" ", 100), Limits{ResponseBytes: int64(len(body)), EventBytes: 10000, ArgumentBytes: 1000, OutputItems: 10}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseStream(context.Background(), strings.NewReader(test.body), test.limits, nil)
			if !errors.Is(err, errStream) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	limits := parserLimits()
	limits.ResponseBytes = int64(len(body))
	if _, err := parseStream(context.Background(), strings.NewReader(body), limits, nil); err != nil {
		t.Fatalf("exact cap: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseStream(ctx, strings.NewReader(body), parserLimits(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := parseStream(ctx, strings.NewReader(body), parserLimits(), func(Delta) error { cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation: %v", err)
	}
	callbackErr := errors.New("internal storage failure")
	if _, err := parseStream(context.Background(), strings.NewReader(body), parserLimits(), func(Delta) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("callback: %v", err)
	}
	if _, err := parseStream(context.Background(), parserBrokenReader{}, parserLimits(), nil); !errors.Is(err, errStream) {
		t.Fatalf("reader failure: %v", err)
	}
}

type parserBrokenReader struct{}

func (parserBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestUnitParserFunctionsRejectInvalidArguments(t *testing.T) {
	base := func() []map[string]any {
		return []map[string]any{
			parserCreated(),
			parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "f", "type": "function_call", "call_id": "c", "name": "find", "arguments": ""}}),
			parserEvent("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "f", "delta": "{}"}),
			parserEvent("response.function_call_arguments.done", map[string]any{"output_index": 0, "item_id": "f", "name": "find", "arguments": "{}"}),
			parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "f", "type": "function_call", "call_id": "c", "name": "find", "arguments": "{}"}}),
			parserCompleted(map[string]any{"id": "f", "type": "function_call", "call_id": "c", "name": "find", "arguments": "{}"}),
		}
	}
	for name, change := range map[string]func([]map[string]any){
		"argument done mismatch":  func(e []map[string]any) { e[3]["arguments"] = "different" },
		"name mismatch":           func(e []map[string]any) { e[3]["name"] = "different" },
		"missing delta":           func(e []map[string]any) { delete(e[2], "delta") },
		"missing argument done":   func(e []map[string]any) { e[3] = parserEvent("ping", nil) },
		"late delta":              func(e []map[string]any) { e[3], e[2] = e[2], e[3] },
		"changed final arguments": func(e []map[string]any) { e[4]["item"].(map[string]any)["arguments"] = "[]" },
		"missing call ID":         func(e []map[string]any) { delete(e[1]["item"].(map[string]any), "call_id") },
		"initial arguments":       func(e []map[string]any) { e[1]["item"].(map[string]any)["arguments"] = "{}" },
	} {
		t.Run(name, func(t *testing.T) {
			e := base()
			change(e)
			_, err := parseStream(context.Background(), strings.NewReader(parserBody(e)), parserLimits(), nil)
			if !errors.Is(err, errStream) {
				t.Fatalf("error %v", err)
			}
		})
	}
	limits := parserLimits()
	limits.ArgumentBytes = 1
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody(base())), limits, nil); !errors.Is(err, errStream) {
		t.Fatalf("argument bound: %v", err)
	}
	limits.ArgumentBytes = 2
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody(base())), limits, nil); err != nil {
		t.Fatalf("exact argument bound: %v", err)
	}
	// Streamed chunks are bounded in aggregate, not only per chunk.
	e := base()
	e[2]["delta"] = "{"
	e = append(e[:3], append([]map[string]any{parserEvent("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "f", "delta": "}"})}, e[3:]...)...)
	limits.ArgumentBytes = 1
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody(e)), limits, nil); !errors.Is(err, errStream) {
		t.Fatalf("accumulated bound: %v", err)
	}
}

func TestUnitParserItemIdentitiesLimitsAndFinalRaw(t *testing.T) {
	first := map[string]any{"id": "f", "type": "function_call", "call_id": "c", "name": "find", "arguments": ""}
	for name, second := range map[string]map[string]any{
		"repeated ID":      {"id": "f", "type": "reasoning"},
		"repeated call ID": {"id": "f2", "type": "function_call", "call_id": "c", "name": "find", "arguments": ""},
	} {
		t.Run(name, func(t *testing.T) {
			events := []map[string]any{parserCreated(), parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": first}), parserEvent("response.output_item.added", map[string]any{"output_index": 1, "item": second})}
			if _, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), nil); !errors.Is(err, errStream) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	events := []map[string]any{parserCreated(), parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": first}), parserEvent("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "reason", "type": "reasoning"}})}
	limits := parserLimits()
	limits.OutputItems = 1
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), limits, nil); !errors.Is(err, errStream) {
		t.Fatalf("item count: %v", err)
	}
	for _, body := range []string{
		parserBody([]map[string]any{parserCreated(), parserCompleted()}) + "data: [DONE]\n\ndata: [DONE]\n\n",
		parserBody([]map[string]any{parserCreated(), parserCompleted()}) + "event: ping\nevent: ping\ndata: {}\n\n",
	} {
		if _, err := parseStream(context.Background(), strings.NewReader(body), parserLimits(), nil); !errors.Is(err, errStream) {
			t.Fatalf("duplicate framing: %v", err)
		}
	}
	initial := map[string]any{"id": "reason", "type": "reasoning"}
	done := map[string]any{"id": "reason", "type": "reasoning", "opaque": json.Number("9007199254740992")}
	final := map[string]any{"id": "reason", "type": "reasoning", "opaque": json.Number("9007199254740993")}
	events = []map[string]any{parserCreated(), parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": initial}), parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": done}), parserCompleted(final)}
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), nil); !errors.Is(err, errStream) {
		t.Fatalf("changed raw integer: %v", err)
	}
	if equalJSON(json.RawMessage(`{"bad":`), json.RawMessage(`{}`)) {
		t.Fatal("invalid JSON was equal")
	}
}

func TestUnitParserRefusalIsDistinctFromProtocolFailure(t *testing.T) {
	for name, events := range map[string][]map[string]any{
		"refusal delta": {parserCreated(), parserEvent("response.refusal.delta", map[string]any{"delta": "PRIVATE refusal"})},
		"refusal done":  {parserCreated(), parserEvent("response.refusal.done", map[string]any{"refusal": "PRIVATE refusal"})},
		"added refusal part": func() []map[string]any {
			e := parserTextEvents()[:4]
			e[3]["part"] = map[string]any{"type": "refusal", "refusal": "PRIVATE refusal"}
			return e
		}(),
		"done refusal part": func() []map[string]any {
			e := parserTextEvents()[:8]
			e[7]["part"] = map[string]any{"type": "refusal", "refusal": "PRIVATE refusal"}
			return e
		}(),
		"final refusal message": func() []map[string]any {
			e := parserTextEvents()[:9]
			e[8]["item"].(map[string]any)["content"] = []any{map[string]any{"type": "refusal", "refusal": "PRIVATE refusal"}}
			return e
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := parseStream(context.Background(), strings.NewReader(parserBody(events)), parserLimits(), nil)
			if !errors.Is(err, ErrRefused) || errors.Is(err, errStream) || strings.Contains(err.Error(), "PRIVATE") || result.ID != "" {
				t.Fatalf("result %+v; error %v", result, err)
			}
		})
	}
	// An out-of-order refusal event is still a protocol violation.
	if _, err := parseStream(context.Background(), strings.NewReader(parserBody([]map[string]any{parserEvent("response.refusal.done", nil)})), parserLimits(), nil); !errors.Is(err, errStream) {
		t.Fatalf("missing lifecycle: %v", err)
	}
}

func TestUnitProviderNestedToolUsage(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  int64
		fails bool
	}{
		{`{"input_tokens":10,"input_tokens_details":{"tool_tokens":7}}`, 7, false},
		{`{"tool_tokens":7,"input_tokens_details":{"tool_tokens":7}}`, 7, false},
		{`{"tool_tokens":0,"input_tokens_details":{"tool_tokens":7}}`, 0, true},
	} {
		var u responseUsage
		if e := decodeProviderJSON([]byte(tc.raw), &u); e != nil {
			t.Fatal(e)
		}
		got, e := u.value()
		if (e != nil) != tc.fails || got.ToolTokens != tc.want {
			t.Fatal(tc, got, e)
		}
	}
}
