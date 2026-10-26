package dialogue

import (
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	domain "github.com/fin-assist/assistant/internal/dialogue"
)

func TestUnitResultStatusDualWritePreservesStoredPermission(t *testing.T) {
	for _, reason := range []string{"MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC"} {
		for _, policy := range []*bool{nil, proto.Bool(false), proto.Bool(true)} {
			name := reason + "/legacy"
			if policy != nil {
				name = reason + "/false"
				if *policy {
					name = reason + "/true"
				}
			}
			t.Run(name, func(t *testing.T) {
				answer := domain.Answer{Status: "failed", ErrorCode: reason, Retryable: policy}
				terminal, err := Error(answer, 4)
				if err != nil {
					t.Fatal(err)
				}
				decoded, ok := finerr.DecodeResultStatus(terminal.Status)
				wantCode := codes.FailedPrecondition
				if reason == "MODEL_ERROR" {
					wantCode = codes.Internal
				}
				if !ok || decoded.Reason != reason || decoded.Code != wantCode || len(decoded.Metadata) != 0 || len(terminal.Status.Details) != 1 || terminal.Status.Message != reason {
					t.Fatalf("canonical failure contains unexpected cause/details: %v", terminal.Status)
				}
				wantRetry := reason == "MODEL_ERROR"
				if policy != nil {
					wantRetry = *policy
				}
				if terminal.Retryable != wantRetry || terminal.RemainingLimit != 4 || terminal.Text == "" {
					t.Fatalf("legacy result policy changed: %v", terminal)
				}
				history, err := Message(domain.Message{ID: "saved", CreatedAt: time.Now(), Answer: &answer})
				if err != nil {
					t.Fatal(err)
				}
				if !proto.Equal(history.Error, terminal.Status) || history.ErrorCode != terminal.Code || history.GetErrorText() != terminal.Text || history.Retryable == nil || *history.Retryable != wantRetry {
					t.Fatalf("terminal/history disagree: %v / %v", terminal, history)
				}
			})
		}
	}
}

func TestUnitNonFailedMessagesOmitResultError(t *testing.T) {
	for _, state := range []string{"user", "generating", "complete"} {
		t.Run(state, func(t *testing.T) {
			message := domain.Message{ID: "saved", CreatedAt: time.Now()}
			if state != "user" {
				message.Answer = &domain.Answer{Status: state}
			}
			out, err := Message(message)
			if err != nil || out.GetError() != nil || out.Retryable != nil || out.ErrorText != nil {
				t.Fatalf("nonfailed result acquired failure policy: %v / %v", out, err)
			}
		})
	}
}
