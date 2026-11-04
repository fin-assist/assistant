package app_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	analyticsv1 "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
)

func TestAPIGenerationEmptyPeriodsWithoutTimezone(t *testing.T) {
	const answerText = "Доступных периодов пока нет."
	const periodsTool = "list_periods"
	for _, test := range []struct {
		name     string
		response *analyticsv1.ListPeriodsResponse
		invalid  bool
	}{
		{name: "new user without analytics state", response: &analyticsv1.ListPeriodsResponse{}},
		{name: "empty with source zone", response: &analyticsv1.ListPeriodsResponse{TimeZone: "Europe/Moscow"}},
		{name: "explicit invalid zone", response: &analyticsv1.ListPeriodsResponse{TimeZone: "Local"}, invalid: true},
		{name: "populated without zone", response: &analyticsv1.ListPeriodsResponse{Periods: []*analyticsv1.PeriodItem{{Key: "2026-09", Range: generationAPIRange(), Coverage: commonv1.Coverage_COVERAGE_COMPLETE}}}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := &generationAPIPeers{grant: true, periodsResponse: test.response}
			model := generationAPIModel(t, func(step int, w http.ResponseWriter, _ *http.Request) {
				if step == 1 {
					_, _ = io.WriteString(w, generationAPIFunction(periodsTool, `{}`))
					return
				}
				_, _ = io.WriteString(w, generationAPIText(answerText))
			})
			env := generationAPIEnv(t, peer, model)
			accepted := generationAPIAsk(t, env, generationAPIUser, generationAPIKey(), "Какие периоды доступны?", commonv1.TransferMode_TRANSFER_MODE_WITHOUT)
			if test.invalid {
				presenterAssertFailure(t, env, generationAPIUser, accepted.AnswerId)
				if model.count.Load() != 1 {
					t.Fatal("invalid peer result reached the next model step")
				}
				return
			}
			done, failed := generationAPITerminal(t, env, generationAPIUser, accepted.AnswerId)
			// Period navigation alone does not change the existing charging policy.
			if failed != nil || done == nil || !done.Charged || done.RemainingLimit != 4 {
				t.Fatalf("empty periods terminal: done=%v failed=%v", done, failed)
			}
			saved := presenterHistory(t, env, generationAPIUser)
			if saved.Id != accepted.AnswerId || saved.GetStatus() != assistantv1.AnswerStatus_ANSWER_STATUS_COMPLETE || len(saved.Blocks) != 1 || saved.Blocks[0].GetText().GetText() != answerText || saved.Source != nil {
				t.Fatalf("empty periods history: %v", saved)
			}
			if peer.count("periods") != 1 || model.count.Load() != 2 {
				t.Fatal("period navigation did not complete exactly once")
			}
			model.mu.Lock()
			defer model.mu.Unlock()
			var input []struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(model.requests[1].body["input"], &input); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range input {
				if item.Type != "function_call_output" {
					continue
				}
				var output struct {
					Periods  []json.RawMessage `json:"periods"`
					TimeZone string            `json:"time_zone"`
				}
				if err := json.Unmarshal([]byte(item.Output), &output); err != nil || len(output.Periods) != 0 || output.TimeZone != test.response.TimeZone {
					t.Fatalf("model did not receive exact empty result: %s %v", item.Output, err)
				}
				found = true
			}
			if !found {
				t.Fatal("model continuation omitted period result")
			}
		})
	}
}
