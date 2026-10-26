package aistudio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntegrationRecordedAIStudioResponses(t *testing.T) {
	paths, e := filepath.Glob("testdata/recorded/*.sse")
	if e != nil || len(paths) < 4 {
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
				"text-0.sse": {87, 0, 1}, "functions-0.sse": {209, 0, 2}, "functions-1.sse": {281, 0, 1}, "functions-2.sse": {337, 0, 1}, "search-0.sse": {2789, 2001, 2},
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
