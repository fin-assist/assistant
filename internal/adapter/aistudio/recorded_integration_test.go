package aistudio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/generation"
)

func TestIntegrationRecordedAIStudioResponses(t *testing.T) {
	paths, e := filepath.Glob("testdata/recorded/*.sse")
	if e != nil || len(paths) != 10 {
		t.Fatal("real provider fixtures missing", e)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, e := os.ReadFile(p)
			if e != nil {
				t.Fatal(e)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-data-logging-enabled") != "false" {
					t.Error("missing privacy header")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(raw)
			}))
			defer srv.Close()
			c, e := New(Options{Environment: "dev", Endpoint: srv.URL + "/v1/responses", FolderID: "folder", Model: "gpt://folder/aliceai-llm-flash", Key: recordedKey{}, Limits: Limits{RequestBytes: 131072, ResponseBytes: 1048576, EventBytes: 65536, ArgumentBytes: 16384, OutputItems: 64}})
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			out, e := c.Stream(ctx, Request{Input: []json.RawMessage{json.RawMessage(`{"role":"user","content":"synthetic"}`)}, MaxOutputTokens: 512}, func(Delta) error { return nil })
			if e != nil {
				t.Fatal(e)
			}
			if out.ID == "" || len(out.Output) == 0 || out.Usage.TotalTokens == 0 {
				t.Fatalf("incomplete recording result: %+v", out)
			}
			expected := map[string]struct {
				total, tool int64
				items       int
			}{
				"text-0.sse": {87, 0, 1}, "functions-0.sse": {209, 0, 2}, "functions-1.sse": {281, 0, 1}, "functions-2.sse": {337, 0, 1}, "search-0.sse": {2789, 2001, 2}, "repeat-0.sse": {176, 0, 2}, "repeat-1.sse": {252, 0, 1}, "correlation-0.sse": {260, 0, 2}, "correlation-1.sse": {403, 0, 2}, "correlation-2.sse": {444, 0, 1},
			}[filepath.Base(p)]
			if out.Usage.TotalTokens != expected.total || out.Usage.ToolTokens != expected.tool || len(out.Output) != expected.items {
				t.Fatalf("recorded usage/output changed: %+v", out)
			}
			if filepath.Base(p) == "search-0.sse" && (!strings.Contains(string(out.Output[1].Raw), "url_citation") || out.Output[0].Type != "web_search_call") {
				t.Fatal("recorded search/citations lost")
			}
		})
	}
}

type recordedKey struct{}

func (recordedKey) Key(context.Context) (string, error) { return "synthetic-fixture-key", nil }

func TestIntegrationRecordedCanonicalConversation(t *testing.T) {
	var requests []Request
	var streams [][]byte
	for step := 0; step < 3; step++ {
		prefix := fmt.Sprintf("testdata/recorded/correlation-%d", step)
		body, e := os.ReadFile(prefix + ".request.json")
		if e != nil {
			t.Fatal(e)
		}
		var request Request
		if e = json.Unmarshal(body, &request); e != nil {
			t.Fatal(e)
		}
		requests = append(requests, request)
		stream, e := os.ReadFile(prefix + ".sse")
		if e != nil {
			t.Fatal(e)
		}
		streams = append(streams, stream)
	}
	var posted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(posted.Add(1)) - 1
		if index >= len(streams) {
			t.Error("unexpected retry")
			http.Error(w, "unexpected retry", 500)
			return
		}
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			t.Error(e)
		}
		var got Request
		if e = json.Unmarshal(raw, &got); e != nil {
			t.Error(e)
		}
		expected, _ := json.Marshal(requests[index])
		actual, _ := json.Marshal(got)
		if !equalJSON(expected, actual) {
			t.Errorf("step %d: wire input differs from the accepted live request", index)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(streams[index])
	}))
	defer server.Close()
	client, e := New(Options{Environment: "dev", Endpoint: server.URL + "/v1/responses", FolderID: "folder", Model: "gpt://folder/aliceai-llm-flash", Key: recordedKey{}, Limits: Limits{RequestBytes: 131072, ResponseBytes: 1048576, EventBytes: 65536, ArgumentBytes: 16384, OutputItems: 64}})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	model, e := NewGenerationModel(client)
	if e != nil {
		t.Fatal(e)
	}
	input := requests[0].Input
	ids := map[string]bool{}
	slotB := ""
	for step, recorded := range requests {
		request := generation.ModelRequest{Instructions: recorded.Instructions, Input: input, Tools: recorded.Tools, MaxOutputTokens: recorded.MaxOutputTokens, MaxToolCalls: recorded.MaxToolCalls}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		result, e := model.Stream(ctx, request, func(generation.ModelDelta) error { return nil }, func([]byte) error { return nil })
		cancel()
		if e != nil {
			t.Fatal(step, e)
		}
		for _, item := range result.Output {
			input = append(input, item.Raw)
		}
		for _, item := range result.Output {
			if item.Type != "function_call" {
				continue
			}
			if item.CallID != item.ID || ids[item.CallID] {
				t.Fatal("non-unique canonical correlation", item)
			}
			ids[item.CallID] = true
			var args map[string]string
			if e = json.Unmarshal([]byte(item.Arguments), &args); e != nil {
				t.Fatal(e)
			}
			var encoded string
			if item.Name == "test_repeat" {
				encoded = fmt.Sprintf(`{"marker": %q}`, "marker-"+args["slot"])
				if args["slot"] == "b" {
					slotB = item.ID
				}
			} else {
				if item.Name != "test_present" || args["result_id"] != slotB || args["marker"] != "marker-b" {
					t.Fatal("selected result changed", item)
				}
				encoded = `{"presented": true}`
			}
			output, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": item.CallID, "output": encoded})
			input = append(input, output)
		}
	}
	if posted.Load() != 3 || len(ids) != 4 {
		t.Fatal("incomplete canonical dialogue", posted.Load(), ids)
	}
}
