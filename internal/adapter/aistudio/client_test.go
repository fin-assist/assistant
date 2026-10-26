package aistudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

type httpTestKey func(context.Context) (string, error)

func (f httpTestKey) Key(ctx context.Context) (string, error) { return f(ctx) }

func httpTestOptions() Options {
	return Options{Environment: "dev", FolderID: "folder", Model: "gpt://folder/aliceai-llm",
		Key:    httpTestKey(func(context.Context) (string, error) { return "test-secret", nil }),
		Limits: Limits{RequestBytes: 32 << 10, ResponseBytes: 64 << 10, EventBytes: 8 << 10, ArgumentBytes: 4 << 10, OutputItems: 8}}
}

func httpTestRequest() Request {
	return Request{Instructions: "system", Input: []json.RawMessage{json.RawMessage(`{"role":"user","content":"private-question"}`)}, MaxOutputTokens: 64, MaxToolCalls: 3}
}

func httpTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestUnitClientConfiguration(t *testing.T) {
	for _, tc := range []struct {
		env, endpoint string
		valid         bool
	}{
		{"prod", "", true}, {"stage", officialEndpoint, true}, {"dev", officialEndpoint, true},
		{"dev", "http://localhost:123/v1/responses", true}, {"dev", "http://127.0.0.1:123/v1/responses", true},
		{"dev", "http://[::1]:123/v1/responses", true},
		{"test", officialEndpoint, false}, {"stage", "http://localhost/v1/responses", false},
		{"prod", "http://ai.api.cloud.yandex.net/v1/responses", false},
		{"prod", "https://ai.api.cloud.yandex.net:443/v1/responses", false},
		{"prod", "https://ai.api.cloud.yandex.net.evil/v1/responses", false},
		{"dev", "http://192.0.2.1/v1/responses", false}, {"dev", "https://localhost/v1/responses", false},
		{"dev", "http://user:password@localhost/v1/responses", false},
		{"dev", "http://localhost/v1/responses?key=secret", false},
		{"dev", "http://localhost/v1/responses?", false},
		{"dev", "http://localhost/v1/responses#fragment", false},
		{"dev", "http://localhost/v1/%72esponses", false},
		{"dev", "http://localhost/v1/responses/", false}, {"dev", "http://localhost/other", false},
		{"dev", "%", false},
	} {
		t.Run(tc.env+tc.endpoint, func(t *testing.T) {
			opts := httpTestOptions()
			opts.Environment, opts.Endpoint = tc.env, tc.endpoint
			keyCalls := 0
			opts.Key = httpTestKey(func(context.Context) (string, error) { keyCalls++; return "secret", nil })
			client, err := New(opts)
			if (err == nil) != tc.valid {
				t.Fatalf("New valid=%v, error=%v", tc.valid, err)
			}
			if client != nil {
				client.Close()
			}
			if keyCalls != 0 {
				t.Fatal("construction fetched credentials")
			}
		})
	}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.Key = nil }, func(o *Options) { o.FolderID = "" },
		func(o *Options) { o.FolderID = " folder" }, func(o *Options) { o.FolderID = "folder/other" },
		func(o *Options) { o.Model = "gpt://other/aliceai-llm" }, func(o *Options) { o.Model = "gpt://folder/unknown" },
		func(o *Options) { o.Limits.RequestBytes = 0 }, func(o *Options) { o.Limits.ResponseBytes = -1 },
		func(o *Options) { o.Limits.ResponseBytes = math.MaxInt64 },
		func(o *Options) { o.Limits.EventBytes = 0 }, func(o *Options) { o.Limits.EventBytes = 1 << 20 },
		func(o *Options) { o.Limits.ArgumentBytes = 0 }, func(o *Options) { o.Limits.ArgumentBytes = 1 << 20 },
		func(o *Options) { o.Limits.OutputItems = 0 },
	} {
		o := httpTestOptions()
		mutate(&o)
		if c, err := New(o); err == nil {
			c.Close()
			t.Fatal("invalid configuration accepted")
		}
	}
	flash := httpTestOptions()
	flash.Model = "gpt://folder/aliceai-llm-flash"
	c, err := New(flash)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

type httpTestRoundTrip func(*http.Request) (*http.Response, error)

func (f httpTestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type httpTestBody struct {
	io.Reader
	closed bool
}

func (b *httpTestBody) Close() error { b.closed = true; return nil }

func TestUnitClientRejectsBeforeSending(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*Request)
		noDeadline bool
		emitNil    bool
	}{
		{name: "deadline", noDeadline: true}, {name: "nil callback", emitNil: true},
		{name: "empty input", mutate: func(r *Request) { r.Input = nil }},
		{name: "output limit", mutate: func(r *Request) { r.MaxOutputTokens = 15 }},
		{name: "negative tools", mutate: func(r *Request) { r.MaxToolCalls = -1 }},
		{name: "input scalar", mutate: func(r *Request) { r.Input = []json.RawMessage{json.RawMessage(`"secret"`)} }},
		{name: "input null", mutate: func(r *Request) { r.Input = []json.RawMessage{json.RawMessage(`null`)} }},
		{name: "duplicate field", mutate: func(r *Request) { r.Input = []json.RawMessage{json.RawMessage(`{"role":"user","role":"system"}`)} }},
		{name: "malformed tool", mutate: func(r *Request) { r.Tools = []json.RawMessage{json.RawMessage(`{`)} }},
		{name: "oversize", mutate: func(r *Request) { r.Instructions = strings.Repeat("private-question", 4096) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpTestOptions()
			calls := 0
			o.Key = httpTestKey(func(context.Context) (string, error) { calls++; return "secret", nil })
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				t.Error("sent invalid request")
				return nil, errors.New("unexpected")
			})
			r := httpTestRequest()
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			ctx := httpTestContext(t)
			if tc.noDeadline {
				ctx = t.Context()
			}
			emit := func(Delta) error { return nil }
			if tc.emitNil {
				emit = nil
			}
			_, err = c.Stream(ctx, r, emit)
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			if calls != 0 {
				t.Fatal("invalid request fetched key")
			}
			assertHTTPTestSanitized(t, err)
		})
	}
}

func TestUnitClientCredentialAndTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		keyErr    error
		transport bool
	}{
		{name: "credential error", keyErr: errors.New("private-question test-secret provider-private")},
		{name: "empty key"}, {name: "whitespace key", key: "test secret"}, {name: "newline key", key: "test\nsecret"},
		{name: "long key", key: strings.Repeat("s", 8193)}, {name: "unicode key", key: "secreté"},
		{name: "transport error", key: "test-secret", transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpTestOptions()
			o.Key = httpTestKey(func(context.Context) (string, error) { return tc.key, tc.keyErr })
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			calls := 0
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("private-question test-secret provider-private")
			})
			_, err = c.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
			if err == nil {
				t.Fatal("failure accepted")
			}
			assertHTTPTestSanitized(t, err)
			want := 0
			if tc.transport {
				want = 1
			}
			if calls != want {
				t.Fatalf("HTTP requests=%d want=%d", calls, want)
			}
		})
	}
	ctx, cancel := context.WithCancel(httpTestContext(t))
	cancel()
	c, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Stream(ctx, httpTestRequest(), func(Delta) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
}

func TestUnitClientPreservesCancellationDuringCredentialAndTransport(t *testing.T) {
	for _, duringKey := range []bool{false, true} {
		t.Run(fmt.Sprintf("key=%v", duringKey), func(t *testing.T) {
			ctx, cancel := context.WithCancel(httpTestContext(t))
			defer cancel()
			o := httpTestOptions()
			o.Key = httpTestKey(func(context.Context) (string, error) {
				if duringKey {
					cancel()
					return "", errors.New("provider-private credential")
				}
				return "test-secret", nil
			})
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				if duringKey {
					t.Error("request sent after credential cancellation")
				}
				cancel()
				return nil, errors.New("provider-private transport")
			})
			_, err = c.Stream(ctx, httpTestRequest(), func(Delta) error { return nil })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			assertHTTPTestSanitized(t, err)
		})
	}
}

func TestUnitClientClosesResponseOnEveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, media, encoding, body string
		status                      int
		responseLimit               int64
	}{
		{name: "status", status: 503, media: "text/event-stream", body: "provider-private"},
		{name: "mime", status: 200, media: "application/json", body: "provider-private"},
		{name: "malformed mime", status: 200, media: "text/event-stream; malformed", body: "provider-private"},
		{name: "encoding", status: 200, media: "text/event-stream", encoding: "gzip", body: "provider-private"},
		{name: "stream", status: 200, media: "text/event-stream", body: "data: provider-private\n\n"},
		{name: "response limit", status: 200, media: "text/event-stream", body: strings.Repeat(": comment\n", 100), responseLimit: 32},
		{name: "event limit", status: 200, media: "text/event-stream", body: "data: " + strings.Repeat("x", 9000) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := httpTestOptions()
			if tc.responseLimit != 0 {
				o.Limits.ResponseBytes = tc.responseLimit
				o.Limits.EventBytes = 16
				o.Limits.ArgumentBytes = 16
			}
			c, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			body := &httpTestBody{Reader: strings.NewReader(tc.body)}
			calls := 0
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.media}, "Content-Encoding": {tc.encoding}}, Body: body}, nil
			})
			_, err = c.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error { return nil })
			if err == nil {
				t.Fatal("bad response accepted")
			}
			assertHTTPTestSanitized(t, err)
			if !body.closed {
				t.Fatal("response body not closed")
			}
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
		})
	}
}

func TestUnitClientClosesSuccessfulAndInterruptedBody(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupt), func(t *testing.T) {
			c, err := New(httpTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			body := &httpTestBody{Reader: strings.NewReader(httpTestSSE("hello"))}
			c.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
			})
			stop := errors.New("durable output failed")
			_, err = c.Stream(httpTestContext(t), httpTestRequest(), func(Delta) error {
				if interrupt {
					return stop
				}
				return nil
			})
			if interrupt && !errors.Is(err, stop) || !interrupt && err != nil {
				t.Fatalf("completion error=%v", err)
			}
			if !body.closed {
				t.Fatal("response body not closed")
			}
		})
	}
}

func assertHTTPTestSanitized(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{"private-question", "test-secret", "provider-private"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed private data: %q", err)
		}
	}
}

func TestUnitRequestValidityDuplicatesFailBeforeCredentials(t *testing.T) {
	options := httpTestOptions()
	calls := 0
	options.Key = httpTestKey(func(context.Context) (string, error) { calls++; return "", errors.New("must not obtain credentials") })
	client, e := New(options)
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	request := httpTestRequest()
	request.Input = []json.RawMessage{json.RawMessage(`{"role":"user","content":"test","valid":true,"valid":true}`)}
	if _, e = client.Stream(httpTestContext(t), request, func(Delta) error { return nil }); e == nil || calls != 0 {
		t.Fatal("request used provider duplicate exception", e, calls)
	}
}
