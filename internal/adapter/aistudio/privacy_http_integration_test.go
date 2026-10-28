package aistudio

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fin-assist/assistant/internal/generation"
)

func TestIntegrationPrivacyPANWindowsAndKnownValuesReachOnlySafeHTTPFields(t *testing.T) {
	for _, tc := range []struct{ question, value string }{
		{"5000 4111 1111 1111 1111", "4111 1111 1111 1111"},
		{"4111 1111 1111 1111 100 рублей", "4111 1111 1111 1111"},
		{"4111 1111 1111 1111 1 октября", "4111 1111 1111 1111"},
		{"карта 4111111111111111", "4111111111111111"},
		{"телефон: 79991234567", "79991234567"},
		{"счёт 40817810099910004312", "40817810099910004312"},
	} {
		t.Run(tc.question, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body Request
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				var question, output map[string]any
				_ = json.Unmarshal(body.Input[0], &question)
				if strings.Contains(question["content"].(string), tc.value) {
					t.Error("PAN/requisite escaped in actual question HTTP")
				}
				_ = json.Unmarshal(body.Input[1], &output)
				var result map[string]any
				_ = json.Unmarshal([]byte(output["output"].(string)), &result)
				if result["q"] == tc.value || result["amount_minor"] != digits(tc.value) || result["total_amount_minor"] != digits(tc.value) {
					t.Error("known q leak or quoted-money corruption in HTTP")
				}
				if strings.Contains(tc.question, "100 рублей") && !strings.Contains(question["content"].(string), "100 рублей") {
					t.Error("adjacent amount lost")
				}
				if strings.HasPrefix(tc.question, "5000 ") && !strings.HasPrefix(question["content"].(string), "5000 ") {
					t.Error("preceding amount lost")
				}
				if strings.Contains(tc.question, "1 октября") && !strings.Contains(question["content"].(string), "1 октября") {
					t.Error("adjacent date lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, httpTestSSE("ordinary response"))
			}))
			defer srv.Close()
			client := httpTestClient(t, srv, nil)
			question, _ := json.Marshal(map[string]any{"role": "user", "content": tc.question})
			raw, _ := json.Marshal(map[string]any{"q": tc.value, "amount_minor": digits(tc.value), "total_amount_minor": digits(tc.value)})
			output, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": "f", "output": string(raw)})
			req := httpTestRequest()
			req.Input = []json.RawMessage{question, output}
			if _, err := client.Stream(httpTestContext(t), req, func(Delta) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func privacyFragmentedSSE(text string, fragments ...string) string {
	var frames []map[string]any
	for _, frame := range strings.Split(strings.TrimSpace(httpTestSSE(text)), "\n\n") {
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event) != nil {
			panic("invalid synthetic SSE")
		}
		if event["type"] != "response.output_text.delta" {
			frames = append(frames, event)
			continue
		}
		for _, fragment := range fragments {
			frames = append(frames, map[string]any{"type": "response.output_text.delta", "item_id": "m", "output_index": 0, "content_index": 0, "delta": fragment})
		}
	}
	return httpTestFrames(frames...)
}
func TestIntegrationPrivacyRestoredStreamBecomesSafeFollowupHistory(t *testing.T) {
	const phone = "+7 (999) 123-45-67"
	const unknown = "[phone_000000000000000000000000]"
	store := &sizeEngineStore{}
	var firstToken string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body Request
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(body.Input)
		tokens := aliasPattern.FindAllString(string(raw), -1)
		if len(tokens) == 0 || strings.Contains(string(raw), phone) {
			t.Error("unsafe or missing phone alias")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		calls++
		if calls == 1 {
			firstToken = tokens[0]
			answer := "По " + firstToken + " 100 рублей; " + unknown
			_, _ = io.WriteString(w, privacyFragmentedSSE(answer, "По "+firstToken[:4], firstToken[4:15], firstToken[15:]+" 100 рублей; "+unknown))
			return
		}
		count := 0
		for _, token := range tokens {
			if token != unknown {
				if token == firstToken {
					t.Error("old generation alias reached history")
				}
				count++
				if token != tokens[0] {
					t.Error("question/answer link lost")
				}
			}
		}
		if count != 2 {
			t.Error("restored history did not contain both linked values")
		}
		_, _ = io.WriteString(w, httpTestSSE("Уточнение"))
	}))
	defer srv.Close()
	client := httpTestClient(t, srv, nil)
	model, _ := NewGenerationModel(client)
	engine := sizeEngine(t, store, model, &sizeData{}, sizeLimits())
	task := sizeTask()
	task.Text = "Расходы по " + phone
	if err := engine.Run(httpTestContext(t), task); err != nil {
		t.Fatal(err)
	}
	expected := "По " + phone + " 100 рублей; " + unknown
	if store.appended != expected || store.outcome.ErrorCode != "" || strings.Contains(store.appended, firstToken) {
		t.Fatal("local stream/final mismatch", store.outcome)
	}
	store.history = []generation.HistoryPair{{Question: task.Text, Answer: store.appended}}
	store.appended = ""
	task.AnswerID = "next-answer"
	task.Text = "Уточни ответ"
	if err := engine.Run(httpTestContext(t), task); err != nil || store.outcome.ErrorCode != "" || calls != 2 {
		t.Fatal("followup failed", err, store.outcome)
	}
}
