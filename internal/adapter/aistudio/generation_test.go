package aistudio

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fin-assist/assistant/internal/generation"
)

func TestGenerationModelMapsTransportAndRefusal(t *testing.T) {
	if _, err := NewGenerationModel(nil); err == nil {
		t.Fatal("nil transport accepted")
	}
	for _, scenario := range []string{"success", "refusal", "callback", "guard"} {
		t.Run(scenario, func(t *testing.T) {
			client, err := New(httpTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			calls := 0
			client.http.Transport = httpTestRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				stream := httpTestSSE("Ответ")
				if scenario == "refusal" {
					stream = httpTestFrames(map[string]any{"type": "response.created", "response": map[string]any{"id": "r", "status": "in_progress", "output": []any{}}}, map[string]any{"type": "response.refusal.done", "refusal": "provider private"})
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})
			model, err := NewGenerationModel(client)
			if err != nil {
				t.Fatal(err)
			}
			request := httpTestRequest()
			guardError := errors.New("budget refused")
			callbackError := errors.New("sink failed")
			guards := 0
			var text string
			r, err := model.Stream(httpTestContext(t), generation.ModelRequest{Instructions: request.Instructions, Input: request.Input, Tools: request.Tools, MaxOutputTokens: request.MaxOutputTokens, MaxToolCalls: request.MaxToolCalls}, func(delta generation.ModelDelta) error {
				if scenario == "callback" {
					return callbackError
				}
				text += delta.Text
				return nil
			}, func(raw []byte) error {
				guards++
				if scenario == "guard" {
					return guardError
				}
				if !strings.Contains(string(raw), `"store":false`) {
					t.Error("incomplete guarded wire body")
				}
				return nil
			})
			switch scenario {
			case "success":
				if err != nil || text != "Ответ" || len(r.Output) != 1 || r.Output[0].Type != "message" || !strings.Contains(string(r.Output[0].Raw), "output_text") || r.Usage.TotalTokens != 2 {
					t.Fatalf("result %+v err %v text %s", r, err, text)
				}
			case "refusal":
				if !errors.Is(err, generation.ErrRefused) {
					t.Fatal(err)
				}
			case "callback":
				if !errors.Is(err, callbackError) {
					t.Fatal(err)
				}
			case "guard":
				if !errors.Is(err, guardError) || calls != 0 {
					t.Fatalf("%v calls=%d", err, calls)
				}
			}
			if guards != 1 {
				t.Fatalf("guards %d", guards)
			}
		})
	}
	client, err := New(httpTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	model, _ := NewGenerationModel(client)
	if _, err := model.Stream(context.Background(), generation.ModelRequest{}, nil, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
}
