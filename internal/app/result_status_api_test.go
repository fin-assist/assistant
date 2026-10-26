package app_test

import (
	"fmt"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/assistant/internal/app/apitest"
)

func TestAPIResultStatusKeepsExplicitDomainPermissionAcrossReaders(t *testing.T) {
	for _, reason := range []string{"MODEL_ERROR", "CANNOT_ANSWER", "OFF_TOPIC"} {
		for _, retryable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", reason, retryable), func(t *testing.T) {
				env := apitest.Start(t)
				seedAPIDialogue(t, env, "failed")
				dialogueAPIExec(t, env, `UPDATE answer SET error_code=$3,retryable=$4,charged=false WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, reason, retryable)
				ctx, cancel := apiDialogueContext(t, apiDialogueUser)
				defer cancel()
				client := assistantv1.NewAssistantServiceClient(env.Conn)
				page, err := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
				if err != nil || len(page.GetMessages()) != 2 {
					t.Fatalf("history: %v / %v", page, err)
				}
				history := page.Messages[1]
				decoded, valid := finerr.DecodeResultStatus(history.GetError())
				classification := codes.FailedPrecondition
				if reason == "MODEL_ERROR" {
					classification = codes.Internal
				}
				if !valid || decoded.Reason != reason || decoded.Code != classification || len(history.Error.Details) != 1 || history.Retryable == nil || history.GetRetryable() != retryable {
					t.Fatalf("canonical cause changed independent domain permission: %v", history)
				}
				if page.Messages[0].Error != nil || page.Messages[0].Retryable != nil {
					t.Fatal("user question acquired result error metadata")
				}
				stream, err := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
				if err != nil {
					t.Fatal(err)
				}
				if first, err := stream.Recv(); err != nil || first.GetSnapshot() == nil {
					t.Fatalf("failed snapshot: %v / %v", first, err)
				}
				terminal, err := stream.Recv()
				failure := terminal.GetError()
				if err != nil || failure == nil || !proto.Equal(failure.Status, history.Error) || failure.Retryable != retryable || failure.Code != history.ErrorCode || failure.Text != history.GetErrorText() {
					t.Fatalf("terminal/history dual-write disagree: %v / %v / %v", failure, history, err)
				}
			})
		}
	}
}
