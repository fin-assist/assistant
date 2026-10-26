package aistudio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fin-assist/assistant/internal/generation"
)

// These checks cross the real HTTP transport and the model port. A valid text
// fragment is provisional: no failed response may expose terminal output or tool calls.
func assertRejectedHTTPModel(t *testing.T, body string, refused bool, fragments int) {
	t.Helper()
	var posts atomic.Int32
	sent := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		select {
		case sent <- raw:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	model, err := NewGenerationModel(httpTestClient(t, server, nil))
	if err != nil {
		t.Fatal(err)
	}
	req := httpTestRequest()
	var reserved []byte
	reservations := 0
	var emitted []generation.ModelDelta
	result, err := model.Stream(httpTestContext(t), generation.ModelRequest{
		Instructions: req.Instructions, Input: req.Input, Tools: req.Tools,
		MaxOutputTokens: req.MaxOutputTokens, MaxToolCalls: req.MaxToolCalls,
	}, func(delta generation.ModelDelta) error {
		emitted = append(emitted, delta)
		return nil
	}, func(raw []byte) error {
		reservations++
		reserved = bytes.Clone(raw)
		return nil
	})
	if err == nil || errors.Is(err, generation.ErrRefused) != refused {
		t.Fatalf("refused=%v result=%+v err=%v", refused, result, err)
	}
	if !reflect.DeepEqual(result, generation.ModelResult{}) {
		t.Fatalf("unconfirmed output, usage or tool calls escaped failed stream: %+v", result)
	}
	assertHTTPTestSanitized(t, err)
	if posts.Load() != 1 || reservations != 1 {
		t.Fatalf("failed paid stream retried or re-reserved: posts=%d reservations=%d", posts.Load(), reservations)
	}
	if raw := <-sent; !bytes.Equal(raw, reserved) {
		t.Fatal("HTTP request differs from its single budget reservation")
	}
	if len(emitted) != fragments {
		t.Fatalf("provisional fragments=%d want=%d", len(emitted), fragments)
	}
	for _, delta := range emitted {
		if delta.OutputIndex != 0 || delta.ContentIndex != 0 || (delta.Text != "При" && delta.Text != "вет 🌍") {
			t.Fatalf("unvalidated content reached the text callback: %+v", delta)
		}
	}
}

func TestIntegrationHTTPModelRejectsBrokenTextLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func([]map[string]any) []map[string]any
		fragments int
	}{
		{"missing creation", func(e []map[string]any) []map[string]any { return e[1:] }, 0},
		{"duplicate creation", func(e []map[string]any) []map[string]any { return append([]map[string]any{e[0]}, e...) }, 0},
		{"duplicate progress", func(e []map[string]any) []map[string]any { e[2] = e[1]; return e }, 0},
		{"foreign progress ID", func(e []map[string]any) []map[string]any {
			e[1]["response"].(map[string]any)["id"] = "provider-private"
			return e
		}, 0},
		{"unsupported item", func(e []map[string]any) []map[string]any {
			e[2]["item"].(map[string]any)["type"] = "file_search_call"
			return e
		}, 0},
		{"initial message contains text", func(e []map[string]any) []map[string]any {
			e[2]["item"].(map[string]any)["content"] = []any{map[string]any{"type": "output_text", "text": "provider-private"}}
			return e
		}, 0},
		{"negative item index", func(e []map[string]any) []map[string]any { e[2]["output_index"] = -1; return e }, 0},
		{"missing output index on text", func(e []map[string]any) []map[string]any { delete(e[4], "output_index"); return e }, 0},
		{"prefilled text part", func(e []map[string]any) []map[string]any {
			e[3]["part"] = map[string]any{"type": "output_text", "text": "provider-private"}
			return e
		}, 0},
		{"malformed part completion", func(e []map[string]any) []map[string]any { e[7]["part"] = "provider-private"; return e }, 2},
		{"unknown completed item index", func(e []map[string]any) []map[string]any { e[8]["output_index"] = 1; return e }, 2},
		{"malformed final content part", func(e []map[string]any) []map[string]any {
			e[8]["item"].(map[string]any)["content"] = []any{"provider-private"}
			return e
		}, 2},
		{"missing final content part", func(e []map[string]any) []map[string]any {
			e[8]["item"].(map[string]any)["content"] = []any{}
			return e
		}, 2},
		{"foreign text item", func(e []map[string]any) []map[string]any { e[4]["item_id"] = "provider-private"; return e }, 0},
		{"missing content index", func(e []map[string]any) []map[string]any { delete(e[4], "content_index"); return e }, 0},
		{"content gap", func(e []map[string]any) []map[string]any { e[3]["content_index"] = 1; return e }, 0},
		{"delta before part", func(e []map[string]any) []map[string]any { return append(e[:3], e[4:]...) }, 0},
		{"delta after text done", func(e []map[string]any) []map[string]any { e[7] = e[4]; return e }, 2},
		{"changed text done", func(e []map[string]any) []map[string]any { e[6]["text"] = "provider-private"; return e }, 2},
		{"part before text done", func(e []map[string]any) []map[string]any { e[6], e[7] = e[7], e[6]; return e }, 2},
		{"item before part done", func(e []map[string]any) []map[string]any { return append(e[:7], e[8:]...) }, 2},
		{"repeated text after finished part", func(e []map[string]any) []map[string]any { e[8] = e[4]; return e }, 2},
		{"repeated item completion", func(e []map[string]any) []map[string]any {
			return append(e[:9], append([]map[string]any{e[8]}, e[9:]...)...)
		}, 2},
		{"wrong final role", func(e []map[string]any) []map[string]any { e[8]["item"].(map[string]any)["role"] = "user"; return e }, 2},
		{"wrong final item ID", func(e []map[string]any) []map[string]any {
			e[8]["item"].(map[string]any)["id"] = "provider-private"
			return e
		}, 2},
		{"missing completion", func(e []map[string]any) []map[string]any { return e[:9] }, 2},
		{"changed final output", func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["output"] = []any{}
			return e
		}, 2},
		{"wrong response ID", func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["id"] = "provider-private"
			return e
		}, 2},
		{"missing usage", func(e []map[string]any) []map[string]any {
			delete(e[9]["response"].(map[string]any), "usage")
			return e
		}, 2},
		{"negative usage", func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["usage"].(map[string]any)["tool_tokens"] = -1
			return e
		}, 2},
		{"fractional usage", func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["usage"].(map[string]any)["total_tokens"] = 1.5
			return e
		}, 2},
		{"completion carries error", func(e []map[string]any) []map[string]any {
			e[9]["response"].(map[string]any)["error"] = map[string]any{"message": "provider-private"}
			return e
		}, 2},
		{"sequence regression", func(e []map[string]any) []map[string]any {
			e[0]["sequence_number"] = 3
			e[1]["sequence_number"] = 2
			return e
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRejectedHTTPModel(t, parserBody(tc.change(parserTextEvents())), false, tc.fragments)
		})
	}
}

func httpProtocolFunctionEvents() []map[string]any {
	initial := map[string]any{"id": "f", "type": "function_call", "call_id": "call", "name": "find", "arguments": ""}
	final := map[string]any{"id": "f", "type": "function_call", "call_id": "call", "name": "find", "arguments": `{"query":"provider-private"}`}
	return []map[string]any{
		parserCreated(),
		parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": initial}),
		parserEvent("response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": "f", "delta": final["arguments"]}),
		parserEvent("response.function_call_arguments.done", map[string]any{"output_index": 0, "item_id": "f", "name": "find", "arguments": final["arguments"]}),
		parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": final}),
		parserCompleted(final),
	}
}

func TestIntegrationHTTPModelNeverExposesUnconfirmedFunctionCalls(t *testing.T) {
	for name, change := range map[string]func([]map[string]any) []map[string]any{
		"missing call ID": func(e []map[string]any) []map[string]any { delete(e[1]["item"].(map[string]any), "call_id"); return e },
		"prefilled arguments": func(e []map[string]any) []map[string]any {
			e[1]["item"].(map[string]any)["arguments"] = "provider-private"
			return e
		},
		"foreign argument item":  func(e []map[string]any) []map[string]any { e[2]["item_id"] = "provider-private"; return e },
		"missing argument delta": func(e []map[string]any) []map[string]any { delete(e[2], "delta"); return e },
		"argument cap": func(e []map[string]any) []map[string]any {
			e[2]["delta"] = strings.Repeat("provider-private", 500)
			return e
		},
		"changed arguments":          func(e []map[string]any) []map[string]any { e[3]["arguments"] = `{}`; return e },
		"changed function name":      func(e []map[string]any) []map[string]any { e[3]["name"] = "provider-private"; return e },
		"arguments after done":       func(e []map[string]any) []map[string]any { e[4] = e[2]; return e },
		"item before arguments done": func(e []map[string]any) []map[string]any { return append(e[:3], e[4:]...) },
		"changed final call ID": func(e []map[string]any) []map[string]any {
			e[4]["item"].(map[string]any)["call_id"] = "provider-private"
			return e
		},
		"completed function followed by provider failure": func(e []map[string]any) []map[string]any {
			return append(e[:5], parserEvent("response.failed", map[string]any{"error": "provider-private"}))
		},
		"changed terminal function raw": func(e []map[string]any) []map[string]any {
			e[5]["response"].(map[string]any)["output"] = []any{map[string]any{"id": "f", "type": "function_call", "call_id": "call", "name": "find", "arguments": "{}"}}
			return e
		},
		"duplicate item ID": func(e []map[string]any) []map[string]any {
			e[2] = parserEvent("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "f", "type": "reasoning"}})
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertRejectedHTTPModel(t, parserBody(change(httpProtocolFunctionEvents())), false, 0)
		})
	}
	t.Run("changed exact integer in final raw item", func(t *testing.T) {
		initial := map[string]any{"id": "reason", "type": "reasoning"}
		done := map[string]any{"id": "reason", "type": "reasoning", "opaque": json.Number("9007199254740992")}
		final := map[string]any{"id": "reason", "type": "reasoning", "opaque": json.Number("9007199254740993")}
		events := []map[string]any{parserCreated(), parserEvent("response.output_item.added", map[string]any{"output_index": 0, "item": initial}), parserEvent("response.output_item.done", map[string]any{"output_index": 0, "item": done}), parserCompleted(final)}
		assertRejectedHTTPModel(t, parserBody(events), false, 0)
	})
}

func TestIntegrationHTTPModelDistinguishesRefusalFromBrokenProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, id, status, reason string
		refused                  bool
	}{
		{"moderation", "r", "incomplete", "content_filter", true},
		{"token exhaustion", "r", "incomplete", "max_output_tokens", false},
		{"unknown reason", "r", "incomplete", "provider-private", false},
		{"wrong response ID", "foreign", "incomplete", "content_filter", false},
		{"wrong status", "r", "completed", "content_filter", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := []map[string]any{parserCreated(), parserEvent("response.incomplete", map[string]any{"response": map[string]any{"id": tc.id, "status": tc.status, "incomplete_details": map[string]any{"reason": tc.reason}}})}
			assertRejectedHTTPModel(t, parserBody(events), tc.refused, 0)
		})
	}
	for _, kind := range []string{"response.refusal.delta", "response.refusal.done"} {
		t.Run(kind, func(t *testing.T) {
			assertRejectedHTTPModel(t, parserBody([]map[string]any{parserCreated(), parserEvent(kind, map[string]any{"delta": "provider-private"})}), true, 0)
		})
	}
	t.Run("moderation after provisional text", func(t *testing.T) {
		events := append(parserTextEvents()[:6], parserEvent("response.incomplete", map[string]any{"response": map[string]any{"id": "r", "status": "incomplete", "incomplete_details": map[string]any{"reason": "content_filter"}}}))
		assertRejectedHTTPModel(t, parserBody(events), true, 2)
	})
	for _, tc := range []struct {
		name             string
		index, fragments int
	}{
		{"part added", 3, 0}, {"part done", 7, 2}, {"item done", 8, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := parserTextEvents()
			if tc.index == 8 {
				events[tc.index]["item"].(map[string]any)["content"] = []any{map[string]any{"type": "refusal", "refusal": "provider-private"}}
			} else {
				events[tc.index]["part"] = map[string]any{"type": "refusal", "refusal": "provider-private"}
			}
			assertRejectedHTTPModel(t, parserBody(events), true, tc.fragments)
		})
	}
}

func TestIntegrationHTTPModelRejectsAmbiguousSSEFraming(t *testing.T) {
	valid := parserBody(parserTextEvents())
	for name, body := range map[string]string{
		"DONE before completion":  "data: [DONE]\n\n",
		"duplicate DONE":          valid + "data: [DONE]\n\ndata: [DONE]\n\n",
		"duplicate completion":    valid + parserBody([]map[string]any{parserCompleted()}),
		"event type mismatch":     "event: error\ndata: {\"type\":\"response.created\"}\n\n",
		"duplicate event header":  "event: response.created\nevent: response.created\ndata: {}\n\n",
		"duplicate JSON key":      "data: {\"type\":\"response.created\",\"type\":\"error\"}\n\n",
		"invalid UTF8":            "data: {\"type\":\"response.created\",\"private\":\"\xff\"}\n\n",
		"trailing JSON token":     "data: {\"type\":\"response.created\"} null\n\n",
		"malformed nested object": "data: {\"type\":\"response.created\",\"response\":{\"private\":}}\n\n",
		"malformed nested array":  "data: {\"type\":\"response.created\",\"response\":{\"output\":[} }}\n\n",
		"missing object end":      "data: {\"type\":\"response.created\",\"response\":{\n\n",
		"unterminated event":      "data: {\"type\":\"response.created\"}",
		"unknown critical event":  parserBody([]map[string]any{parserCreated(), parserEvent("response.unknown_critical", nil)}),
	} {
		t.Run(name, func(t *testing.T) {
			fragments := 0
			if strings.HasPrefix(body, valid) {
				fragments = 2
			}
			assertRejectedHTTPModel(t, body, false, fragments)
		})
	}
}

func TestIntegrationHTTPModelAcceptsNormalSSEFramingAtExactResponseLimit(t *testing.T) {
	events := parserTextEvents()
	for i := range events {
		events[i]["sequence_number"] = i
	}
	body := parserBody(events)
	body = strings.Replace(body, `,"sequence_number":`, ",\ndata: \"sequence_number\":", 1)
	body = ": keepalive\nid: ignored\nretry: 100\n\n" + body + "data: [DONE]\n\n"
	body = strings.ReplaceAll(body, "\n", "\r\n")
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client := httpTestClient(t, server, func(o *Options) {
		o.Limits.ResponseBytes = int64(len(body))
		o.Limits.EventBytes = len(body)
		o.Limits.ArgumentBytes = 128
	})
	model, err := NewGenerationModel(client)
	if err != nil {
		t.Fatal(err)
	}
	request := httpTestRequest()
	var fragments []string
	result, err := model.Stream(httpTestContext(t), generation.ModelRequest{Instructions: request.Instructions, Input: request.Input, MaxOutputTokens: request.MaxOutputTokens}, func(delta generation.ModelDelta) error { fragments = append(fragments, delta.Text); return nil }, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || len(result.Output) != 1 || result.Output[0].Type != "message" || result.Usage.TotalTokens != 10 || !reflect.DeepEqual(fragments, []string{"При", "вет 🌍"}) {
		t.Fatalf("posts=%d fragments=%q result=%+v", posts.Load(), fragments, result)
	}
}
