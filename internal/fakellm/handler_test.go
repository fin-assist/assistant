package fakellm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestBody(question string) map[string]any {
	return map[string]any{"model": "gpt://folder/aliceai-llm", "input": []any{map[string]any{"role": "user", "content": question}}, "stream": true, "store": false, "max_tool_calls": 3}
}
func serve(t *testing.T, h *Handler, body any, header string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
	r.Header.Set("x-data-logging-enabled", header)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func defaultHandler(t *testing.T) *Handler {
	t.Helper()
	h, e := Default()
	if e != nil {
		t.Fatal(e)
	}
	return h
}
func TestUnitRequestPrivacyAndProtocol(t *testing.T) {
	h := defaultHandler(t)
	for _, tc := range []string{"header", "store", "stream", "budget", "input", "structured", "trailing", "oversized", "model"} {
		t.Run(tc, func(t *testing.T) {
			b := requestBody("unknown")
			header := "false"
			switch tc {
			case "header":
				header = ""
			case "store":
				b["store"] = true
			case "stream":
				b["stream"] = false
			case "budget":
				b["max_tool_calls"] = -1
			case "input":
				b["input"] = []any{}
			case "model":
				b["model"] = ""
			case "structured":
				b["tools"] = []any{map[string]any{"type": "web_search"}}
				b["text"] = map[string]any{"format": map[string]any{"type": "json_schema"}}
			case "oversized":
				b["instructions"] = strings.Repeat("x", 131073)
			}
			var w *httptest.ResponseRecorder
			if tc == "trailing" {
				r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{} {}`))
				r.Header.Set("x-data-logging-enabled", "false")
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
			} else {
				w = serve(t, h, b, header)
			}
			if w.Code != 400 || strings.Contains(w.Body.String(), "unknown") {
				t.Fatalf("status/body: %d %s", w.Code, w.Body)
			}
		})
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"GET", "/v1/responses", 405}, {"POST", "/else", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
}
func TestUnitDefaultAndTemplateContinuation(t *testing.T) {
	h := defaultHandler(t)
	w := serve(t, h, requestBody("unknown"), "false")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal(w.Code, w.Body)
	}
	b := requestBody("rows")
	b["tools"] = []any{map[string]any{"type": "function", "name": "get_data_overview", "parameters": map[string]any{"type": "object"}}, map[string]any{"type": "function", "name": "aggregate_operations", "parameters": map[string]any{"type": "object"}}}
	w = serve(t, h, b, "false")
	if w.Code != 200 || strings.Count(w.Body.String(), `"type":"function_call"`) != 6 {
		t.Fatal(w.Code, w.Body)
	}
	b["input"] = []any{map[string]any{"role": "user", "content": "rows"}, map[string]any{"id": "fake_rows_1_0", "type": "function_call"}, map[string]any{"type": "function_call_output", "call_id": "overview", "output": "{}"}, map[string]any{"type": "function_call_output", "call_id": "totals", "output": `{"amount":9007199254740993}`}, map[string]any{"type": "function_call_output", "call_id": "show", "output": "{}"}}
	w = serve(t, h, b, "false")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "9007199254740993") {
		t.Fatal(w.Code, w.Body)
	}
	// Another user question starts at step zero even when the history contains fake IDs.
	b["input"] = append(b["input"].([]any), map[string]any{"role": "user", "content": "unknown"})
	w = serve(t, h, b, "false")
	if w.Code != 200 || strings.Contains(w.Body.String(), "9007199254740993") {
		t.Fatal(w.Code, w.Body)
	}
}
func TestUnitScenarioValidation(t *testing.T) {
	valid := `[{"id":"fallback","default":true,"steps":[{"output":[{"type":"message","text":["ok"]}]}]}]`
	if _, e := Load(strings.NewReader(valid)); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{`[]`, `{}`, valid + ` {}`, strings.Replace(valid, `"default":true`, `"pattern":"["`, 1), strings.Replace(valid, `"message"`, `"invalid"`, 1), strings.Replace(valid, `"ok"`, `"{{"`, 1), strings.Replace(valid, `"steps":[`, `"unknown":true,"steps":[`, 1), strings.Replace(valid, `"output":[`, `"delay_ms":-1,"output":[`, 1), strings.Replace(valid, `"output":[`, `"fault":"other","output":[`, 1)} {
		if _, e := Load(strings.NewReader(raw)); e == nil {
			t.Fatalf("accepted: %s", raw)
		}
	}
}
func TestUnitSearchCapAndInstructionAssertions(t *testing.T) {
	raw := `[{"id":"fallback","default":true,"steps":[{"output":[{"type":"web_search_call"},{"type":"web_search_call"}],"usage":{"tool_tokens":8}}]}]`
	h, e := Load(strings.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	for _, n := range []int{0, 1, 2} {
		b := requestBody("unknown")
		b["max_tool_calls"] = n
		b["tools"] = []any{map[string]any{"type": "web_search"}}
		w := serve(t, h, b, "false")
		if w.Code != 200 || strings.Count(w.Body.String(), `"type":"web_search_call"`) != n*3 {
			t.Fatal(n, w.Code, w.Body)
		}
	}
	b := requestBody("search")
	w := serve(t, defaultHandler(t), b, "false")
	if w.Code != 400 {
		t.Fatal("missing prompt assertion accepted")
	}
}
func TestUnitCancellationAndWriterFailure(t *testing.T) {
	h := defaultHandler(t)
	raw, _ := json.Marshal(requestBody("timeout"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw)).WithContext(ctx)
	r.Header.Set("x-data-logging-enabled", "false")
	h.ServeHTTP(httptest.NewRecorder(), r)
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if pause(ctx, 100) {
		t.Fatal("cancelled delay succeeded")
	}
	if !pause(t.Context(), 1) {
		t.Fatal("timer failed")
	}
	if _, e := render(`{{.Results.missing.value}}`, templateData{Results: map[string]any{}}); e == nil {
		t.Fatal("missing result accepted")
	}
	if _, e := render(strings.Repeat("x", 65537), templateData{}); e == nil {
		t.Fatal("oversized template accepted")
	}
}

func TestUnitInvalidScenarioDefinitions(t *testing.T) {
	for _, raw := range []string{
		`[{"id":"bad!","default":true,"steps":[{}]}]`,
		`[{"id":"a","default":true,"steps":[]}]`,
		`[{"id":"a","default":true,"pattern":"x","steps":[{}]}]`,
		`[{"id":"a","steps":[{}]}]`,
		`[{"id":"a","default":true,"steps":[{}]},{"id":"a","default":true,"steps":[{}]}]`,
		`[{"id":"a","default":true,"steps":[{"usage":{"tool_tokens":-1}}]}]`,
		`[{"id":"a","default":true,"steps":[{"http_status":200}]}]`,
		`[{"id":"a","default":true,"steps":[{"max_tool_calls":-1}]}]`,
		`[{"id":"a","default":true,"steps":[{"output":[{"type":"message"}]}]}]`,
		`[{"id":"a","default":true,"steps":[{"output":[{"type":"function_call","name":"read","call_id":"c","arguments":[]}]}]}]`,
	} {
		if _, e := Load(strings.NewReader(raw)); e == nil {
			t.Fatal("accepted invalid suite", raw)
		}
	}
	if _, e := Load(strings.NewReader(strings.Repeat(" ", 1<<20) + "[]")); e == nil {
		t.Fatal("oversized suite accepted")
	}
}
func TestUnitContinuationRequiresRealResults(t *testing.T) {
	h := defaultHandler(t)
	for _, continuation := range []string{
		`{"id":"fake_rows_0_0","type":"function_call"}`,
		`{"id":"fake_rows_99_0"}`,
		`{"id":"fake_rows_bad_0"}`,
		`{"type":"function_call_output","call_id":"c","output":"invalid"}`,
	} {
		b := requestBody("rows")
		b["tools"] = modelRequest("rows").Tools
		b["input"] = []any{map[string]any{"role": "user", "content": "rows"}, json.RawMessage(continuation)}
		if w := serve(t, h, b, "false"); w.Code != 400 {
			t.Fatal("invalid continuation accepted", continuation)
		}
	}
	b := requestBody("rows")
	if w := serve(t, h, b, "false"); w.Code != 400 {
		t.Fatal("unadvertised function accepted")
	}
}
