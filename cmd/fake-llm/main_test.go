package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUnitRejectUnsafeAddressAndBadArguments(t *testing.T) {
	for _, args := range [][]string{{"-addr", "0.0.0.0:8092"}, {"-addr", "localhost:8092"}, {"-addr", "invalid"}, {"extra"}, {"-bad"}, {"-scenarios", "/nonexistent/fixture"}} {
		if e := run(t.Context(), args, io.Discard); e == nil {
			t.Fatal("accepted", args)
		}
	}
}

type startupWriter struct{ addr chan string }

func (w startupWriter) Write(b []byte) (int, error) {
	parts := strings.Fields(string(b))
	if len(parts) == 5 {
		w.addr <- parts[4]
	}
	return len(b), nil
}
func testCommandServer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := startupWriter{make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, w) }()
	var addr string
	select {
	case addr = <-w.addr:
	case e := <-done:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	req, e := http.NewRequestWithContext(t.Context(), "POST", "http://"+addr+"/v1/responses", strings.NewReader(`{"model":"gpt://local/aliceai-llm-flash","input":[{"role":"user","content":"unknown"}],"stream":true,"store":false,"max_tool_calls":0}`))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("x-data-logging-enabled", "false")
	c := &http.Client{Timeout: 5 * time.Second}
	response, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if e != nil || response.StatusCode != 200 || !strings.Contains(string(raw), "response.completed") {
		t.Fatal(fmt.Sprint(response.StatusCode, e, string(raw)))
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown hung")
	}
}
func TestIntegrationCommandServesAndShutsDown(t *testing.T) { testCommandServer(t) }
func TestAPICommandResponses(t *testing.T)                  { testCommandServer(t) }
