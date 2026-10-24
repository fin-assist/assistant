package generation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/assistant/internal/dialogue"
	"github.com/fin-assist/assistant/internal/domain"
)

func TestUnitPrepareFinishSelectsEffectsBeforeStorage(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		code      string
		charged   bool
		action    domain.OffTopicLedgerAction
		retryable bool
	}{{"", true, domain.ResetOffTopic, false}, {"", false, domain.KeepOffTopic, false}, {"OFF_TOPIC", true, domain.RecordOffTopic, false}, {"MODEL_ERROR", true, domain.KeepOffTopic, true}, {"CANNOT_ANSWER", true, domain.KeepOffTopic, false}, {"FUTURE_ERROR", true, domain.KeepOffTopic, false}} {
		t.Run(fmt.Sprintf("%s/charged=%t", tc.code, tc.charged), func(t *testing.T) {
			out := Outcome{ErrorCode: tc.code, Charged: tc.charged, Blocks: []dialogue.Block{{Type: "text", Version: 1, Text: "partial"}}, Chips: []json.RawMessage{json.RawMessage(`{"label":"saved"}`)}, Source: &dialogue.Source{CalculatedAt: at}}
			terminal := PrepareFinish(out, at)
			if !terminal.At.Equal(at) || terminal.Ledger.Action != tc.action || terminal.Outcome.ErrorCode != tc.code {
				t.Fatalf("wrong terminal: %+v", terminal)
			}
			if tc.code != "" {
				if terminal.Status != "failed" || terminal.Retryable == nil || *terminal.Retryable != tc.retryable || terminal.Outcome.Charged || terminal.PreserveBlockPrefix || !terminal.RefundCurrentDay || len(terminal.Outcome.Blocks) != 0 || len(terminal.Outcome.Chips) != 0 || terminal.Outcome.Source != nil {
					t.Fatalf("failure retained content/charge: %+v", terminal)
				}
			} else if terminal.Status != "complete" || terminal.Retryable != nil || !terminal.PreserveBlockPrefix || terminal.RefundCurrentDay == tc.charged || !reflect.DeepEqual(terminal.Outcome, out) {
				t.Fatalf("success changed: %+v", terminal)
			}
			if len(out.Blocks) != 1 || len(out.Chips) != 1 || out.Source == nil || out.Charged != tc.charged {
				t.Fatal("preparation mutated the source outcome")
			}
		})
	}
}
