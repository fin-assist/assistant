package app_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	assistantv1 "github.com/fin-assist/contracts/gen/go/finassist/assistant/v1"
	statementv1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/assistant/internal/app/apitest"
	"github.com/fin-assist/assistant/internal/config"
	"github.com/fin-assist/assistant/internal/datarevision"
)

func strictVersionAvailability(t *testing.T) {
	detailed, _ := status.New(codes.Unavailable, "PRIVATE").WithDetails(&errdetails.ErrorInfo{Reason: "TOKEN_REVOKED"})
	for _, tc := range []struct {
		name    string
		failure error
		degrade bool
	}{
		{"unavailable", status.Error(codes.Unavailable, "PRIVATE"), true},
		{"child-deadline", status.Error(codes.DeadlineExceeded, "PRIVATE"), true},
		{"invalid-token", finerr.InvalidToken(), false},
		{"revoked-token", finerr.TokenRevoked(), false},
		{"permission", status.Error(codes.PermissionDenied, "PRIVATE"), false},
		{"detailed-unavailable", detailed.Err(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, target := startDialogueVersionPeer(t)
			peer.read = func(context.Context, *statementv1.GetDataVersionRequest) (*statementv1.GetDataVersionResponse, error) {
				return nil, tc.failure
			}
			env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
			seedAPIDialogue(t, env, "complete")
			setAPIDialogueSource(t, env)
			var original []byte
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT data_revisions FROM answer WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer).Scan(&original); err != nil {
				t.Fatal(err)
			}
			client := assistantv1.NewAssistantServiceClient(env.Conn)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			page, err := client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if !tc.degrade {
				if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "PRIVATE") {
					t.Fatalf("refusal degraded: %v %v", page, err)
				}
			} else {
				if err != nil || !reflect.DeepEqual(page.GetMessages()[1].GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED}) || page.GetMessages()[1].GetBlocks()[0].GetText().GetText() != "saved" {
					t.Fatalf("outage history: %v %v", page, err)
				}
			}
			stream, watchErr := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if watchErr == nil {
				first, recvErr := stream.Recv()
				watchErr = recvErr
				if tc.degrade {
					if recvErr != nil || !reflect.DeepEqual(first.GetSnapshot().GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UNVERIFIED}) {
						t.Fatalf("outage snapshot: %v %v", first, recvErr)
					}
					terminal, recvErr := stream.Recv()
					if recvErr != nil || terminal.GetDone() == nil {
						t.Fatal(terminal, recvErr)
					}
				}
			}
			if !tc.degrade && (status.Code(watchErr) != codes.Internal || strings.Contains(watchErr.Error(), "PRIVATE")) {
				t.Fatalf("refusal snapshot degraded: %v", watchErr)
			}
			peer.mu.Lock()
			peer.read = nil
			peer.revisions = strictAPIWire(1, 1, 1)
			peer.mu.Unlock()
			page, err = client.ListMessages(ctx, &assistantv1.ListMessagesRequest{})
			if err != nil || len(page.GetMessages()[1].GetSource().GetChangedSince()) != 0 {
				t.Fatalf("outage overwrote baseline: %v %v", page, err)
			}
			var after []byte
			var savedStatus string
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT data_revisions,status FROM answer WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer).Scan(&after, &savedStatus); err != nil || string(after) != string(original) || savedStatus != "complete" {
				t.Fatalf("saved facts changed: %s %s %v", after, savedStatus, err)
			}
		})
	}
}
func TestAPISourceVersionOutageAndRefusal(t *testing.T)         { strictVersionAvailability(t) }
func TestIntegrationSourceVersionOutageAndRefusal(t *testing.T) { strictVersionAvailability(t) }

// A source first published after a data change, and an unchanged source whose
// revision changes during generation, both need the latest primary comparison.
func strictLiveSourceComparison(t *testing.T) {
	for _, initialSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-source", true: "same-source-refresh"}[initialSource], func(t *testing.T) {
			peer, target := startDialogueVersionPeer(t)
			peer.revisions = strictAPIWire(5, 5, 5)
			env := apitest.Start(t, func(c *config.Config) { c.Peers["bank-statement-data"] = target })
			seedAPIDialogue(t, env, "generating")
			base, _ := json.Marshal(datarevision.Baseline{Values: map[string][]int64{"categories": {5}, "uploads": {5}, "time_zone": {5}}})
			if initialSource {
				setAPIDialogueSource(t, env)
				dialogueAPIExec(t, env, `UPDATE answer SET data_revisions=$3 WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, base)
			}
			client := assistantv1.NewAssistantServiceClient(env.Conn)
			ctx, cancel := apiDialogueContext(t, apiDialogueUser)
			defer cancel()
			stream, err := client.WatchAnswer(ctx, &assistantv1.WatchAnswerRequest{AnswerId: apiDialogueAnswer})
			if err != nil {
				t.Fatal(err)
			}
			first, err := stream.Recv()
			if err != nil || first.GetSnapshot() == nil || len(first.GetSnapshot().GetSource().GetChangedSince()) != 0 {
				t.Fatal(first, err)
			}
			peer.mu.Lock()
			peer.revisions = strictAPIWire(5, 6, 5)
			peer.mu.Unlock()
			dialogueAPIExec(t, env, `UPDATE answer SET status='complete',calculated_at='2026-10-23T12:00:00Z',data_from='2026-10-01T00:00:00Z',data_to='2026-11-01T00:00:00Z',data_revisions=$3 WHERE user_id=$1 AND id=$2`, apiDialogueUser, apiDialogueAnswer, base)
			dialogueAPIExec(t, env, `SELECT pg_notify('assistant_answers',$1)`, apiDialogueAnswer)
			next, err := stream.Recv()
			if err != nil || !reflect.DeepEqual(next.GetSource().GetChangedSince(), []assistantv1.ChangedSince{assistantv1.ChangedSince_CHANGED_SINCE_UPLOADS}) {
				t.Fatalf("live source did not compare current: %v %v", next, err)
			}
			done, err := stream.Recv()
			if err != nil || done.GetDone() == nil {
				t.Fatal(done, err)
			}
		})
	}
}
func TestAPISourceLivePrimaryComparison(t *testing.T)         { strictLiveSourceComparison(t) }
func TestIntegrationSourceLivePrimaryComparison(t *testing.T) { strictLiveSourceComparison(t) }
