package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestIntegrationStreamGuardedReservesExactSentWireBody(t *testing.T) {
	guarded := make(chan []byte, 1)
	var keys, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		actual, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var reserved []byte
		select {
		case reserved = <-guarded:
		default:
			t.Error("HTTP arrived before reservation")
			return
		}
		if !bytes.Equal(actual, reserved) {
			t.Errorf("reserved body differs from HTTP body:\n%s\n%s", reserved, actual)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(actual, &fields); err != nil {
			t.Error(err)
		}
		for field, want := range map[string]string{"model": `"gpt://folder/aliceai-llm"`, "stream": "true", "store": "false", "parallel_tool_calls": "true", "truncation": `"disabled"`, "max_output_tokens": "64", "max_tool_calls": "3"} {
			if string(fields[field]) != want {
				t.Errorf("wire field %s: %s, want %s", field, fields[field], want)
			}
		}
		if !bytes.Contains(actual, []byte(`\u003c`)) || !bytes.Contains(actual, []byte(`\u0026`)) || !bytes.Contains(actual, []byte(`\u2028`)) || !bytes.Contains(actual, []byte("Привет 🌍")) {
			t.Errorf("wire escapes or UTF-8 were lost: %s", actual)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, httpTestSSE("done"))
	}))
	defer server.Close()
	c := httpTestClient(t, server, func(o *Options) {
		o.Key = httpTestKey(func(context.Context) (string, error) { keys.Add(1); return "test-secret", nil })
	})
	request := httpTestRequest()
	request.Instructions = "Привет 🌍 <tag>&\u2028\n\"quote\""
	request.Input = []json.RawMessage{json.RawMessage(`{"role":"user","content":"<>& Привет 🌍 \"quote\""}`)}
	request.Tools = []json.RawMessage{json.RawMessage(`{"type":"function","name":"get_data","description":"<>& Русский 🌍","parameters":{"type":"object"}}`)}
	guardCalls := 0
	result, err := c.StreamGuarded(httpTestContext(t), request, func(Delta) error { return nil }, func(raw []byte) error {
		guardCalls++
		if keys.Load() != 0 || calls.Load() != 0 {
			t.Error("guard ran after credentials or HTTP")
		}
		guarded <- bytes.Clone(raw)
		// Changes to the caller-owned request after reservation must not cause
		// a second marshal or alter the bytes already checked by the guard.
		copy(request.Input[0], []byte(`{"different":"value"}`))
		return nil
	})
	if err != nil || result.ID != "r" || guardCalls != 1 || keys.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("result %+v err=%v guard=%d keys=%d HTTP=%d", result, err, guardCalls, keys.Load(), calls.Load())
	}
}

func TestUnitStreamGuardedFailureDoesNotReadKeysOrSendHTTP(t *testing.T) {
	guardFailure := errors.New("reservation refused")
	for _, failure := range []error{guardFailure, fmt.Errorf("wrapped: %w", guardFailure), context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			o := httpTestOptions()
			keys, calls, guards := 0, 0, 0
			o.Key = httpTestKey(func(context.Context) (string, error) { keys++; return "test-secret", nil })
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected HTTP") })
			result, err := c.StreamGuarded(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil }, func([]byte) error { guards++; return failure })
			if !errors.Is(err, failure) || result.ID != "" || keys != 0 || calls != 0 || guards != 1 {
				t.Fatalf("result %+v err=%v keys=%d HTTP=%d guard=%d", result, err, keys, calls, guards)
			}
		})
	}
}

func TestUnitStreamGuardedValidationAndCancellation(t *testing.T) {
	o := httpTestOptions()
	keys, calls, guards := 0, 0, 0
	o.Key = httpTestKey(func(context.Context) (string, error) { keys++; return "test-secret", nil })
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected HTTP") })
	if _, err := c.StreamGuarded(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil }, nil); err == nil {
		t.Fatal("nil guard accepted")
	}
	guard := func([]byte) error { guards++; return nil }
	oversized := httpTestRequest()
	oversized.Instructions = strings.Repeat("<&", int(o.Limits.RequestBytes)/8)
	if _, err := c.StreamGuarded(httpTestContext(t), oversized, func(Delta) error { return nil }, guard); err == nil {
		t.Fatal("technical byte guard skipped")
	}
	malformed := httpTestRequest()
	malformed.Input = []json.RawMessage{json.RawMessage(`{"bad":`)}
	if _, err := c.StreamGuarded(httpTestContext(t), malformed, func(Delta) error { return nil }, guard); err == nil {
		t.Fatal("malformed request accepted")
	}
	ctx, cancel := context.WithCancel(httpTestContext(t))
	cancel()
	if _, err := c.StreamGuarded(ctx, httpTestRequest(), func(Delta) error { return nil }, guard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if guards != 0 || keys != 0 || calls != 0 {
		t.Fatalf("validation had effects guard=%d keys=%d HTTP=%d", guards, keys, calls)
	}
	ctx, cancel = context.WithCancel(httpTestContext(t))
	defer cancel()
	if _, err := c.StreamGuarded(ctx, httpTestRequest(), func(Delta) error { return nil }, func([]byte) error { guards++; cancel(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if guards != 1 || keys != 0 || calls != 0 {
		t.Fatalf("cancelled guard had effects guard=%d keys=%d HTTP=%d", guards, keys, calls)
	}
}
