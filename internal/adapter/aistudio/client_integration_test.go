package aistudio

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func httpTestSSE(text string) string {
	part := map[string]any{"type": "output_text", "text": text}
	item := map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "completed", "content": []any{part}}
	return httpTestFrames(
		map[string]any{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "m", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		map[string]any{"type": "response.content_part.added", "item_id": "m", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}},
		map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": text},
		map[string]any{"type": "response.output_text.done", "item_id": "m", "output_index": 0, "content_index": 0, "text": text},
		map[string]any{"type": "response.content_part.done", "item_id": "m", "output_index": 0, "content_index": 0, "part": part},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "tool_tokens": 0}}},
	)
}

func httpTestFrames(events ...map[string]any) string {
	var result strings.Builder
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		result.WriteString("data: ")
		result.Write(raw)
		result.WriteString("\n\n")
	}
	return result.String()
}

func httpTestClient(t *testing.T, server *httptest.Server, mutate func(*Options)) *Client {
	t.Helper()
	o := httpTestOptions()
	o.Endpoint = server.URL + "/v1/responses"
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestIntegrationHTTPWirePolicyAndStatelessContinuation(t *testing.T) {
	var requests []map[string]json.RawMessage
	var keys atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" {
			t.Errorf("method/path=%s %s", r.Method, r.URL.Path)
		}
		for header, want := range map[string]string{"Authorization": fmt.Sprintf("Bearer test-secret-%d", keys.Load()), "OpenAI-Project": "folder", "X-Data-Logging-Enabled": "false", "Content-Type": "application/json", "Accept": "text/event-stream"} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("header %s=%q want=%q", header, got, want)
			}
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, httpTestSSE("Привет"))
	}))
	defer server.Close()
	c := httpTestClient(t, server, func(o *Options) {
		o.Key = httpTestKey(func(context.Context) (string, error) { return fmt.Sprintf("test-secret-%d", keys.Add(1)), nil })
	})
	first := httpTestRequest()
	first.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","name":"get_data","parameters":{"type":"object"}}`)}
	var deltas []Delta
	result, err := c.Stream(httpTestContext(t), first, func(d Delta) error { deltas = append(deltas, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "r" || len(result.Output) != 1 || result.Usage.TotalTokens != 2 || !reflect.DeepEqual(deltas, []Delta{{OutputIndex: 0, ContentIndex: 0, Text: "Привет"}}) {
		t.Fatalf("result=%+v deltas=%+v", result, deltas)
	}
	// The continuation uses the complete original output item, plus the caller's
	// tool result. The transport must neither strip unknown fields nor substitute a
	// provider-side conversation ID.
	raw := json.RawMessage(`{"id":"call-item","type":"function_call","call_id":"c","name":"get_data","arguments":"{}","future":{"opaque":true}}`)
	second := httpTestRequest()
	second.Input = append(second.Input, raw, json.RawMessage(`{"type":"function_call_output","call_id":"c","output":"{\"amount\":123}"}`))
	if _, err = c.Stream(httpTestContext(t), second, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || keys.Load() != 2 {
		t.Fatalf("requests=%d keyreads=%d", len(requests), keys.Load())
	}
	for _, body := range requests {
		for field, want := range map[string]string{"stream": "true", "store": "false", "truncation": `"disabled"`, "model": `"gpt://folder/aliceai-llm"`, "parallel_tool_calls": "true"} {
			if string(body[field]) != want {
				t.Errorf("%s=%s want=%s", field, body[field], want)
			}
		}
		for _, absent := range []string{"previous_response_id", "conversation", "response_format", "text"} {
			if _, exists := body[absent]; exists {
				t.Errorf("forbidden field %s", absent)
			}
		}
	}
	var input []json.RawMessage
	if err = json.Unmarshal(requests[1]["input"], &input); err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err = json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(input[1], &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) || !bytes.Contains(input[2], []byte(`function_call_output`)) {
		t.Fatalf("continuation changed: %s", requests[1]["input"])
	}
}

func TestIntegrationHTTPFunctionCallRawOutputContinuation(t *testing.T) {
	item := map[string]any{"id": "f", "type": "function_call", "call_id": "call", "name": "get_data", "arguments": `{"period":"month"}`, "status": "completed", "future": map[string]any{"opaque": true}}
	functionStream := httpTestFrames(
		map[string]any{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}},
		map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "f", "type": "function_call", "call_id": "call", "name": "get_data", "arguments": "", "status": "in_progress"}},
		map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "f", "delta": `{"period":`},
		map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "f", "delta": `"month"}`},
		map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": "f", "arguments": `{"period":"month"}`, "name": "get_data"},
		map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "r", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 3, "total_tokens": 5, "tool_tokens": 1}}},
	)
	var calls atomic.Int32
	var continuation []json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, functionStream)
			return
		}
		var request Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		continuation = request.Input
		_, _ = io.WriteString(w, httpTestSSE("done"))
	}))
	defer server.Close()
	c := httpTestClient(t, server, nil)
	result, err := c.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { t.Error("function emitted text"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 1 || result.Output[0].Arguments != `{"period":"month"}` || result.Output[0].CallID != "call" || result.Output[0].Name != "get_data" || result.Usage.ToolTokens != 1 {
		t.Fatalf("function result: %+v", result)
	}
	next := httpTestRequest()
	next.Input = append(next.Input, result.Output[0].Raw, json.RawMessage(`{"type":"function_call_output","call_id":"call","output":"[]"}`))
	if _, err = c.Stream(httpTestContext(t), next, func(Delta) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(continuation) != 3 {
		t.Fatalf("continuation items=%d", len(continuation))
	}
	var returned map[string]any
	if err = json.Unmarshal(continuation[1], &returned); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(item, returned) {
		t.Fatalf("raw function item lost provider fields: %s", continuation[1])
	}
}

func TestIntegrationHTTPDoesNotRetryPaidRequests(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "provider-private test-secret private-question")
			}))
			defer server.Close()
			_, err := httpTestClient(t, server, nil).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
			if err == nil {
				t.Fatal("HTTP refusal accepted")
			}
			assertHTTPTestSanitized(t, err)
			if calls.Load() != 1 {
				t.Fatalf("paid request retried %d times", calls.Load())
			}
		})
	}
	t.Run("disconnect", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}))
		defer server.Close()
		_, err := httpTestClient(t, server, nil).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
		if err == nil {
			t.Fatal("disconnect accepted")
		}
		assertHTTPTestSanitized(t, err)
		if calls.Load() != 1 {
			t.Fatalf("paid request retried %d times", calls.Load())
		}
	})
}

func TestIntegrationHTTPRejectsRedirectBeforeSecondRequest(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1); w.WriteHeader(200) }))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, destination.URL+"/v1/responses", status)
			}))
			defer source.Close()
			_, err := httpTestClient(t, source, nil).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
			if err == nil {
				t.Fatal("redirect accepted")
			}
			if destinationCalls.Load() != 0 {
				t.Fatalf("secret-bearing redirected request sent: %d", destinationCalls.Load())
			}
		})
	}
}

func TestIntegrationHTTPRejectsInvalidResponse(t *testing.T) {
	for _, tc := range []struct {
		name, media, encoding, body string
		gzip                        bool
		mutate                      func(*Options)
	}{
		{name: "missing media", body: httpTestSSE("private-question")},
		{name: "JSON media", media: "application/json", body: httpTestSSE("private-question")},
		{name: "malformed SSE", media: "text/event-stream", body: "data: provider-private\n\n"},
		{name: "truncated SSE", media: "text/event-stream", body: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\",\"output\":[]}}\n\n"},
		{name: "encoding", media: "text/event-stream", encoding: "br", body: httpTestSSE("private-question")},
		{name: "compressed response", media: "text/event-stream", encoding: "gzip", body: httpTestSSE("private-question"), gzip: true},
		{name: "response cap", media: "text/event-stream", body: httpTestSSE("hello"), mutate: func(o *Options) { o.Limits.ResponseBytes = 128; o.Limits.EventBytes = 64; o.Limits.ArgumentBytes = 64 }},
		{name: "event cap", media: "text/event-stream", body: httpTestSSE(strings.Repeat("x", 1000)), mutate: func(o *Options) { o.Limits.EventBytes = 512 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.media != "" {
					w.Header().Set("Content-Type", tc.media)
				} else {
					w.Header()["Content-Type"] = nil
				}
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				if tc.gzip {
					writer := gzip.NewWriter(w)
					_, _ = io.WriteString(writer, tc.body)
					_ = writer.Close()
				} else {
					_, _ = io.WriteString(w, tc.body)
				}
			}))
			defer server.Close()
			_, err := httpTestClient(t, server, tc.mutate).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
			if err == nil {
				t.Fatal("invalid HTTP response accepted")
			}
			assertHTTPTestSanitized(t, err)
		})
	}
}

func TestIntegrationHTTPContextAndCallback(t *testing.T) {
	t.Run("callback", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, httpTestSSE("hello"))
		}))
		defer server.Close()
		stop := errors.New("caller stopped durable output")
		_, err := httpTestClient(t, server, nil).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return stop })
		if !errors.Is(err, stop) {
			t.Fatalf("callback error not preserved: %v", err)
		}
	})
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelled), func(t *testing.T) {
			started := make(chan struct{})
			ended := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(ended)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if cancelled {
				ctx, cancel = context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			_, err := httpTestClient(t, server, nil).Stream(ctx, httpTestRequest(), func(Delta) error { return nil })
			want := context.DeadlineExceeded
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("context error=%v want=%v", err, want)
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("HTTP response body remained active after cancellation")
			}
		})
	}
}

func TestIntegrationStageLoopbackTransport(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("x-data-logging-enabled") != "false" {
			t.Error("stage provider lost explicit model identity or privacy headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, httpTestSSE("stage answer"))
	}))
	defer server.Close()
	client := httpTestClient(t, server, func(o *Options) { o.Environment = "stage" })
	result, err := client.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
	if err != nil || len(result.Output) != 1 || calls.Load() != 1 {
		t.Fatalf("stage fake: result=%v calls=%d error=%v", result, calls.Load(), err)
	}
	// A stage provider cannot redirect a secret-bearing request, even to loopback.
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/v1/responses", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	_, err = httpTestClient(t, redirect, func(o *Options) { o.Environment = "stage" }).Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
	if err == nil || redirected.Load() != 0 {
		t.Fatal("stage redirect sent credentials", err, redirected.Load())
	}
}
